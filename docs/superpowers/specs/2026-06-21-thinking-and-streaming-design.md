# 显示思考过程 + 支持 SSE 流式（统一 sink）

日期：2026-06-21

## 背景与动机

- 用 OpenAI 兼容后端（实测 Ollama + `qwen3:8b`）时，模型其实返回了思考，但终端看不到。curl 实测响应 `message` 同时含 `content` 和 `reasoning`，而 `fromOpenAIChoice` 只读 `content`，思考被丢弃。（字段名因后端而异：Ollama / 部分网关用 `reasoning`，DeepSeek-R1 / vLLM 用 `reasoning_content`。）
- 用户还要求支持**配置成 SSE 流式**。流式正是让思考逐字实时显示的方式，与「显示思考」深度纠缠，故两者一起设计、一起实现。

## 核心设计：非流式 = 只有一个 chunk 的流式

显示统一走一个 **sink**。流式与否对核心循环和 UI **完全透明**，判断封死在 `OpenAIProvider` 内部，由 config 决定：

- 流式 Provider：边读 SSE 边多次 `sink.OnReasoning/OnContent`。
- 非流式 Provider：拿到整段后，把整段当作「一次 delta」调一次 sink。

三条干净边界：
- **协议细节留在 provider**：SSE 解析、`[DONE]`、工具调用分片按 index 累积。返回的永远是组装好的完整 `*Response`。
- **显示细节留在 UI**：sink 只管「来了增量就显示」，不关心是 1 段还是 100 段。
- **sink 是两者之间唯一的细线**。

## 改动清单（自底向上）

### llm 层
- `types.go`：`Message` 加 `Reasoning string`；新增中立接口
  ```go
  type StreamSink interface {
      OnReasoning(delta string) // 思考增量
      OnContent(delta string)   // 正文增量
  }
  ```
  约定：实现者只会收到**非空**增量，Provider 不转发空串。
- `provider.go`：`Chat` 签名加 sink 参数 → `Chat(ctx, msgs, tools, sink StreamSink) (*Response, error)`。
- `openai.go`：
  - `OpenAIProvider` 加 `stream bool`；`NewOpenAIProvider` 加 `stream` 入参。
  - 请求 DTO 加 `Stream bool json:"stream,omitempty"`。
  - 响应 DTO `openAIMessage` 加 `reasoning` / `reasoning_content`（都 omitempty）；`fromOpenAIChoice` 取非空者填入 `Message.Reasoning`。
  - `Chat`：发请求 → 非 200 读 body 报错 → 按 `p.stream` 分派 `parseOnce` 或 `parseSSE`。
  - `parseOnce(r, sink)`：保留原解析；末尾把整段 `emit(sink, reasoning, content)`（仅非空）。
  - `parseSSE(r, sink)`：扫 `data:` 行，`[DONE]` 结束；累积 content / reasoning / 按 index 累积 tool_calls；逐 delta 调 sink；组装并返回 `*Response`。是独立可测函数。

### agent 层
- `ui.go`：`UI` 接口把 `AssistantText` 换成 `Sink() OutputSink`；保留 `ConfirmTool` / `ToolOutput`。
  ```go
  type OutputSink interface {
      llm.StreamSink // 给 Provider 用（窄）
      Close()        // 给核心循环收尾用（补换行 / 重置样式）
  }
  ```
  拆两层：Provider 只看见窄的 `llm.StreamSink`，不碰显示生命周期。
- `terminal.go`：删 `AssistantText` / `AssistantThinking`，改为 `Sink()` 返回有状态的 `terminalSink`：
  - 首个思考增量 → 打暗灰 `思考 ` 标签，暗灰保持；
  - 思考转正文 → 关暗灰 + 换行，打绿色加粗 `助手 ` 标签；
  - `Close()` → 收尾补换行 / 重置；本回合无任何文字则什么都不打。
  - `ConfirmTool` / `ToolOutput` 不变（上次改的配色保留）。
- `agent.go`：循环里 `sink := ui.Sink()` → `Chat(..., sink)` → `sink.Close()`；删掉原来直接 `ui.AssistantText` 的分支（显示已在 Chat 内经 sink 完成）。

### config / main / 文档
- `config.go`：加 `Stream bool json:"stream,omitempty"`（默认 false）。
- `main.go`：`NewOpenAIProvider(..., cfg.Stream, ...)`。
- `config.example.json` + `README.md`：补 `stream` 字段说明。

### 测试
- `openai_test.go`：补 `fromOpenAIChoice` 对 `reasoning` / `reasoning_content` 的解析；新增 `parseSSE` 的单测（思考+正文增量、工具调用分片累积）。
- `terminal_test.go`：删 AssistantText 测试，新增 `terminalSink` 渲染测试（思考→正文 / 仅正文 / 空）；保留 ConfirmTool / ToolOutput 测试。
- `integration_test.go`：`fakeUI` 改为实现 `Sink()`；端到端断言改用 sink 收集的文本；并在假响应里加 `reasoning` 验证贯通。

## 取舍

- **两种字段名都兼容**，以后换 DeepSeek/vLLM 不用再改。
- **stream 默认 false**：保守、向后兼容；用户按需开。
- **逐字流式**仅在显示层；历史仍存完整 `*Response`。思考**不回传**给模型（请求 DTO 不带 reasoning，省 token、合规）。
- SSE 容错：坏行跳过、放大行缓冲、`sc.Err()` 兜底。

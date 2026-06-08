# OpenAI 兼容 Agent 框架设计

- 日期：2026-06-08
- 状态：已批准，待实现
- 模块：`zsh-agent`（Go 1.24，纯标准库，无第三方依赖）

## 1. 背景与现状

当前 `main.go` 是一个扁平的交互式 agent：调用 **Anthropic Messages API**（`/v1/messages`、`x-api-key`、content blocks、`tool_use`），在本地执行 zsh 命令。

需求是把它的 LLM 后端换成 **OpenAI 兼容的 `/v1/chat/completions`** 接口（例如本机 Ollama 跑的 `qwen3:8b`），并借这次改动把代码重构成一套**扩展性强、便于迭代、又适合新人学习 agent** 的框架。

目标端点形如：

```
curl -k https://203.0.113.10:443/v1/chat/completions \
  -H "Authorization: Bearer sk-xxx" \
  -H "Content-Type: application/json" \
  -d '{"model":"qwen3:8b","messages":[{"role":"user","content":"你好"}]}'
```

注意 agent 是这条请求的**客户端**（不是服务端）。

## 2. 目标 / 非目标

**目标**
- LLM 后端从 Anthropic 协议切换为 OpenAI Chat Completions 协议。
- 接口地址、API key、模型名等从项目内的 JSON 配置文件读取。
- 纯 IP 端点（如 `203.0.113.10:443`）自动跳过 TLS 证书校验（等价于 `curl -k`）；域名端点走正常 HTTPS 校验。
- 重构为分层框架：核心循环一眼读懂，"会变的部分"用接口隔离，便于扩展。
- 保留 bash 工具与执行前的人工确认。

**非目标（YAGNI，刻意不做）**
- 不做插件系统、反射、中间件链、事件总线。
- 不保留 Anthropic 协议（可未来通过新增 Provider 实现，不在本次范围）。
- 不做 HTTP 服务端模式（UI 接口为未来留口，本次只实现终端）。

## 3. 设计哲学

Agent 的本质：**循环「问模型 → 模型要调工具就执行 → 把结果喂回去」，直到模型不再要工具**。

框架标准：**核心循环要一眼读懂（给新人），"会变的东西"要能轻松替换（给迭代）**。会变的东西就三类，各用一个接口挡住：

| 会变的东西 | 隔离接口 | 扩展方式 |
|---|---|---|
| LLM 后端 | `llm.Provider` | 新写一个文件实现接口 |
| 工具 | `tools.Tool` + `Registry` | 写个工具文件 + 注册一行 |
| 交互方式 | `agent.UI` | 换个 UI 实现 |

核心循环本身永不改动——这就是扩展性的来源。

## 4. 文件结构

```
zsh-agent/
├── main.go                 # 组装：读配置 → 建 provider → 注册工具 → 跑 agent
├── config.example.json     # 配置示例（提交）
├── config.json             # 真实配置（gitignore）
├── config/
│   └── config.go           # 配置结构 + JSON 加载 + 校验/默认值
├── llm/
│   ├── types.go            # 中立类型：Message / ToolCall / ToolSpec / Response
│   ├── provider.go         # Provider 接口
│   └── openai.go           # OpenAI 兼容实现 + TLS（纯 IP 自动免校验）
├── tools/
│   ├── tool.go             # Tool 接口 + Registry
│   └── bash.go             # bash 工具（zsh -c 执行）
└── agent/
    ├── ui.go               # UI 接口
    ├── terminal.go         # 终端 UI 实现（显示 / y-n 确认）
    └── agent.go            # 核心循环（think → act → observe）
```

## 5. 契约（接口与中立类型）—— 由主 agent 亲自编写

### 5.1 `llm/types.go`

```go
package llm

import "encoding/json"

const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Message 是一条 provider 中立的对话消息。
type Message struct {
	Role       string     // system / user / assistant / tool
	Content    string     // 文本内容（assistant 请求工具时可能为空）
	ToolCalls  []ToolCall // 仅 assistant：模型请求调用的工具
	ToolCallID string     // 仅 role=tool：对应哪个 ToolCall.ID
	IsError    bool       // 仅 role=tool：该结果是否为错误（供 UI 展示）
}

// ToolCall 是模型发起的一次工具调用请求。
type ToolCall struct {
	ID   string          // 本次调用唯一 id（回填结果时用）
	Name string          // 工具名
	Args json.RawMessage // 参数 JSON，符合 ToolSpec.Parameters
}

// ToolSpec 是暴露给模型的工具声明。
type ToolSpec struct {
	Name        string
	Description string
	Parameters  json.RawMessage // JSON Schema
}

// Response 是 provider 归一化后的"模型下一步"。
type Response struct {
	Message    Message // assistant 回复（可能含 ToolCalls）
	StopReason string  // "stop" / "tool_calls"（provider 已归一化）
}

// ToolResult 构造一条 role=tool 消息，方便循环回填工具结果。
func ToolResult(toolCallID, content string, isError bool) Message {
	return Message{Role: RoleTool, ToolCallID: toolCallID, Content: content, IsError: isError}
}
```

### 5.2 `llm/provider.go`

```go
package llm

import "context"

// Provider 是 LLM 后端的统一抽象。换后端 = 换一个实现，循环不动。
type Provider interface {
	// Chat 发送整段对话历史 + 可用工具，返回模型的下一步回复。
	Chat(ctx context.Context, msgs []Message, tools []ToolSpec) (*Response, error)
}
```

### 5.3 `tools/tool.go`

```go
package tools

import (
	"context"
	"encoding/json"

	"zsh-agent/llm"
)

// Tool 是一个可被模型调用的工具。加工具 = 实现本接口 + 注册。
type Tool interface {
	Spec() llm.ToolSpec                                            // 给模型看的声明
	Run(ctx context.Context, args json.RawMessage) (string, error) // 真正执行
}

// Registry 管理已注册的工具。
type Registry struct {
	tools map[string]Tool
}

func NewRegistry() *Registry            { return &Registry{tools: map[string]Tool{}} }
func (r *Registry) Register(t Tool)     { r.tools[t.Spec().Name] = t }
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}
func (r *Registry) Specs() []llm.ToolSpec {
	specs := make([]llm.ToolSpec, 0, len(r.tools))
	for _, t := range r.tools {
		specs = append(specs, t.Spec())
	}
	return specs
}
```

### 5.4 `agent/ui.go`

```go
package agent

// UI 把"与人交互"从核心循环里隔离出来。换终端/HTTP/日志 = 换实现。
type UI interface {
	AssistantText(s string)                // 显示模型的文字回复
	ConfirmTool(name, preview string) bool // 工具执行前确认（y/n）
	ToolOutput(s string)                   // 显示工具输出
}
```

## 6. 叶子实现（由 subagent 实现，主 agent review）

### 6.1 `config/config.go`

```go
type Config struct {
	BaseURL            string `json:"base_url"`             // 如 https://203.0.113.10:443/v1
	APIKey             string `json:"api_key"`              // Bearer token
	Model              string `json:"model"`                // 如 qwen3:8b
	MaxTokens          int    `json:"max_tokens,omitempty"` // 默认 4096
	SystemPrompt       string `json:"system_prompt,omitempty"`
	InsecureSkipVerify *bool  `json:"insecure_skip_verify,omitempty"` // nil=按 IP 自动判断
}
```

- `Load(path string) (*Config, error)`：读文件 → `json.Unmarshal` → 校验必填（`base_url` / `api_key` / `model` 缺失或为空要报清晰的错）→ 应用默认（`MaxTokens==0` 时设 4096）。
- 配置路径：默认 `config.json`，允许用环境变量 `AGENT_CONFIG` 覆盖（在 main 里处理）。
- 同时产出 `config.example.json`，并把 `config.json` 加入 `.gitignore`。

### 6.2 `llm/openai.go` —— 最核心的叶子

`OpenAIProvider` 实现 `llm.Provider`，负责"中立类型 ↔ OpenAI JSON"双向翻译 + HTTP + TLS。

**TLS 规则（纯 IP 自动免校验）**
- 解析 `BaseURL` 的主机名，`net.ParseIP(host) != nil` → 纯 IP → `InsecureSkipVerify: true`。
- 否则（域名）→ 正常校验。
- 若 `Config.InsecureSkipVerify != nil`，以它为准（手动覆盖自动判断）。
- 抽成可单测的纯函数 `shouldSkipVerify(baseURL string, override *bool) bool`。

**请求**：`POST {BaseURL}/chat/completions`，头 `Authorization: Bearer {APIKey}` + `Content-Type: application/json`。

**翻译要点（多轮正确性的关键）**
- 中立 `Message` → OpenAI message：
  - `assistant` 且有 `ToolCalls`：序列化出 `tool_calls`（每个含 `id`、`type:"function"`、`function.name`、`function.arguments`，**arguments 是 JSON 字符串**，由 `string(call.Args)` 得到）。
  - `role=tool`：序列化为 `{"role":"tool","tool_call_id":...,"content":...}`。
  - 其它（system/user/普通 assistant）：`{"role":..., "content":...}`。
- 中立 `ToolSpec` → OpenAI tool：`{"type":"function","function":{"name","description","parameters"}}`。
- OpenAI 响应 → 中立 `Response`：取 `choices[0].message`；`tool_calls[i].function.arguments`（字符串）→ `ToolCall.Args = json.RawMessage(arguments)`；`StopReason`：有 tool_calls → `"tool_calls"`，否则用 `finish_reason`（通常 `"stop"`）。
- 非 200 或响应体含 `error` 字段：返回带原始报文的清晰错误。

OpenAI 内部 DTO（`oaiRequest/oaiMessage/oaiToolCall/oaiTool/oaiResponse`）只存在于本文件，不外泄。

### 6.3 `tools/bash.go`

`Bash` 实现 `tools.Tool`：
- `Spec()`：name=`bash`，描述说明"独立 `zsh -c` 子进程，cd/export/alias 不跨调用保留"，参数 schema 含必填 `command`。
- `Run(ctx, args)`：解析 `{command}`，`exec.CommandContext(ctx, "zsh", "-c", command).CombinedOutput()`，合并 stdout+stderr；非零退出码追加 `[退出码 N]`；空输出返回"（无输出）"。
- **确认逻辑不在这里**——已上移到循环里的 `UI.ConfirmTool`。本工具只管执行。

### 6.4 `agent/terminal.go`

`TerminalUI` 实现 `agent.UI`，基于 `os.Stdin`/`os.Stdout`：
- `AssistantText`：直接打印。
- `ConfirmTool(name, preview)`：黄色显示 `name` + 参数预览，提示 `执行？[Y/n]`，读一行；`n`/`no` 返回 false，其余返回 true。
- `ToolOutput`：打印工具输出，必要时补换行。
- 构造器 `NewTerminalUI(in io.Reader, out io.Writer) *TerminalUI`。

## 7. 核心循环 `agent/agent.go` —— 由主 agent 亲自编写

```go
type Agent struct {
	provider llm.Provider
	tools    *tools.Registry
	ui       UI
}

func New(p llm.Provider, t *tools.Registry, ui UI) *Agent

// Run 处理一个回合：反复问模型，模型要工具就执行并回填，直到 stop。
func (a *Agent) Run(ctx context.Context, history []llm.Message) ([]llm.Message, error) {
	for {
		resp, err := a.provider.Chat(ctx, history, a.tools.Specs())
		if err != nil {
			return history, err
		}
		history = append(history, resp.Message)
		if resp.Message.Content != "" {
			a.ui.AssistantText(resp.Message.Content)
		}
		if resp.StopReason != "tool_calls" {
			return history, nil
		}
		for _, call := range resp.Message.ToolCalls {
			tool, ok := a.tools.Get(call.Name)
			if !ok {
				history = append(history, llm.ToolResult(call.ID, "未知工具: "+call.Name, true))
				continue
			}
			if !a.ui.ConfirmTool(call.Name, string(call.Args)) {
				history = append(history, llm.ToolResult(call.ID, "用户拒绝执行此命令。", true))
				continue
			}
			out, err := tool.Run(ctx, call.Args)
			if err != nil {
				out = out + "\n[执行错误: " + err.Error() + "]"
			}
			a.ui.ToolOutput(out)
			history = append(history, llm.ToolResult(call.ID, out, err != nil))
		}
	}
}
```

## 8. 入口 `main.go` —— 由主 agent 亲自编写

```go
func main() {
	path := os.Getenv("AGENT_CONFIG")
	if path == "" { path = "config.json" }
	cfg, err := config.Load(path)               // 读配置
	// ... err 处理
	provider := llm.NewOpenAIProvider(cfg)        // 建 provider（含 TLS 判断）
	reg := tools.NewRegistry()
	reg.Register(tools.Bash{})                    // 注册工具
	ui := agent.NewTerminalUI(os.Stdin, os.Stdout)
	ag := agent.New(provider, reg, ui)

	system := cfg.SystemPrompt
	if system == "" { system = defaultSystemPrompt }
	history := []llm.Message{{Role: llm.RoleSystem, Content: system}}

	// REPL：读用户输入 → 追加 user message → ag.Run → 更新 history
}
```

（`NewOpenAIProvider` 的入参可直接收 `*config.Config`，或拆成具体字段；实现 subagent 二选一，主 agent 在组装时适配。）

## 9. 数据流

```
用户输入 ─▶ history(+user) ─▶ Agent.Run ─▶ Provider.Chat ─▶ OpenAI /chat/completions
                ▲                              │
                │        StopReason==stop      │ 翻译响应
                │   ┌──────────────────────────┘
                │   ▼ 否则（tool_calls）
                │  UI.ConfirmTool → Tool.Run → UI.ToolOutput
                └── 把 ToolResult 追加进 history，继续循环
```

## 10. 错误处理
- 配置缺失/非法：启动即报清晰错误并退出。
- HTTP 非 200 / 响应含 error：`Chat` 返回带原始报文的错误；REPL 打印后丢弃本轮 user 消息，允许干净重试。
- 工具执行出错：作为 `IsError` 的 tool 结果喂回模型，让模型自行决定下一步（不中断循环）。

## 11. 测试策略（聚焦"纯逻辑/易错点"，不为覆盖率而测）
- `llm`：`shouldSkipVerify` 表驱动测（纯 IP / 带端口 IP / 域名 / 手动覆盖）；中立↔OpenAI 翻译的往返测（尤其 tool_calls 的 arguments 字符串、多轮 tool 消息）。
- `config`：缺必填字段报错；默认值填充。
- `tools`：bash 执行回显与非零退出码（可用 `echo` / `false` 之类稳定命令）。

## 12. 扩展示例（验证设计达成"易迭代"）
- **加工具**：新建 `tools/readfile.go` 实现 `Tool`，`main` 里 `reg.Register(...)` 一行。循环零改动。
- **换 LLM 后端**：新建 `llm/anthropic.go` 实现 `Provider`，`main` 里换构造。循环、工具零改动。
- **换交互**：实现 `agent.UI`（如 HTTP/JSON），复用 `Agent.Run`。

## 13. 实施计划
1. 主 agent 编写契约：`llm/types.go`、`llm/provider.go`、`tools/tool.go`、`agent/ui.go`。
2. 并行派 subagent 实现叶子（各写独立文件，按上面契约）：`config/config.go`(+example+gitignore)、`llm/openai.go`、`tools/bash.go`、`agent/terminal.go`。
3. 主 agent review 每个产出（接口符合度、翻译正确性、错误处理、注释质量），不合格打回。
4. 主 agent 编写 `agent/agent.go` + `main.go` 组装，`go build ./...` + `go vet` + 跑测试 + 真实 curl 端点联调。

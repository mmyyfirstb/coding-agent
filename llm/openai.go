package llm

// openai.go 实现一个「OpenAI 兼容」的 Provider。
//
// 所谓「OpenAI 兼容」：只要后端暴露 POST /chat/completions 且请求/响应体
// 遵循 OpenAI Chat Completions 协议，本实现就能直接对接——这覆盖了绝大多数
// 国内外大模型服务（vLLM / Ollama 兼容层 / DeepSeek / Moonshot / 各类网关 …）。
//
// 本文件只依赖 Go 标准库，核心职责就两件事：
//  1. 把「中立类型」(llm.Message / llm.ToolSpec) 翻译成 OpenAI 请求 JSON；
//  2. 把 OpenAI 响应 JSON 翻译回「中立类型」(llm.Response)。
// 翻译逻辑被拆成若干小的非导出函数，方便单测逐个验证。

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// OpenAIProvider 是 llm.Provider 的 OpenAI 兼容实现。
type OpenAIProvider struct {
	baseURL    string       // 服务根地址，例如 https://api.example.com/v1
	apiKey     string       // 鉴权用的 API Key（放进 Authorization: Bearer）
	model      string       // 模型名，例如 gpt-4o / deepseek-chat
	maxTokens  int          // 生成上限；为 0 时不下发该字段（让服务端用默认值）
	stream     bool         // 是否用 SSE 流式接收（逐字显示）；false 则一次性整段返回
	httpClient *http.Client // 复用连接的 HTTP 客户端（含 TLS 配置）
}

// NewOpenAIProvider 构造一个 OpenAIProvider。
//
// insecureOverride 用于显式控制是否跳过 TLS 证书校验：
//   - 传 nil  → 自动判断（见 shouldSkipVerify：纯 IP 端点跳过，域名正常校验）；
//   - 传 &true / &false → 强制覆盖自动判断。
func NewOpenAIProvider(baseURL, apiKey, model string, maxTokens int, stream bool, insecureOverride *bool) *OpenAIProvider {
	// 根据 baseURL 与覆盖项决定是否跳过证书校验。
	skip := shouldSkipVerify(baseURL, insecureOverride)

	// 自定义 Transport，把 TLS 配置塞进去。其余走默认即可。
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: skip},
	}

	return &OpenAIProvider{
		baseURL:    baseURL,
		apiKey:     apiKey,
		model:      model,
		maxTokens:  maxTokens,
		stream:     stream,
		httpClient: &http.Client{Transport: transport},
	}
}

// shouldSkipVerify 决定是否跳过 TLS 证书校验，是一个纯函数（无副作用、可单测）。
//
// 规则：
//   - 若 override != nil → 直接返回 *override（调用方显式拍板）。
//   - 否则解析 baseURL 取主机名：
//   - 主机名是纯 IP（如 203.0.113.10）→ 返回 true。
//     原因：直连 IP 的自建/内网端点通常没有匹配该 IP 的合法证书，
//     这里等价于 `curl -k`，跳过校验以保证能连上。
//   - 主机名是域名（如 api.example.com）→ 返回 false，走正常证书校验。
func shouldSkipVerify(baseURL string, override *bool) bool {
	// 1) 显式覆盖优先。
	if override != nil {
		return *override
	}

	// 2) 解析 URL 失败时保守处理：不跳过校验。
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}

	// 3) 主机名是纯 IP → 跳过；是域名 → 不跳过。
	host := u.Hostname()
	if net.ParseIP(host) != nil {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// 请求 / 响应的 OpenAI DTO（Data Transfer Object）。
// 这些结构体只服务于「与 OpenAI 协议互转」，故全部非导出、只存在于本文件。
// 中立类型 (llm.Message 等) 不感知这些字段名，翻译都在本文件内完成。
// ---------------------------------------------------------------------------

// openAIRequest 是发往 /chat/completions 的请求体。
type openAIRequest struct {
	Model     string          `json:"model"`
	Messages  []openAIMessage `json:"messages"`
	Tools     []openAITool    `json:"tools,omitempty"`      // 无工具时不下发
	MaxTokens int             `json:"max_tokens,omitempty"` // 为 0 时不下发
	Stream    bool            `json:"stream,omitempty"`     // 为 false 时不下发（默认非流式）
}

// openAIMessage 是 OpenAI 协议里的一条消息。
type openAIMessage struct {
	Role string `json:"role"`
	// content 在「assistant 仅发起工具调用」时可能为空，故 omitempty。
	Content string `json:"content,omitempty"`
	// 思考 / 推理内容。不同后端字段名不一（Ollama/部分网关用 reasoning，
	// DeepSeek-R1/vLLM 用 reasoning_content），两个都解析、取非空者。
	// 这两个字段仅在「解析响应」时有意义；构造请求时从不赋值，omitempty
	// 保证不会回传给模型（思考不该喂回去）。
	Reasoning        string `json:"reasoning,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
	// 仅 assistant 发起工具调用时出现。
	ToolCalls []openAIToolCall `json:"tool_calls,omitempty"`
	// 仅 role=tool 时出现：标明本结果对应哪次调用。
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// openAIToolCall 是 OpenAI 协议里一次工具调用的描述。
type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"` // 固定 "function"
	Function openAIToolCallFunc `json:"function"`
}

// openAIToolCallFunc 描述被调函数名与参数。
//
// 关键点：Arguments 是「一个 JSON 字符串」，而不是 JSON 对象。
// 即字段类型是 string，其内容形如 `{"path":"a.go"}`（注意外层是字符串）。
// 这正是 OpenAI 协议的约定，也是本实现里最容易踩坑的地方。
type openAIToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// openAITool 是暴露给模型的一个可调用工具的声明。
type openAITool struct {
	Type     string         `json:"type"` // 固定 "function"
	Function openAIToolFunc `json:"function"`
}

// openAIToolFunc 描述工具函数：名字 / 说明 / 参数 JSON Schema。
type openAIToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// openAIResponse 是 /chat/completions 的响应体（只解析我们关心的字段）。
type openAIResponse struct {
	Choices []openAIChoice `json:"choices"`
	// error 字段存在即代表服务端报错（即便 HTTP 200 也可能带它）。
	Error json.RawMessage `json:"error"`
}

// openAIChoice 是响应里的一个候选回复。
type openAIChoice struct {
	Message      openAIMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

// --- 流式（SSE）专用 DTO ---
// 流式响应是一串 `data: {chunk}` 事件，每个 chunk 的 choice 里是「增量」(delta)，
// 而不是完整 message：content / reasoning 一片片来，tool_calls 也按 index 分片到达。

// openAIStreamChunk 是流式里的一个 chunk（一条 data: 行的 JSON）。
type openAIStreamChunk struct {
	Choices []openAIStreamChoice `json:"choices"`
	Error   json.RawMessage      `json:"error"` // 某些网关会在流里塞 error
}

// openAIStreamChoice 是 chunk 里的候选，核心是 delta。
type openAIStreamChoice struct {
	Delta        openAIDelta `json:"delta"`
	FinishReason string      `json:"finish_reason"`
}

// openAIDelta 是一次增量：各字段都可能为空（本片没这部分内容）。
type openAIDelta struct {
	Content          string                `json:"content"`
	Reasoning        string                `json:"reasoning"`
	ReasoningContent string                `json:"reasoning_content"`
	ToolCalls        []openAIDeltaToolCall `json:"tool_calls"`
}

// openAIDeltaToolCall 是工具调用的一个分片。
// Index 标明它属于第几个调用（用来把分片拼回完整调用）；
// 通常 id / name 只在首片出现，arguments 则跨多片拼接。
type openAIDeltaToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ---------------------------------------------------------------------------
// 翻译：中立类型 → OpenAI 请求体
// ---------------------------------------------------------------------------

// toOpenAIMessages 把中立消息列表翻译成 OpenAI 消息列表。
//
// 三种情况：
//   - assistant 且含 ToolCalls：翻成带 tool_calls 的 assistant 消息，
//     其中每个调用的 arguments 用 string(call.Args) 转成「JSON 字符串」。
//   - role=tool：翻成带 tool_call_id 的 tool 消息（回填工具结果）。
//   - 其它（system / user / 纯文本 assistant）：直接搬运 role + content。
func toOpenAIMessages(msgs []Message) []openAIMessage {
	out := make([]openAIMessage, 0, len(msgs))
	for _, m := range msgs {
		switch {
		case m.Role == RoleAssistant && len(m.ToolCalls) > 0:
			// assistant 发起工具调用。
			calls := make([]openAIToolCall, 0, len(m.ToolCalls))
			for _, c := range m.ToolCalls {
				calls = append(calls, openAIToolCall{
					ID:   c.ID,
					Type: "function",
					Function: openAIToolCallFunc{
						Name: c.Name,
						// 关键：Args 是 json.RawMessage（原始 JSON 字节），
						// 这里转成 string 即得到「JSON 字符串」形式的 arguments。
						Arguments: string(c.Args),
					},
				})
			}
			out = append(out, openAIMessage{
				Role:      RoleAssistant,
				Content:   m.Content, // 可能为空，omitempty 会自动省略
				ToolCalls: calls,
			})

		case m.Role == RoleTool:
			// 工具执行结果回填。
			out = append(out, openAIMessage{
				Role:       RoleTool,
				ToolCallID: m.ToolCallID,
				Content:    m.Content,
			})

		default:
			// system / user / 纯文本 assistant。
			out = append(out, openAIMessage{
				Role:    m.Role,
				Content: m.Content,
			})
		}
	}
	return out
}

// toOpenAITools 把中立工具声明翻译成 OpenAI 工具声明。
// 返回 nil 时（无工具），上层请求体的 tools 字段会因 omitempty 被省略。
func toOpenAITools(tools []ToolSpec) []openAITool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]openAITool, 0, len(tools))
	for _, t := range tools {
		out = append(out, openAITool{
			Type: "function",
			Function: openAIToolFunc{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters, // 直接透传 JSON Schema
			},
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// 翻译：OpenAI 响应 → 中立类型
// ---------------------------------------------------------------------------

// fromOpenAIChoice 把一个 OpenAI choice 翻译成中立 Response。
//
//   - Message.Role 固定为 "assistant"。
//   - Message.Content 取 choice.message.content。
//   - 每个 tool_call 翻成 llm.ToolCall，其 Args 把「arguments 字符串」
//     还原回 json.RawMessage（原始 JSON 字节）。
//   - StopReason：有工具调用 → "tool_calls"；否则取 finish_reason，
//     空则兜底为 "stop"。
func fromOpenAIChoice(choice openAIChoice) *Response {
	msg := Message{
		Role:      RoleAssistant,
		Content:   choice.Message.Content,
		Reasoning: pickReasoning(choice.Message.Reasoning, choice.Message.ReasoningContent),
	}

	for _, tc := range choice.Message.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{
			ID:   tc.ID,
			Name: tc.Function.Name,
			// arguments 在协议里是字符串，其内容本身就是 JSON，
			// 故直接转回 RawMessage 即可被后续按 schema 解析。
			Args: json.RawMessage(tc.Function.Arguments),
		})
	}

	stopReason := choice.FinishReason
	if len(msg.ToolCalls) > 0 {
		stopReason = "tool_calls"
	} else if stopReason == "" {
		stopReason = "stop"
	}

	return &Response{
		Message:    msg,
		StopReason: stopReason,
	}
}

// ---------------------------------------------------------------------------
// Chat：完整的一次请求/响应往返
// ---------------------------------------------------------------------------

// Chat 实现 llm.Provider：把对话历史 + 工具发给模型，返回归一化的下一步。
//
// 流式与否由 p.stream 决定，并且对调用方透明：两种模式都通过 sink 把文字吐出去、
// 都返回组装好的完整 *Response。差别只在内部——非流式一次性解析，流式边读 SSE 边解析。
func (p *OpenAIProvider) Chat(ctx context.Context, msgs []Message, tools []ToolSpec, sink StreamSink) (*Response, error) {
	// 1) 组装 OpenAI 请求体。
	reqBody := openAIRequest{
		Model:     p.model,
		Messages:  toOpenAIMessages(msgs),
		Tools:     toOpenAITools(tools),
		MaxTokens: p.maxTokens, // 为 0 时 omitempty 省略
		Stream:    p.stream,    // 为 false 时 omitempty 省略
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("openai: 序列化请求失败: %w", err)
	}

	// 2) 拼接 URL：去掉 baseURL 尾部的 "/" 再接 "/chat/completions"。
	endpoint := strings.TrimRight(p.baseURL, "/") + "/chat/completions"

	// 3) 构造带 ctx 的请求（支持上层取消/超时）。
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("openai: 构造请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")
	if p.stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	// 4) 发请求。
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai: 请求发送失败: %w", err)
	}
	defer resp.Body.Close()

	// 5) 非 200 一律视为失败：读出 body（可能是 JSON error）一并带进错误信息。
	//    这一步对流式 / 非流式相同——出错时服务端给的都不是正常内容。
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("openai: 请求失败 (status=%d): %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// 6) 按模式分派解析。两条路都会：边解析边喂 sink，最后返回完整 Response。
	if p.stream {
		return parseSSE(resp.Body, sink)
	}
	return parseOnce(resp.Body, sink)
}

// parseOnce 解析「非流式」整段响应，并把整段内容当作一次 delta 喂给 sink。
func parseOnce(r io.Reader, sink StreamSink) (*Response, error) {
	respBytes, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("openai: 读取响应失败: %w", err)
	}

	var parsed openAIResponse
	jsonErr := json.Unmarshal(respBytes, &parsed)

	// 即便 HTTP 200，某些网关也会塞顶层 error，先判这个。
	if hasTopLevelError(parsed.Error) {
		return nil, fmt.Errorf("openai: 请求失败: %s", strings.TrimSpace(string(respBytes)))
	}
	if jsonErr != nil {
		return nil, fmt.Errorf("openai: 解析响应 JSON 失败: %w (body=%s)",
			jsonErr, strings.TrimSpace(string(respBytes)))
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("openai: 响应不含任何 choices (body=%s)",
			strings.TrimSpace(string(respBytes)))
	}

	out := fromOpenAIChoice(parsed.Choices[0])
	// 非流式 = 一段的流式：把整段思考 / 正文各当一次 delta 发出去，统一显示路径。
	emit(sink, out.Message.Reasoning, out.Message.Content)
	return out, nil
}

// parseSSE 解析「流式」响应：逐行扫 `data:` 事件，边解析边喂 sink，最后组装成完整 Response。
//
// 关键点：
//   - content / reasoning 一片片来，累加即可；
//   - tool_calls 分片到达，按 index 把 id / name / arguments 拼回完整调用；
//   - 遇到 [DONE] 结束；个别坏行容错跳过，不中断整条流。
func parseSSE(r io.Reader, sink StreamSink) (*Response, error) {
	var (
		content   strings.Builder
		reasoning strings.Builder
		acc       = map[int]*accTool{} // index → 累积中的工具调用
		order     []int                // 记录 index 首次出现的顺序，保证结果有序
		finish    string
	)

	sc := bufio.NewScanner(r)
	// 放大行缓冲：单个 SSE 事件可能较长（长 JSON），默认 64KB 上限不够稳妥。
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		// SSE 数据行形如 "data: {...}"；其它行（event: / 注释 / 空行）忽略。
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}

		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // 坏行跳过，保持整条流不中断
		}
		if hasTopLevelError(chunk.Error) {
			return nil, fmt.Errorf("openai: 流式响应报错: %s", strings.TrimSpace(string(chunk.Error)))
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		d := choice.Delta

		if rd := pickReasoning(d.Reasoning, d.ReasoningContent); rd != "" {
			reasoning.WriteString(rd)
			sink.OnReasoning(rd)
		}
		if d.Content != "" {
			content.WriteString(d.Content)
			sink.OnContent(d.Content)
		}
		for _, tc := range d.ToolCalls {
			a, ok := acc[tc.Index]
			if !ok {
				a = &accTool{}
				acc[tc.Index] = a
				order = append(order, tc.Index)
			}
			if tc.ID != "" {
				a.id = tc.ID
			}
			if tc.Function.Name != "" {
				a.name = tc.Function.Name
			}
			a.args.WriteString(tc.Function.Arguments)
		}
		if choice.FinishReason != "" {
			finish = choice.FinishReason
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("openai: 读取流式响应失败: %w", err)
	}

	msg := Message{
		Role:      RoleAssistant,
		Content:   content.String(),
		Reasoning: reasoning.String(),
	}
	for _, idx := range order {
		a := acc[idx]
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{
			ID:   a.id,
			Name: a.name,
			Args: json.RawMessage(a.args.String()),
		})
	}

	stop := finish
	if len(msg.ToolCalls) > 0 {
		stop = "tool_calls"
	} else if stop == "" {
		stop = "stop"
	}
	return &Response{Message: msg, StopReason: stop}, nil
}

// accTool 是流式里一个「正在被分片拼接」的工具调用。
type accTool struct {
	id   string
	name string
	args strings.Builder
}

// emit 把非空的思考 / 正文各发一次给 sink（约定不转发空串）。
func emit(sink StreamSink, reasoning, content string) {
	if reasoning != "" {
		sink.OnReasoning(reasoning)
	}
	if content != "" {
		sink.OnContent(content)
	}
}

// pickReasoning 在两种字段名里取非空者（reasoning 优先，回退 reasoning_content）。
func pickReasoning(reasoning, reasoningContent string) string {
	if reasoning != "" {
		return reasoning
	}
	return reasoningContent
}

// hasTopLevelError 判断响应里是否带了顶层 "error" 对象。
//
// OpenAI 协议在出错时返回 {"error": {...}}；某些网关即便给 HTTP 200 也会塞 error。
// 这里只需判断该字段「存在且非 JSON null」即可。
func hasTopLevelError(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	if string(bytes.TrimSpace(raw)) == "null" {
		return false
	}
	return true
}

// Package llm 定义了一套「provider 中立」的对话类型，把具体某家大模型的
// HTTP/JSON 协议细节挡在各 Provider 实现里。
//
// 这么做的好处：agent 的核心循环只跟这些中立类型打交道，
// 换 LLM 后端（OpenAI / Anthropic / …）时循环一行都不用改。
package llm

import "encoding/json"

// 消息角色常量。对应对话里"谁说的话"。
const (
	RoleSystem    = "system"    // 系统提示
	RoleUser      = "user"      // 用户
	RoleAssistant = "assistant" // 模型
	RoleTool      = "tool"      // 工具执行结果（回填给模型）
)

// Message 是一条 provider 中立的对话消息。
//
// 不同 Role 下有意义的字段不同：
//   - system / user：只用 Content
//   - assistant：用 Content（文字），若模型要调工具则填 ToolCalls
//   - tool：用 ToolCallID（对应哪次调用）+ Content（结果）+ IsError
type Message struct {
	Role    string // system / user / assistant / tool
	Content string // 文本内容（assistant 请求工具时可能为空）
	// Reasoning 是 assistant 的「思考 / 推理」内容（部分模型会单独返回）。
	// 仅用于展示，不回传给模型——思考不该喂回去（省 token，也更规范）。
	Reasoning  string
	ToolCalls  []ToolCall // 仅 assistant：模型请求调用的工具
	ToolCallID string     // 仅 role=tool：对应哪个 ToolCall.ID
	IsError    bool       // 仅 role=tool：该结果是否为错误（供 UI 展示）
}

// ToolCall 是模型发起的一次工具调用请求。
type ToolCall struct {
	ID   string          // 本次调用唯一 id（回填结果时要用同一个 id）
	Name string          // 工具名
	Args json.RawMessage // 参数 JSON，应符合对应 ToolSpec.Parameters 的 schema
}

// ToolSpec 是暴露给模型的"我有哪些工具可调"的声明。
type ToolSpec struct {
	Name        string
	Description string
	Parameters  json.RawMessage // JSON Schema，描述参数结构
}

// Response 是 Provider 归一化后的"模型下一步"。
type Response struct {
	Message    Message // assistant 回复（可能含 ToolCalls）
	StopReason string  // "stop"（说完了）/ "tool_calls"（想调工具），由 Provider 归一化
}

// StreamSink 接收模型输出的「增量」，是 Provider 把内容吐出来、UI 把内容显示出去
// 之间唯一的细线。
//
// 关键设计：非流式 = 只有一个 chunk 的流式。
//   - 流式 Provider：边读 SSE 边多次调用（每个 delta 一次）。
//   - 非流式 Provider：拿到整段后，把整段当作「一次 delta」调用一次。
//
// 于是「是不是流式」对核心循环和 UI 完全透明——这层判断封死在 Provider 内部。
//
// 约定：实现者只会收到「非空」增量，Provider 不应转发空字符串。
type StreamSink interface {
	OnReasoning(delta string) // 思考（推理）增量
	OnContent(delta string)   // 正文增量
}

// ToolResult 构造一条 role=tool 的消息，方便核心循环把工具执行结果回填进历史。
func ToolResult(toolCallID, content string, isError bool) Message {
	return Message{
		Role:       RoleTool,
		ToolCallID: toolCallID,
		Content:    content,
		IsError:    isError,
	}
}

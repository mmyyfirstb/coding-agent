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
	Role       string     // system / user / assistant / tool
	Content    string     // 文本内容（assistant 请求工具时可能为空）
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

// ToolResult 构造一条 role=tool 的消息，方便核心循环把工具执行结果回填进历史。
func ToolResult(toolCallID, content string, isError bool) Message {
	return Message{
		Role:       RoleTool,
		ToolCallID: toolCallID,
		Content:    content,
		IsError:    isError,
	}
}

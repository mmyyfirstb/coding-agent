package llm

import "context"

// Provider 是 LLM 后端的统一抽象——整个框架对"大模型"的唯一依赖面。
//
// 换后端（OpenAI 兼容 / Anthropic / 本地推理 …）= 新写一个实现本接口的类型，
// agent 核心循环与工具层完全不用动。这就是框架可扩展性的来源。
type Provider interface {
	// Chat 把整段对话历史 + 当前可用工具发给模型，返回模型的下一步回复。
	//
	// 约定：
	//   - 实现负责把中立类型翻译成自家协议、发请求、再把响应翻译回中立 Response。
	//   - 返回的 Response.StopReason 必须归一化为 "tool_calls"（模型想调工具）
	//     或其它值（通常 "stop"，表示本轮说完）。
	Chat(ctx context.Context, msgs []Message, tools []ToolSpec) (*Response, error)
}

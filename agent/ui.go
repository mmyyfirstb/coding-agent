// Package agent 实现 agent 的核心循环，以及与外界交互的抽象。
package agent

import "zsh-agent/llm"

// UI 把"与人交互"从核心循环里隔离出来。
//
// 核心循环只调用这几个方法，不关心背后是终端、HTTP 服务还是日志文件。
// 想把 agent 改成别的形态（比如 Web 服务）时，换一个 UI 实现即可。
type UI interface {
	// Sink 返回本回合用于「实时显示模型输出」的 sink。
	// 核心循环把它传给 Provider.Chat，模型的思考 / 正文就会经它显示出来；
	// 每次 Chat 调用前取一个新的，结束后 Close 收尾。
	Sink() OutputSink
	// ConfirmTool 在执行某个工具前征求用户同意。
	// name 是工具名，preview 是参数预览。返回 true 表示允许执行。
	ConfirmTool(name, preview string) bool
	// ToolOutput 显示工具执行的输出。
	ToolOutput(s string)
}

// OutputSink 是「一个回合的显示 sink」：
//   - 内嵌 llm.StreamSink（OnReasoning / OnContent）给 Provider 用——
//     Provider 只看见这窄窄的一面，不该碰显示的生命周期；
//   - 额外的 Close 给核心循环用，做收尾（补换行 / 重置样式）。
//
// 这样把「往外吐增量」和「收尾」两种关注点分给两个角色，互不越界。
type OutputSink interface {
	llm.StreamSink
	Close()
}

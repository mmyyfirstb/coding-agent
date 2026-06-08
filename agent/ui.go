// Package agent 实现 agent 的核心循环，以及与外界交互的抽象。
package agent

// UI 把"与人交互"从核心循环里隔离出来。
//
// 核心循环只调用这几个方法，不关心背后是终端、HTTP 服务还是日志文件。
// 想把 agent 改成别的形态（比如 Web 服务）时，换一个 UI 实现即可。
type UI interface {
	// AssistantText 显示模型的文字回复。
	AssistantText(s string)
	// ConfirmTool 在执行某个工具前征求用户同意。
	// name 是工具名，preview 是参数预览。返回 true 表示允许执行。
	ConfirmTool(name, preview string) bool
	// ToolOutput 显示工具执行的输出。
	ToolOutput(s string)
}

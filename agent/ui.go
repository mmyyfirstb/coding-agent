// Package agent 实现 agent 的核心循环，以及与外界交互的抽象。
package agent

import (
	"context"

	"zsh-agent/llm"
)

// UI 把"与人交互"从核心循环里隔离出来。
//
// 核心循环只调用这几个方法，不关心背后是终端、HTTP 服务还是日志文件。
// 想把 agent 改成别的形态（比如 Web 服务）时，换一个 UI 实现即可。
type UI interface {
	// Sink 返回本回合用于「实时显示模型输出」的 sink。
	Sink() OutputSink
	// ReadLine 读取用户的一行输入（终端实现支持 raw 模式下的 rune/宽度感知行编辑）。
	// 返回输入文本与结局（提交 / EOF 退出 / 中断重来）。
	ReadLine(prompt string) (string, Outcome)
	// ConfirmTool 在执行某个工具前征求用户同意。返回 true 表示允许执行。
	ConfirmTool(name, preview string) bool
	// ToolOutput 显示工具执行的输出。
	ToolOutput(s string)
	// WatchInterrupt 在一段「可取消操作」期间监听打断键（ESC / Ctrl-C），命中即调
	// cancel。返回的 stop 在操作结束后调用以停止监听、交还键盘。非 raw 实现返回 no-op。
	WatchInterrupt(cancel context.CancelFunc) (stop func())
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

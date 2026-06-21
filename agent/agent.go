package agent

import (
	"context"
	"fmt"

	"zsh-agent/llm"
	"zsh-agent/tools"
)

// maxSteps 是单个回合内"问模型→执行工具"的最大轮数上限。
//
// 正常情况下模型几步就会给出最终文字回复而结束；设这个上限是一道保险，
// 防止模型与工具陷入互相触发的死循环把对话无限拖下去。
const maxSteps = 50

// maxToolOutputRunes 是「单条工具结果回填进历史」时允许的最大字符数。
//
// 超出部分会被 clampToolOutput 截掉中间、只留头尾（见 clamp.go）。这是为了
// 保护模型的上下文窗口：bash 等命令动辄吐出几十 KB，原样累积会迅速塞满窗口、
// 稀释注意力，甚至把最早的 system prompt 挤出窗口。注意这只影响「喂回模型的
// 副本」，UI 展示给人看的仍是完整输出。
const maxToolOutputRunes = 4000

// Agent 把三件可替换的东西组装在一起，对外只暴露一个 Run：
//   - provider：LLM 后端（怎么问模型）
//   - tools：工具注册表（模型能调用哪些工具）
//   - ui：交互层（怎么把过程展示给人、怎么征求确认）
//
// 注意这三个字段都是接口/抽象类型，所以 Agent 完全不关心
// "后端是 OpenAI 还是别的""UI 是终端还是 HTTP"——这正是可扩展性的来源。
type Agent struct {
	provider llm.Provider
	tools    *tools.Registry
	ui       UI
}

// New 组装一个 Agent。
func New(p llm.Provider, t *tools.Registry, ui UI) *Agent {
	return &Agent{provider: p, tools: t, ui: ui}
}

// Run 处理用户的一个回合，是整个 agent 的心脏。
//
// 它就是在反复做一件事（对照数据流图）：
//
//	问模型 ──▶ 模型要调工具？──否──▶ 收工，返回更新后的历史
//	  ▲                  │是
//	  │                  ▼
//	  └── 把工具结果 ◀── 逐个执行工具（先经 UI 确认）
//	      追加进历史
//
// 入参 history 是到目前为止的完整对话（含 system / user 等）；
// 返回值是追加了本回合所有 assistant 回复与工具结果后的新历史。
func (a *Agent) Run(ctx context.Context, history []llm.Message) ([]llm.Message, error) {
	startLen := len(history) // 打断时回滚到这里（含本回合的 user 消息）
	for step := 0; step < maxSteps; step++ {
		// 1. 问模型；WatchInterrupt 期间按 ESC/Ctrl-C 会 cancel 掉 cctx，
		//    令正在读的 SSE/HTTP 立刻报错返回。
		sink := a.ui.Sink()
		cctx, cancel := context.WithCancel(ctx)
		stop := a.ui.WatchInterrupt(cancel)
		resp, err := a.provider.Chat(cctx, history, a.tools.Specs(), sink)
		stop()
		canceled := cctx.Err() == context.Canceled
		cancel()
		sink.Close()
		if canceled {
			return history[:startLen], context.Canceled // 回滚本回合 append
		}
		if err != nil {
			return history, err
		}

		// 2. 记进历史。
		history = append(history, resp.Message)

		// 3. 不再调工具 → 收工。
		if resp.StopReason != "tool_calls" {
			return history, nil
		}

		// 4. 逐个执行工具（先确认；执行期间同样可打断）。
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

			tctx, tcancel := context.WithCancel(ctx)
			tstop := a.ui.WatchInterrupt(tcancel)
			out, rerr := tool.Run(tctx, call.Args)
			tstop()
			tcanceled := tctx.Err() == context.Canceled
			tcancel()
			if tcanceled {
				return history[:startLen], context.Canceled
			}
			if rerr != nil {
				out += "\n[执行错误: " + rerr.Error() + "]"
			}
			// UI 展示完整输出；回填历史的副本做截断（保护上下文窗口）。
			a.ui.ToolOutput(out)
			history = append(history, llm.ToolResult(call.ID, clampToolOutput(out, maxToolOutputRunes), rerr != nil))
		}
	}

	return history, fmt.Errorf("达到最大步数 %d，已停止（可能陷入工具循环）", maxSteps)
}

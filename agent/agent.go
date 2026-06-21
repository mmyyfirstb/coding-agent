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
	for step := 0; step < maxSteps; step++ {
		// 1. 问模型：把完整历史 + 当前可用工具发过去。
		//    sink 负责把模型的思考 / 正文实时显示出来（流式逐字、非流式整段），
		//    所以这里不再单独调 UI 展示文字——显示已在 Chat 内部通过 sink 完成。
		sink := a.ui.Sink()
		resp, err := a.provider.Chat(ctx, history, a.tools.Specs(), sink)
		sink.Close() // 收尾（补换行 / 重置样式）；出错也要收尾，别让终端样式残留。
		if err != nil {
			return history, err
		}

		// 2. 把模型这次的整段回复（文字 + 可能的工具调用）记进历史。
		history = append(history, resp.Message)

		// 3. 模型不再要求调工具 → 本回合结束。
		if resp.StopReason != "tool_calls" {
			return history, nil
		}

		// 4. 逐个执行模型请求的工具，把每个结果作为一条 tool 消息追加回历史。
		for _, call := range resp.Message.ToolCalls {
			tool, ok := a.tools.Get(call.Name)
			if !ok {
				history = append(history, llm.ToolResult(call.ID, "未知工具: "+call.Name, true))
				continue
			}

			// 执行前先经 UI 确认（终端实现就是 y/n 询问）。
			if !a.ui.ConfirmTool(call.Name, string(call.Args)) {
				history = append(history, llm.ToolResult(call.ID, "用户拒绝执行此命令。", true))
				continue
			}

			out, err := tool.Run(ctx, call.Args)
			if err != nil {
				// 工具真正失败（如无法启动子进程）：把错误并进结果文本，
				// 并以 isError=true 回填，让模型知道这步没成。
				out += "\n[执行错误: " + err.Error() + "]"
			}
			a.ui.ToolOutput(out)
			history = append(history, llm.ToolResult(call.ID, out, err != nil))
		}
	}

	// 走到这里说明连续 maxSteps 轮都在调工具还没收尾，按异常处理。
	return history, fmt.Errorf("达到最大步数 %d，已停止（可能陷入工具循环）", maxSteps)
}

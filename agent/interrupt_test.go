package agent

import (
	"context"
	"testing"

	"zsh-agent/llm"
	"zsh-agent/tools"
)

// blockingProvider 的 Chat 阻塞到 ctx 取消，再返回 ctx.Err()。
type blockingProvider struct{}

func (blockingProvider) Chat(ctx context.Context, _ []llm.Message, _ []llm.ToolSpec, _ llm.StreamSink) (*llm.Response, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// interruptUI 的 WatchInterrupt 立刻 cancel，模拟用户一进生成就按 ESC。
type interruptUI struct{}

func (interruptUI) Sink() OutputSink                  { return nopSink{} }
func (interruptUI) ReadLine(string) (string, Outcome) { return "", OutcomeEOF }
func (interruptUI) ConfirmTool(string, string) bool   { return true }
func (interruptUI) ToolOutput(string)                 {}
func (interruptUI) WatchInterrupt(cancel context.CancelFunc) func() {
	cancel()
	return func() {}
}

type nopSink struct{}

func (nopSink) OnReasoning(string) {}
func (nopSink) OnContent(string)   {}
func (nopSink) Close()             {}

func TestAgentRun_InterruptRollsBack(t *testing.T) {
	ag := New(blockingProvider{}, tools.NewRegistry(), interruptUI{})
	history := []llm.Message{
		{Role: llm.RoleSystem, Content: "sys"},
		{Role: llm.RoleUser, Content: "hi"},
	}
	out, err := ag.Run(context.Background(), history)
	if err != context.Canceled {
		t.Fatalf("期望 context.Canceled，得到 %v", err)
	}
	if len(out) != len(history) {
		t.Fatalf("历史应回滚到 %d 条，实际 %d", len(history), len(out))
	}
}

// 连续多次打断：每次都应把「本回合 user + 打断占位」如实记进历史，前一次的记录不被覆盖。
// 这里复刻 main 打断后的善后（history = append(updated, 占位)），用真实 Run 验证切片别名安全。
func TestAgentRun_ConsecutiveInterruptsAllRecorded(t *testing.T) {
	ag := New(blockingProvider{}, tools.NewRegistry(), interruptUI{})
	history := []llm.Message{{Role: llm.RoleSystem, Content: "sys"}}

	const note = "（上一条回复被你打断了）"
	for _, q := range []string{"q1", "q2", "q3"} {
		history = append(history, llm.Message{Role: llm.RoleUser, Content: q})
		updated, err := ag.Run(context.Background(), history)
		if err != context.Canceled {
			t.Fatalf("问 %q 期望 Canceled，得到 %v", q, err)
		}
		history = append(updated, llm.Message{Role: llm.RoleAssistant, Content: note})
	}

	want := []llm.Message{
		{Role: llm.RoleSystem, Content: "sys"},
		{Role: llm.RoleUser, Content: "q1"}, {Role: llm.RoleAssistant, Content: note},
		{Role: llm.RoleUser, Content: "q2"}, {Role: llm.RoleAssistant, Content: note},
		{Role: llm.RoleUser, Content: "q3"}, {Role: llm.RoleAssistant, Content: note},
	}
	if len(history) != len(want) {
		t.Fatalf("历史 %d 条，期望 %d：%+v", len(history), len(want), history)
	}
	for i := range want {
		if history[i].Role != want[i].Role || history[i].Content != want[i].Content {
			t.Errorf("第 %d 条 = {%v %q}，期望 {%v %q}",
				i, history[i].Role, history[i].Content, want[i].Role, want[i].Content)
		}
	}
}

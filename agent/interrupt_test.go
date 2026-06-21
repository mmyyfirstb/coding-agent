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

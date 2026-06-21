package agent

// 验证主循环（Run）在把工具结果回填进历史前会对超长输出做截断：
//   - 历史里的 tool 消息被压短并带省略标记（保护上下文窗口）；
//   - 但 UI（ToolOutput）仍收到完整原始输出（人要看到真实结果）。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"zsh-agent/llm"
	"zsh-agent/tools"
)

// bigTool 返回一段远超截断上限的超大文本，用来触发截断。
type bigTool struct{ payload string }

func (bigTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "big",
		Description: "返回超大输出",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
	}
}

func (b bigTool) Run(_ context.Context, _ json.RawMessage) (string, error) {
	return b.payload, nil
}

func TestAgentRun_ClampsLargeToolOutputInHistory(t *testing.T) {
	// 10 万个字符，远大于任何合理的截断上限。
	payload := strings.Repeat("X", 100000)

	var round int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		round++
		w.Header().Set("Content-Type", "application/json")
		if round == 1 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"big","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"好了"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	provider := llm.NewOpenAIProvider(srv.URL, "sk-test", "test-model", 0, false, nil)
	reg := tools.NewRegistry()
	reg.Register(bigTool{payload: payload})
	ui := &fakeUI{}
	ag := New(provider, reg, ui)

	history := []llm.Message{
		{Role: llm.RoleSystem, Content: "你是助手"},
		{Role: llm.RoleUser, Content: "跑一下"},
	}
	out, err := ag.Run(context.Background(), history)
	if err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}

	// 找到回填进历史的那条 tool 消息。
	var toolMsg *llm.Message
	for i := range out {
		if out[i].Role == llm.RoleTool {
			toolMsg = &out[i]
			break
		}
	}
	if toolMsg == nil {
		t.Fatalf("历史里找不到 role=tool 的消息")
	}

	// 历史里的工具结果必须被截断：远小于原始大小，且带省略标记。
	if n := utf8.RuneCountInString(toolMsg.Content); n >= len(payload) {
		t.Fatalf("历史里的工具结果未被截断，长度=%d（原始=%d）", n, len(payload))
	}
	if !strings.Contains(toolMsg.Content, "已省略") {
		t.Errorf("被截断的工具结果应带省略标记，got 前 80 字符=%q", toolMsg.Content[:80])
	}

	// UI 必须拿到完整、未截断的输出。
	if len(ui.toolOutputs) != 1 {
		t.Fatalf("期望 UI 收到 1 次 ToolOutput，实际 %d", len(ui.toolOutputs))
	}
	if ui.toolOutputs[0] != payload {
		t.Errorf("UI 应收到完整原始输出，但收到的被改动了（长度=%d，期望=%d）",
			len(ui.toolOutputs[0]), len(payload))
	}
}

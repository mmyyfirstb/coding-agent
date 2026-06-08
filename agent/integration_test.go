package agent

// 端到端集成测试：用一个假的 OpenAI 服务（httptest）把"真实的 HTTP 往返 +
// 真实的 OpenAIProvider + 真实的 Agent 循环"全链路串起来跑一遍，不需要外网或真 key。
//
// 重点验证两件最容易错的事：
//  1. 模型返回 tool_calls 时，循环能执行工具并把结果回填；
//  2. 第二轮请求发回服务端的对话历史里，assistant 的 tool_calls（arguments 是字符串）
//     和 role=tool 的结果（带 tool_call_id）都被正确序列化——这是多轮协议的关键。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"zsh-agent/llm"
	"zsh-agent/tools"
)

// echoTool 是个不依赖外部环境的假工具：把入参 text 原样回显。
type echoTool struct{}

func (echoTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "echo",
		Description: "回显文本",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
	}
}

func (echoTool) Run(_ context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", err
	}
	return "echo: " + in.Text, nil
}

// fakeUI 自动同意所有工具，并记录展示过的内容，便于断言。
type fakeUI struct {
	confirmed []string
	assistant []string
}

func (f *fakeUI) AssistantText(s string) { f.assistant = append(f.assistant, s) }
func (f *fakeUI) ConfirmTool(name, preview string) bool {
	f.confirmed = append(f.confirmed, name)
	return true
}
func (f *fakeUI) ToolOutput(string) {}

func TestAgentRun_EndToEnd_ToolCallThenFinish(t *testing.T) {
	var round int
	var round2 map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		round++
		w.Header().Set("Content-Type", "application/json")
		if round == 1 {
			// 第一轮：让模型要求调用 echo 工具。注意 arguments 是 JSON 字符串。
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"echo","arguments":"{\"text\":\"hi\"}"}}]},"finish_reason":"tool_calls"}]}`))
			return
		}
		// 第二轮：记录收到的历史，并给出最终文字回复结束。
		_ = json.Unmarshal(body, &round2)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"完成了"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	provider := llm.NewOpenAIProvider(srv.URL, "sk-test", "test-model", 0, nil)
	reg := tools.NewRegistry()
	reg.Register(echoTool{})
	ui := &fakeUI{}
	ag := New(provider, reg, ui)

	history := []llm.Message{
		{Role: llm.RoleSystem, Content: "你是助手"},
		{Role: llm.RoleUser, Content: "回显 hi"},
	}
	out, err := ag.Run(context.Background(), history)
	if err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}
	if round != 2 {
		t.Fatalf("期望服务端被调用 2 轮，实际 %d", round)
	}

	// 最终历史里最后一条应是 assistant 的最终文字回复。
	last := out[len(out)-1]
	if last.Role != llm.RoleAssistant || last.Content != "完成了" {
		t.Fatalf("最终消息不对: role=%q content=%q", last.Role, last.Content)
	}
	if len(ui.assistant) == 0 || ui.assistant[len(ui.assistant)-1] != "完成了" {
		t.Fatalf("UI 未展示最终回复: %v", ui.assistant)
	}
	if len(ui.confirmed) != 1 || ui.confirmed[0] != "echo" {
		t.Fatalf("期望确认过一次 echo 工具，实际: %v", ui.confirmed)
	}

	// 核心断言：第二轮请求里，assistant 的 tool_calls 与 role=tool 结果都正确回传。
	msgs, ok := round2["messages"].([]any)
	if !ok {
		t.Fatalf("第二轮请求体缺少 messages: %v", round2)
	}
	var sawAssistantToolCall, sawToolResult bool
	for _, mAny := range msgs {
		m, _ := mAny.(map[string]any)
		switch m["role"] {
		case "assistant":
			if tcs, ok := m["tool_calls"].([]any); ok && len(tcs) > 0 {
				tc := tcs[0].(map[string]any)
				fn := tc["function"].(map[string]any)
				// arguments 必须是字符串，而不是对象——OpenAI 协议的硬性约定。
				if argsStr, ok := fn["arguments"].(string); !ok || argsStr == "" {
					t.Fatalf("tool_call.arguments 应为非空字符串，实际: %#v", fn["arguments"])
				}
				if tc["id"] != "call_1" {
					t.Fatalf("tool_call.id 不对: %v", tc["id"])
				}
				sawAssistantToolCall = true
			}
		case "tool":
			if m["tool_call_id"] != "call_1" {
				t.Fatalf("tool 消息 tool_call_id 不对: %v", m["tool_call_id"])
			}
			if m["content"] != "echo: hi" {
				t.Fatalf("tool 结果内容不对: %v", m["content"])
			}
			sawToolResult = true
		}
	}
	if !sawAssistantToolCall {
		t.Fatalf("第二轮历史里没有带 tool_calls 的 assistant 消息")
	}
	if !sawToolResult {
		t.Fatalf("第二轮历史里没有 role=tool 的工具结果消息")
	}
}

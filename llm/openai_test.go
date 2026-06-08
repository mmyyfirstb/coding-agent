package llm

// openai_test.go 是「包内测试」（package llm，而非 llm_test），
// 这样才能直接调用本包里的非导出辅助函数：
//   - shouldSkipVerify
//   - toOpenAIMessages
//   - fromOpenAIChoice
// 这些纯函数承载了核心翻译逻辑，单测它们即可覆盖大部分行为。

import (
	"encoding/json"
	"testing"
)

// boolPtr 是个小工具：返回指向 b 的指针，便于构造 *bool 覆盖项。
func boolPtr(b bool) *bool { return &b }

// TestShouldSkipVerify 用表驱动覆盖 TLS 证书校验的判定规则。
func TestShouldSkipVerify(t *testing.T) {
	cases := []struct {
		name     string
		baseURL  string
		override *bool
		want     bool
	}{
		{
			name:    "纯 IP 带端口 → 跳过校验",
			baseURL: "https://203.0.113.10:443/v1",
			want:    true,
		},
		{
			name:    "纯 IP 不带端口 → 跳过校验",
			baseURL: "https://1.2.3.4/v1",
			want:    true,
		},
		{
			name:    "域名 → 正常校验（不跳过）",
			baseURL: "https://api.example.com/v1",
			want:    false,
		},
		{
			name:     "覆盖 &false 作用于 IP → 不跳过",
			baseURL:  "https://203.0.113.10:443/v1",
			override: boolPtr(false),
			want:     false,
		},
		{
			name:     "覆盖 &true 作用于域名 → 跳过",
			baseURL:  "https://api.example.com/v1",
			override: boolPtr(true),
			want:     true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := shouldSkipVerify(c.baseURL, c.override)
			if got != c.want {
				t.Fatalf("shouldSkipVerify(%q, %v) = %v, want %v",
					c.baseURL, c.override, got, c.want)
			}
		})
	}
}

// TestToOpenAIMessages_ToolCallArgumentsAreString 验证最关键的翻译约定：
//   - assistant 工具调用的 arguments 序列化后必须是「字符串」而非「对象」；
//   - role=tool 的消息必须带上 tool_call_id。
func TestToOpenAIMessages_ToolCallArgumentsAreString(t *testing.T) {
	// 构造一段中立消息：用户提问 → assistant 发起工具调用 → 工具回填结果。
	msgs := []Message{
		{Role: RoleUser, Content: "读一下 main.go"},
		{
			Role:    RoleAssistant,
			Content: "", // 仅发起调用，无文本
			ToolCalls: []ToolCall{
				{
					ID:   "call_1",
					Name: "read_file",
					Args: json.RawMessage(`{"path":"main.go"}`),
				},
			},
		},
		ToolResult("call_1", "package main", false),
	}

	oa := toOpenAIMessages(msgs)
	if len(oa) != 3 {
		t.Fatalf("翻译后消息数 = %d, want 3", len(oa))
	}

	// 序列化整体，再用泛型 map 检查实际 JSON 形状（避免被结构体类型蒙蔽）。
	raw, err := json.Marshal(oa)
	if err != nil {
		t.Fatalf("marshal 失败: %v", err)
	}

	var generic []map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("反序列化为泛型失败: %v", err)
	}

	// --- 检查第二条：assistant 工具调用 ---
	assistant := generic[1]
	toolCalls, ok := assistant["tool_calls"].([]any)
	if !ok || len(toolCalls) != 1 {
		t.Fatalf("assistant.tool_calls 形状不对: %#v", assistant["tool_calls"])
	}
	fn, ok := toolCalls[0].(map[string]any)["function"].(map[string]any)
	if !ok {
		t.Fatalf("tool_call.function 形状不对: %#v", toolCalls[0])
	}

	// 核心断言：arguments 必须是字符串类型。
	argsVal, ok := fn["arguments"]
	if !ok {
		t.Fatalf("function.arguments 缺失")
	}
	argStr, isString := argsVal.(string)
	if !isString {
		t.Fatalf("function.arguments 应为 string，实际类型 %T (值 %#v)", argsVal, argsVal)
	}
	if argStr != `{"path":"main.go"}` {
		t.Fatalf("function.arguments 内容 = %q, want %q", argStr, `{"path":"main.go"}`)
	}

	// --- 检查第三条：role=tool 必须带 tool_call_id ---
	toolMsg := generic[2]
	if toolMsg["role"] != RoleTool {
		t.Fatalf("第三条 role = %v, want %q", toolMsg["role"], RoleTool)
	}
	if toolMsg["tool_call_id"] != "call_1" {
		t.Fatalf("tool 消息缺少正确的 tool_call_id: %#v", toolMsg["tool_call_id"])
	}
}

// TestFromOpenAIChoice_ToolCalls 验证 OpenAI choice → 中立 Response 的翻译：
//   - arguments 字符串能原样还原成中立 ToolCall.Args；
//   - 含工具调用时 StopReason 归一化为 "tool_calls"。
func TestFromOpenAIChoice_ToolCalls(t *testing.T) {
	choice := openAIChoice{
		Message: openAIMessage{
			Role:    RoleAssistant,
			Content: "我来读取文件",
			ToolCalls: []openAIToolCall{
				{
					ID:   "call_42",
					Type: "function",
					Function: openAIToolCallFunc{
						Name:      "read_file",
						Arguments: `{"path":"a.go"}`, // 协议里 arguments 是字符串
					},
				},
			},
		},
		// 即便服务端给的是别的，有 tool_calls 时也应被覆盖为 "tool_calls"。
		FinishReason: "stop",
	}

	resp := fromOpenAIChoice(choice)

	if resp.Message.Role != RoleAssistant {
		t.Fatalf("Message.Role = %q, want %q", resp.Message.Role, RoleAssistant)
	}
	if resp.Message.Content != "我来读取文件" {
		t.Fatalf("Message.Content = %q", resp.Message.Content)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("ToolCalls 数量 = %d, want 1", len(resp.Message.ToolCalls))
	}

	tc := resp.Message.ToolCalls[0]
	if tc.ID != "call_42" || tc.Name != "read_file" {
		t.Fatalf("ToolCall 基础字段不对: %+v", tc)
	}

	// Args 应能作为合法 JSON 往返解析回原始 map。
	var got map[string]string
	if err := json.Unmarshal(tc.Args, &got); err != nil {
		t.Fatalf("ToolCall.Args 不是合法 JSON: %v (raw=%s)", err, string(tc.Args))
	}
	if got["path"] != "a.go" {
		t.Fatalf("ToolCall.Args 往返后内容不对: %#v", got)
	}

	if resp.StopReason != "tool_calls" {
		t.Fatalf("StopReason = %q, want %q", resp.StopReason, "tool_calls")
	}
}

// TestFromOpenAIChoice_PlainText 补充覆盖「无工具调用」时的 StopReason 取值。
func TestFromOpenAIChoice_PlainText(t *testing.T) {
	// 有 finish_reason → 原样采用。
	r1 := fromOpenAIChoice(openAIChoice{
		Message:      openAIMessage{Role: RoleAssistant, Content: "你好"},
		FinishReason: "length",
	})
	if r1.StopReason != "length" {
		t.Fatalf("StopReason = %q, want %q", r1.StopReason, "length")
	}
	if len(r1.Message.ToolCalls) != 0 {
		t.Fatalf("不应有 ToolCalls: %+v", r1.Message.ToolCalls)
	}

	// finish_reason 为空 → 兜底为 "stop"。
	r2 := fromOpenAIChoice(openAIChoice{
		Message: openAIMessage{Role: RoleAssistant, Content: "你好"},
	})
	if r2.StopReason != "stop" {
		t.Fatalf("空 finish_reason 应兜底为 stop，实际 %q", r2.StopReason)
	}
}

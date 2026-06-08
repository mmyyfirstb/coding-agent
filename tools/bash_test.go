package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// argsOf 是个小helper：把 command 拼成符合 Spec 的参数 JSON。
func argsOf(command string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"command": command})
	return b
}

// TestBashEcho 验证最基本的正常路径：执行成功、能拿到 stdout、无 error。
func TestBashEcho(t *testing.T) {
	out, err := Bash{}.Run(context.Background(), argsOf("echo hi"))
	if err != nil {
		t.Fatalf("期望 err==nil，实际为 %v", err)
	}
	if !strings.Contains(out, "hi") {
		t.Fatalf("期望输出包含 \"hi\"，实际为 %q", out)
	}
}

// TestBashNonZeroExit 验证"非零退出码"被当作正常结果处理：
// error 应为 nil，且输出里带上 [退出码 7]。
func TestBashNonZeroExit(t *testing.T) {
	out, err := Bash{}.Run(context.Background(), argsOf("exit 7"))
	if err != nil {
		t.Fatalf("非零退出不应被当作工具失败，期望 err==nil，实际为 %v", err)
	}
	if !strings.Contains(out, "[退出码 7]") {
		t.Fatalf("期望输出包含 \"[退出码 7]\"，实际为 %q", out)
	}
}

// TestBashEmptyCommand 验证空命令被拒绝：应返回非 nil 的 error。
func TestBashEmptyCommand(t *testing.T) {
	_, err := Bash{}.Run(context.Background(), argsOf("   "))
	if err == nil {
		t.Fatalf("空命令应返回 error，实际为 nil")
	}
}

// TestBashNoOutput 验证"成功但无输出"会得到占位文本。
func TestBashNoOutput(t *testing.T) {
	out, err := Bash{}.Run(context.Background(), argsOf("true"))
	if err != nil {
		t.Fatalf("期望 err==nil，实际为 %v", err)
	}
	if out != "（无输出）" {
		t.Fatalf("期望输出为 \"（无输出）\"，实际为 %q", out)
	}
}

// TestBashBadArgs 额外覆盖参数解析失败的分支。
func TestBashBadArgs(t *testing.T) {
	_, err := Bash{}.Run(context.Background(), json.RawMessage(`{not json}`))
	if err == nil {
		t.Fatalf("非法 JSON 应返回 error，实际为 nil")
	}
}

// TestBashSpec 校验 Spec 的关键字段，避免名字/必填项写错。
func TestBashSpec(t *testing.T) {
	spec := Bash{}.Spec()
	if spec.Name != "bash" {
		t.Fatalf("期望 Name==\"bash\"，实际为 %q", spec.Name)
	}
	// 参数 schema 应是合法 JSON，且把 command 标为 required。
	var schema struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(spec.Parameters, &schema); err != nil {
		t.Fatalf("Parameters 不是合法 JSON: %v", err)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "command" {
		t.Fatalf("期望 required==[\"command\"]，实际为 %v", schema.Required)
	}
}

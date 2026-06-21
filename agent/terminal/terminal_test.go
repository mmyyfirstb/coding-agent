package terminal

// TerminalUI 的行为测试，分两部分：
//   - 非 raw（行模式）的渲染：sink（思考 / 正文）、工具确认、命令输出各自带上约定的
//     标签与颜色转义，这是「回看屏幕能分清谁说的话」的基础；
//   - raw 模式特有行为：rune/宽度感知行编辑（中文退格）与 ESC 打断。
//
// 全程用注入的 bytes.Buffer / bytes.Reader 作 in/out，不碰真实终端。

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"zsh-agent/agent"
)

// 先思考后正文：暗灰「思考」块 + 换行收尾，再接绿色「助手」正文。
func TestTerminalSink_ReasoningThenContent(t *testing.T) {
	var out bytes.Buffer
	s := NewTerminalUI(strings.NewReader(""), &out).Sink()

	// 模拟流式：增量一片片来。
	s.OnReasoning("想")
	s.OnReasoning("一下")
	s.OnContent("你")
	s.OnContent("好")
	s.Close()

	want := "\033[2m思考 想一下\033[0m\n\033[1;32m助手\033[0m 你好\n"
	if got := out.String(); got != want {
		t.Fatalf("sink 渲染不对：\n got=%q\nwant=%q", got, want)
	}
}

// 只有正文（无思考）：直接绿色「助手」标签 + 正文 + 换行。
func TestTerminalSink_ContentOnly(t *testing.T) {
	var out bytes.Buffer
	s := NewTerminalUI(strings.NewReader(""), &out).Sink()

	s.OnContent("你好")
	s.Close()

	want := "\033[1;32m助手\033[0m 你好\n"
	if got := out.String(); got != want {
		t.Fatalf("sink 渲染不对：\n got=%q\nwant=%q", got, want)
	}
}

// 一个字都没输出（模型只发了工具调用）：sink 不打印任何东西。
func TestTerminalSink_Empty(t *testing.T) {
	var out bytes.Buffer
	s := NewTerminalUI(strings.NewReader(""), &out).Sink()

	s.Close()

	if got := out.String(); got != "" {
		t.Fatalf("空回合 sink 不应输出，实际 %q", got)
	}
}

// 命令输出应整体被暗灰包裹；不以换行结尾时补一个换行（且在重置之后）。
func TestTerminalUI_ToolOutput(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"无尾换行要补一个", "file.go", "\033[2mfile.go\033[0m\n"},
		{"已有尾换行不重复补", "file.go\n", "\033[2mfile.go\n\033[0m"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			ui := NewTerminalUI(strings.NewReader(""), &out)

			ui.ToolOutput(c.in)

			if got := out.String(); got != c.want {
				t.Fatalf("ToolOutput 渲染不对：\n got=%q\nwant=%q", got, c.want)
			}
		})
	}
}

// 工具确认行应黄色、缩进、带工具名与参数；直接回车视为同意。
func TestTerminalUI_ConfirmTool_DefaultYes(t *testing.T) {
	var out bytes.Buffer
	ui := NewTerminalUI(strings.NewReader("\n"), &out) // 直接回车

	if !ui.ConfirmTool("bash", `{"command":"ls"}`) {
		t.Fatal("直接回车应视为同意（返回 true）")
	}
	s := out.String()
	if !strings.Contains(s, "\033[33m▶ bash {\"command\":\"ls\"}\033[0m") {
		t.Fatalf("确认行缺少黄色工具名/参数：%q", s)
	}
	if !strings.Contains(s, "  执行？[Y/n] ") {
		t.Fatalf("确认提示缺少缩进：%q", s)
	}
}

// 明确输入 n 应判为拒绝，并打印缩进的「（已拒绝）」。
func TestTerminalUI_ConfirmTool_No(t *testing.T) {
	var out bytes.Buffer
	ui := NewTerminalUI(strings.NewReader("n\n"), &out)

	if ui.ConfirmTool("bash", "{}") {
		t.Fatal("输入 n 应判为拒绝（返回 false）")
	}
	if !strings.Contains(out.String(), "  （已拒绝）") {
		t.Fatalf("拒绝提示缺少缩进：%q", out.String())
	}
}

// raw 模式下中文应能被退格正确删除（核心 bug 修复的验证）。
func TestRawTerminalUI_ReadLineChineseBackspace(t *testing.T) {
	// 「中文」+ 退格(0x7f) + 回车 → "中"
	in := bytes.NewReader([]byte("中文\x7f\r"))
	ui := NewRawTerminalUI(in, io.Discard)
	line, oc := ui.ReadLine("> ")
	if line != "中" || oc != agent.OutcomeSubmit {
		t.Fatalf("得到 (%q,%d)，期望 (\"中\",Submit)", line, oc)
	}
}

// ESC 应触发 WatchInterrupt 的 cancel。
func TestRawTerminalUI_WatchInterrupt(t *testing.T) {
	in := bytes.NewReader([]byte{0x1b}) // ESC，随后流结束
	ui := NewRawTerminalUI(in, io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	stop := ui.WatchInterrupt(cancel)
	defer stop()
	select {
	case <-ctx.Done(): // 期望被打断
	case <-time.After(2 * time.Second):
		t.Fatal("ESC 未触发 cancel")
	}
}

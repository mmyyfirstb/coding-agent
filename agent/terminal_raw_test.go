package agent

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"
)

// raw 模式下中文应能被退格正确删除（核心 bug 修复的验证）。
func TestRawTerminalUI_ReadLineChineseBackspace(t *testing.T) {
	// 「中文」+ 退格(0x7f) + 回车 → "中"
	in := bytes.NewReader([]byte("中文\x7f\r"))
	ui := NewRawTerminalUI(in, io.Discard)
	line, oc := ui.ReadLine("> ")
	if line != "中" || oc != OutcomeSubmit {
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

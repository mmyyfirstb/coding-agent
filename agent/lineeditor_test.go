package agent

import (
	"io"
	"testing"
)

// feed 把一串 Key 塞进 channel 并关闭，供 ReadLine 消费。
func feed(ks ...Key) <-chan Key {
	ch := make(chan Key, len(ks))
	for _, k := range ks {
		ch <- k
	}
	close(ch)
	return ch
}

func TestReadLine_ChineseBackspace(t *testing.T) {
	// 输入「中文」，退格一次删掉整个「文」，回车 → "中"。
	keys := feed(
		Key{Kind: KeyRune, Rune: '中'},
		Key{Kind: KeyRune, Rune: '文'},
		Key{Kind: KeyBackspace},
		Key{Kind: KeyEnter},
	)
	line, oc := ReadLine(keys, io.Discard, "> ", false)
	if line != "中" || oc != OutcomeSubmit {
		t.Fatalf("得到 (%q,%d)，期望 (\"中\",Submit)", line, oc)
	}
}

func TestReadLine_CursorInsert(t *testing.T) {
	// 「中文」，左移一次，插入 x → "中x文"。
	keys := feed(
		Key{Kind: KeyRune, Rune: '中'},
		Key{Kind: KeyRune, Rune: '文'},
		Key{Kind: KeyLeft},
		Key{Kind: KeyRune, Rune: 'x'},
		Key{Kind: KeyEnter},
	)
	line, _ := ReadLine(keys, io.Discard, "> ", false)
	if line != "中x文" {
		t.Fatalf("得到 %q，期望 \"中x文\"", line)
	}
}

func TestReadLine_CtrlU(t *testing.T) {
	keys := feed(
		Key{Kind: KeyRune, Rune: 'a'},
		Key{Kind: KeyRune, Rune: 'b'},
		Key{Kind: KeyCtrlU},
		Key{Kind: KeyRune, Rune: 'c'},
		Key{Kind: KeyEnter},
	)
	line, _ := ReadLine(keys, io.Discard, "> ", false)
	if line != "c" {
		t.Fatalf("得到 %q，期望 \"c\"", line)
	}
}

func TestReadLine_CtrlDEmpty(t *testing.T) {
	_, oc := ReadLine(feed(Key{Kind: KeyCtrlD}), io.Discard, "> ", false)
	if oc != OutcomeEOF {
		t.Fatalf("空行 Ctrl-D 应 OutcomeEOF，得到 %d", oc)
	}
}

func TestReadLine_CtrlCInterrupt(t *testing.T) {
	_, oc := ReadLine(feed(Key{Kind: KeyRune, Rune: 'a'}, Key{Kind: KeyCtrlC}), io.Discard, "> ", false)
	if oc != OutcomeInterrupt {
		t.Fatalf("Ctrl-C 应 OutcomeInterrupt，得到 %d", oc)
	}
}

// TestReadLine_EscCancels 验证 escCancels=true 时 ESC 返回 OutcomeCancel（工具确认场景）。
func TestReadLine_EscCancels(t *testing.T) {
	keys := feed(
		Key{Kind: KeyRune, Rune: 'a'},
		Key{Kind: KeyEsc},
	)
	line, oc := ReadLine(keys, io.Discard, "执行？[Y/n] ", true)
	if oc != OutcomeCancel {
		t.Fatalf("escCancels=true 时 ESC 应 OutcomeCancel，得到 oc=%d line=%q", oc, line)
	}
}

// TestReadLine_EscClearsLine 验证 escCancels=false 时 ESC 仅清空当前行，继续编辑（主 REPL 场景）。
func TestReadLine_EscClearsLine(t *testing.T) {
	// 输入 'a'，ESC 清空，再输入 'b'，回车 → 应返回 "b"（不是 "ab"，也不是 OutcomeCancel）。
	keys := feed(
		Key{Kind: KeyRune, Rune: 'a'},
		Key{Kind: KeyEsc},
		Key{Kind: KeyRune, Rune: 'b'},
		Key{Kind: KeyEnter},
	)
	line, oc := ReadLine(keys, io.Discard, "> ", false)
	if line != "b" || oc != OutcomeSubmit {
		t.Fatalf("escCancels=false 时 ESC 应清空行继续编辑，得到 (%q,%d)，期望 (\"b\",Submit)", line, oc)
	}
}

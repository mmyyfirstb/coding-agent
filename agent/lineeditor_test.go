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
	line, oc := ReadLine(keys, io.Discard, "> ")
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
	line, _ := ReadLine(keys, io.Discard, "> ")
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
	line, _ := ReadLine(keys, io.Discard, "> ")
	if line != "c" {
		t.Fatalf("得到 %q，期望 \"c\"", line)
	}
}

func TestReadLine_CtrlDEmpty(t *testing.T) {
	_, oc := ReadLine(feed(Key{Kind: KeyCtrlD}), io.Discard, "> ")
	if oc != OutcomeEOF {
		t.Fatalf("空行 Ctrl-D 应 OutcomeEOF，得到 %d", oc)
	}
}

func TestReadLine_CtrlCInterrupt(t *testing.T) {
	_, oc := ReadLine(feed(Key{Kind: KeyRune, Rune: 'a'}, Key{Kind: KeyCtrlC}), io.Discard, "> ")
	if oc != OutcomeInterrupt {
		t.Fatalf("Ctrl-C 应 OutcomeInterrupt，得到 %d", oc)
	}
}

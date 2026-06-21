package agent

import (
	"bytes"
	"testing"
)

// collectKeys 收集 pump 在给定字节流上产出的全部 Key（流结束即停）。
func collectKeys(b []byte) []Key {
	ch := make(chan Key, 64)
	go pump(bytes.NewReader(b), ch)
	var ks []Key
	for k := range ch {
		ks = append(ks, k)
	}
	return ks
}

func TestPump_UTF8AndEnter(t *testing.T) {
	ks := collectKeys([]byte("中a\r"))
	want := []Key{
		{Kind: KeyRune, Rune: '中'},
		{Kind: KeyRune, Rune: 'a'},
		{Kind: KeyEnter},
	}
	if len(ks) != len(want) {
		t.Fatalf("Key 数=%d，期望 %d：%+v", len(ks), len(want), ks)
	}
	for i := range want {
		if ks[i] != want[i] {
			t.Errorf("第 %d 个=%+v，期望 %+v", i, ks[i], want[i])
		}
	}
}

func TestPump_ArrowKeys(t *testing.T) {
	ks := collectKeys([]byte{0x1b, '[', 'D', 0x1b, '[', 'C'})
	if len(ks) != 2 || ks[0].Kind != KeyLeft || ks[1].Kind != KeyRight {
		t.Fatalf("方向键解码错误：%+v", ks)
	}
}

func TestPump_BareEscAtEnd(t *testing.T) {
	ks := collectKeys([]byte{0x1b}) // 单独 ESC，随后流结束
	if len(ks) != 1 || ks[0].Kind != KeyEsc {
		t.Fatalf("裸 ESC 解码错误：%+v", ks)
	}
}

func TestPump_ControlBytes(t *testing.T) {
	ks := collectKeys([]byte{0x7f, 0x15, 0x17, 0x03, 0x04})
	want := []KeyKind{KeyBackspace, KeyCtrlU, KeyCtrlW, KeyCtrlC, KeyCtrlD}
	if len(ks) != len(want) {
		t.Fatalf("Key 数=%d，期望 %d：%+v", len(ks), len(want), ks)
	}
	for i := range want {
		if ks[i].Kind != want[i] {
			t.Errorf("第 %d 个 Kind=%d，期望 %d", i, ks[i].Kind, want[i])
		}
	}
}

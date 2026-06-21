package agent

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 短输出（未超上限）应原样返回，不加任何标记。
func TestClampToolOutputShort(t *testing.T) {
	in := "hello world"
	got := clampToolOutput(in, 100)
	if got != in {
		t.Fatalf("短输出应原样返回\n want: %q\n got:  %q", in, got)
	}
}

// 恰好等于上限的输出也应原样返回（边界）。
func TestClampToolOutputExactlyAtLimit(t *testing.T) {
	in := strings.Repeat("x", 50)
	got := clampToolOutput(in, 50)
	if got != in {
		t.Fatalf("恰好等于上限应原样返回，但被改动了")
	}
}

// 超长输出：保留头尾、中间省略，并注明省略了多少字符。
func TestClampToolOutputTruncatesKeepingHeadAndTail(t *testing.T) {
	head := strings.Repeat("A", 100)
	mid := strings.Repeat("B", 1000)
	tail := strings.Repeat("C", 100)
	in := head + mid + tail // 共 1200 字符

	got := clampToolOutput(in, 200) // 头尾各留 100

	if utf8.RuneCountInString(got) >= utf8.RuneCountInString(in) {
		t.Fatalf("截断后应更短，got 长度=%d, in 长度=%d",
			utf8.RuneCountInString(got), utf8.RuneCountInString(in))
	}
	if !strings.HasPrefix(got, head) {
		t.Errorf("应保留开头 100 个 A")
	}
	if !strings.HasSuffix(got, tail) {
		t.Errorf("应保留结尾 100 个 C")
	}
	if strings.Contains(got, "B") {
		t.Errorf("中间的 B 应被省略，却出现在结果里")
	}
	// 省略数量 = 1200 - 200 = 1000，应在标记里体现。
	if !strings.Contains(got, "1000") {
		t.Errorf("省略标记里应注明省略了 1000 个字符，got=%q", got)
	}
}

// 在多字节字符（中文）中间截断时，结果必须仍是合法 UTF-8，不能切碎字符。
func TestClampToolOutputPreservesUTF8(t *testing.T) {
	in := strings.Repeat("中", 1000)
	got := clampToolOutput(in, 10)
	if !utf8.ValidString(got) {
		t.Fatalf("截断后必须是合法 UTF-8，不能把中文字符切碎")
	}
}

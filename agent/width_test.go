package agent

import "testing"

func TestRuneWidth(t *testing.T) {
	cases := []struct {
		name string
		r    rune
		want int
	}{
		{"ascii 字母", 'a', 1},
		{"数字", '7', 1},
		{"中文", '中', 2},
		{"全角叹号", '！', 2}, // U+FF01
		{"组合附加符", 0x0301, 0},
		{"零宽空格", 0x200B, 0},
		{"emoji", 0x1F600, 2},
		{"换行控制字符", '\n', 0},
	}
	for _, c := range cases {
		if got := runeWidth(c.r); got != c.want {
			t.Errorf("%s runeWidth(%U)=%d, 期望 %d", c.name, c.r, got, c.want)
		}
	}
}

func TestDisplayWidth_SkipsANSI(t *testing.T) {
	// "你 ›" = 2+1+1 = 4 列；ANSI 颜色码零宽。
	s := "\033[36m你 ›\033[0m"
	if got := displayWidth(s); got != 4 {
		t.Fatalf("displayWidth=%d，期望 4", got)
	}
}

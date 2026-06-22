package terminal

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"zsh-agent/agent"
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
	line, oc := ReadLine(keys, io.Discard, "> ", false, 200)
	if line != "中" || oc != agent.OutcomeSubmit {
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
	line, _ := ReadLine(keys, io.Discard, "> ", false, 200)
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
	line, _ := ReadLine(keys, io.Discard, "> ", false, 200)
	if line != "c" {
		t.Fatalf("得到 %q，期望 \"c\"", line)
	}
}

func TestReadLine_CtrlDEmpty(t *testing.T) {
	_, oc := ReadLine(feed(Key{Kind: KeyCtrlD}), io.Discard, "> ", false, 200)
	if oc != agent.OutcomeEOF {
		t.Fatalf("空行 Ctrl-D 应 agent.OutcomeEOF，得到 %d", oc)
	}
}

func TestReadLine_CtrlCInterrupt(t *testing.T) {
	_, oc := ReadLine(feed(Key{Kind: KeyRune, Rune: 'a'}, Key{Kind: KeyCtrlC}), io.Discard, "> ", false, 200)
	if oc != agent.OutcomeInterrupt {
		t.Fatalf("Ctrl-C 应 agent.OutcomeInterrupt，得到 %d", oc)
	}
}

// TestReadLine_EscCancels 验证 escCancels=true 时 ESC 返回 agent.OutcomeCancel（工具确认场景）。
func TestReadLine_EscCancels(t *testing.T) {
	keys := feed(
		Key{Kind: KeyRune, Rune: 'a'},
		Key{Kind: KeyEsc},
	)
	line, oc := ReadLine(keys, io.Discard, "执行？[Y/n] ", true, 200)
	if oc != agent.OutcomeCancel {
		t.Fatalf("escCancels=true 时 ESC 应 agent.OutcomeCancel，得到 oc=%d line=%q", oc, line)
	}
}

// TestReadLine_EscClearsLine 验证 escCancels=false 时 ESC 仅清空当前行，继续编辑（主 REPL 场景）。
func TestReadLine_EscClearsLine(t *testing.T) {
	// 输入 'a'，ESC 清空，再输入 'b'，回车 → 应返回 "b"（不是 "ab"，也不是 agent.OutcomeCancel）。
	keys := feed(
		Key{Kind: KeyRune, Rune: 'a'},
		Key{Kind: KeyEsc},
		Key{Kind: KeyRune, Rune: 'b'},
		Key{Kind: KeyEnter},
	)
	line, oc := ReadLine(keys, io.Discard, "> ", false, 200)
	if line != "b" || oc != agent.OutcomeSubmit {
		t.Fatalf("escCancels=false 时 ESC 应清空行继续编辑，得到 (%q,%d)，期望 (\"b\",Submit)", line, oc)
	}
}

// vt 是一个极简终端模拟器，只实现行编辑重绘用到的少量转义序列，
// 把 ReadLine 输出的字节流"渲染"成屏幕网格，从而验证折行时不出现重影。
// 支持：可打印字符（按显示宽度自动折行）、\r、\n、CSI nA/nB/nC、CSI [n]K（擦到行尾）。
type vt struct {
	cols int
	rows [][]rune // 每行 cols 个单元，' ' 为空；宽字符第二格存 0 占位
	row  int
	col  int
}

func (v *vt) ensure(r int) {
	for len(v.rows) <= r {
		row := make([]rune, v.cols)
		for i := range row {
			row[i] = ' '
		}
		v.rows = append(v.rows, row)
	}
}

func (v *vt) put(r rune) {
	w := runeWidth(r)
	if w == 0 {
		return // 组合 / 零宽字符不参与本测试
	}
	if v.col+w > v.cols {
		v.row++
		v.col = 0
	}
	v.ensure(v.row)
	v.rows[v.row][v.col] = r
	if w == 2 && v.col+1 < v.cols {
		v.rows[v.row][v.col+1] = 0
	}
	v.col += w
}

// feed 把一段输出字节喂给模拟器。
func (v *vt) feed(s string) {
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		switch r := rs[i]; {
		case r == '\r':
			v.col = 0
		case r == '\n':
			v.row++
			v.ensure(v.row)
		case r == 0x1b && i+1 < len(rs) && rs[i+1] == '[':
			i += 2
			num, hasNum := 0, false
			for i < len(rs) && rs[i] >= '0' && rs[i] <= '9' {
				num, hasNum = num*10+int(rs[i]-'0'), true
				i++
			}
			if i >= len(rs) {
				return
			}
			n := num
			if !hasNum {
				n = 1
			}
			switch rs[i] {
			case 'A':
				if v.row -= n; v.row < 0 {
					v.row = 0
				}
			case 'B':
				v.row += n
				v.ensure(v.row)
			case 'C':
				if v.col += n; v.col > v.cols {
					v.col = v.cols
				}
			case 'K': // 默认参数 0：从光标擦到行尾
				v.ensure(v.row)
				for c := v.col; c < v.cols; c++ {
					v.rows[v.row][c] = ' '
				}
			}
		default:
			v.put(r)
		}
	}
}

// text 把整屏可见字符按行优先拼接（丢弃空格与占位符）。
func (v *vt) text() string {
	var b strings.Builder
	for _, row := range v.rows {
		for _, r := range row {
			if r != 0 && r != ' ' {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// TestReadLine_WrapNoGhost 复现并守护「换行后不停刷旧行」的 bug：
// 在窄终端里输入超过一行宽度的中文，重绘必须回到首行重排，整屏只应有一份内容。
func TestReadLine_WrapNoGhost(t *testing.T) {
	const cols = 10
	const prompt = ">>"             // 宽度 2，无内部空格，便于断言
	content := []rune("一二三四五六七八九十") // 10 个中文，宽 20

	ks := make([]Key, 0, len(content)+1)
	for _, r := range content {
		ks = append(ks, Key{Kind: KeyRune, Rune: r})
	}
	ks = append(ks, Key{Kind: KeyEnter})

	var out bytes.Buffer
	line, oc := ReadLine(feed(ks...), &out, prompt, false, cols)
	if line != string(content) || oc != agent.OutcomeSubmit {
		t.Fatalf("得到 (%q,%d)，期望 (%q,Submit)", line, oc, string(content))
	}

	screen := &vt{cols: cols}
	screen.feed(out.String())
	want := prompt + string(content) // 整屏应恰好一份提示符 + 内容
	if got := screen.text(); got != want {
		t.Fatalf("折行后出现重影：\n 屏幕=%q\n 期望=%q", got, want)
	}
}

// curOf 把一段输出喂给 vt，返回喂完后光标停在第几行第几列。
func curOf(cols int, s string) (row, col int) {
	v := &vt{cols: cols}
	v.feed(s)
	return v.row, v.col
}

// TestReadLine_WrapOddPromptCursor 复现「奇数宽提示符 + 中文折行」的光标错位 bug：
// 真实提示符「你 › 」显示宽 5（奇数），其后跟宽度 2 的汉字时，某个汉字会在折行边界
// 被整体挤到下一行、上一行末尾留 1 列空白。若重绘仍用线性 /cols 估算行列（忽略这段留白），
// 光标列就会偏 1，落进某个汉字的第二格——肉眼看上去就是「光标 / 重打的提示符嵌进汉字中间」。
// 这里不按回车，直接比对 ReadLine 输出留下的光标落点与「裸提示符+内容」的真实落点。
func TestReadLine_WrapOddPromptCursor(t *testing.T) {
	const cols = 10
	const prompt = ">>>>>"          // 宽度 5（奇数），无内部空格便于断言
	content := []rune("一二三四五六七八九十") // 10 个中文，宽 20

	ks := make([]Key, 0, len(content))
	for _, r := range content {
		ks = append(ks, Key{Kind: KeyRune, Rune: r})
	}

	var out bytes.Buffer
	if line, _ := ReadLine(feed(ks...), &out, prompt, false, cols); line != string(content) {
		t.Fatalf("得到 %q，期望 %q", line, string(content))
	}

	// 基准：裸提示符 + 内容按终端折行后，光标本应停在内容末尾的真实落点。
	wantRow, wantCol := curOf(cols, prompt+string(content))
	gotRow, gotCol := curOf(cols, out.String())
	if gotRow != wantRow || gotCol != wantCol {
		t.Fatalf("折行后光标错位：得到 (行%d,列%d)，期望 (行%d,列%d)", gotRow, gotCol, wantRow, wantCol)
	}
}

// TestReadLine_WrapShrinkClears 守护反向场景：内容从多行退格缩回一行时，
// 必须把多出来的旧行清干净，屏上不能残留被删掉的尾部字符。
func TestReadLine_WrapShrinkClears(t *testing.T) {
	const cols = 10
	const prompt = ">>"

	ks := make([]Key, 0, 24)
	for _, r := range "一二三四五六七八九十" { // 先填满 3 行
		ks = append(ks, Key{Kind: KeyRune, Rune: r})
	}
	for i := 0; i < 7; i++ { // 退 7 个，剩 "一二三"
		ks = append(ks, Key{Kind: KeyBackspace})
	}
	ks = append(ks, Key{Kind: KeyEnter})

	var out bytes.Buffer
	line, _ := ReadLine(feed(ks...), &out, prompt, false, cols)
	if line != "一二三" {
		t.Fatalf("得到 %q，期望 \"一二三\"", line)
	}

	screen := &vt{cols: cols}
	screen.feed(out.String())
	if got := screen.text(); got != prompt+"一二三" {
		t.Fatalf("退格缩行后旧行未清干净：\n 屏幕=%q\n 期望=%q", got, prompt+"一二三")
	}
}

# raw 模式输入 + rune/宽度感知行编辑 + ESC 打断 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让中文在提示符能用退格正确删除，并支持生成/工具执行中按 ESC/Ctrl-C 打断、回到提示符随时重输。

**Architecture:** 进 raw 模式接管输入，自写「按 rune、按显示宽度」的行编辑器。单 `pump` goroutine 独占读 stdin、解码成 `Key` 推进 channel；`ReadLine` / `ConfirmTool` / `WatchInterrupt` 三个消费者经该 channel **串行**用键，互不抢。打断走 ctx 取消（HTTP 与 `CommandContext` 已就绪），整回合回滚到发问之前。

**Tech Stack:** Go 1.24，纯标准库；raw 模式用 `stty`（zero-dep），宽度用自写 CJK 区间表。

**设计文档：** `docs/superpowers/specs/2026-06-21-rawmode-input-esc-design.md`

## Global Constraints

- **纯 Go 标准库、零第三方依赖**（raw 模式用 `os/exec` 调 `stty`，宽度自写表）。
- 一文件一概念；**重中文注释**；与现有代码风格一致。
- 公开仓库：代码/注释/测试里**禁止**出现真实 key、IP、域名；用占位符或 RFC 5737 文档 IP。
- 每个任务结束前 `go build ./...`、`go vet ./...`、`go test ./...`、`gofmt -l .`（应无输出）全部干净。
- 每条 commit 信息末尾附一行：`Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`。
- 当前分支 `feat/rawmode-input-esc`（已含设计文档提交）。

---

### Task 1: 显示宽度计算（width.go）

**Files:**
- Create: `agent/width.go`
- Test: `agent/width_test.go`

**Interfaces:**
- Produces: `runeWidth(r rune) int`、`runesWidth(rs []rune) int`、`displayWidth(s string) int`（包内非导出，供 lineeditor 用）。

- [ ] **Step 1: 写失败测试** — `agent/width_test.go`

```go
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
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./agent/ -run 'TestRuneWidth|TestDisplayWidth' -v`
Expected: FAIL（`undefined: runeWidth` / `displayWidth`）

- [ ] **Step 3: 实现** — `agent/width.go`

```go
package agent

// width.go：计算字符在终端里占的「显示列数」。
// 行编辑器据此定位光标、重绘，正确处理中文等全角字符（占 2 列）。
// 表驱动、纯标准库；区间取常用区段，不追求 Unicode 完备。

// wideRanges 是「占 2 列」的码点区间（含中日韩、全角符号、常见 emoji）。
var wideRanges = [][2]rune{
	{0x1100, 0x115F},   // Hangul Jamo
	{0x2E80, 0x303E},   // CJK 部首 / 康熙 / 注音
	{0x3041, 0x33FF},   // 平假名 / 片假名 / CJK 符号
	{0x3400, 0x4DBF},   // CJK 扩展 A
	{0x4E00, 0x9FFF},   // CJK 统一表意
	{0xA000, 0xA4CF},   // 彝文
	{0xAC00, 0xD7A3},   // 谚文音节
	{0xF900, 0xFAFF},   // CJK 兼容表意
	{0xFE30, 0xFE4F},   // CJK 兼容形式
	{0xFF00, 0xFF60},   // 全角 ASCII 变体
	{0xFFE0, 0xFFE6},   // 全角符号
	{0x1F300, 0x1FAFF}, // 杂项符号 / emoji
	{0x20000, 0x3FFFD}, // CJK 扩展 B 及以上
}

// combiningRanges 是「占 0 列」的组合 / 零宽字符区间。
var combiningRanges = [][2]rune{
	{0x0300, 0x036F}, // 组合附加符号
	{0x1AB0, 0x1AFF},
	{0x1DC0, 0x1DFF},
	{0x20D0, 0x20FF},
	{0xFE20, 0xFE2F},
	{0x200B, 0x200F}, // 零宽空格 / 方向标记
	{0xFEFF, 0xFEFF}, // 零宽不换行空格 (BOM)
}

func inRanges(r rune, ranges [][2]rune) bool {
	for _, rg := range ranges {
		if r >= rg[0] && r <= rg[1] {
			return true
		}
	}
	return false
}

// runeWidth 返回 r 的显示列数：组合/零宽=0，全角=2，其余=1。
func runeWidth(r rune) int {
	if r == 0 || r < 0x20 || r == 0x7f { // NUL / 控制字符 / DEL
		return 0
	}
	if inRanges(r, combiningRanges) {
		return 0
	}
	if inRanges(r, wideRanges) {
		return 2
	}
	return 1
}

// runesWidth 是 runeWidth 对一段 rune 求和。
func runesWidth(rs []rune) int {
	w := 0
	for _, r := range rs {
		w += runeWidth(r)
	}
	return w
}

// displayWidth 计算字符串的显示宽度，跳过 ANSI 转义序列（如 \033[36m，零宽）。
func displayWidth(s string) int {
	w := 0
	inEsc := false
	for _, r := range s {
		if inEsc {
			// CSI 序列以字母（A-Z / a-z）结束。
			if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
				inEsc = false
			}
			continue
		}
		if r == 0x1b { // ESC
			inEsc = true
			continue
		}
		w += runeWidth(r)
	}
	return w
}
```

- [ ] **Step 4: 运行确认通过**

Run: `go test ./agent/ -run 'TestRuneWidth|TestDisplayWidth' -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
gofmt -w agent/width.go agent/width_test.go
git add agent/width.go agent/width_test.go
git commit -m "$(printf 'feat(agent): 显示宽度计算（CJK 全角=2 列）\n\nCo-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>')"
```

---

### Task 2: 输入 pump 与按键解码（keys.go）

**Files:**
- Create: `agent/keys.go`
- Test: `agent/keys_test.go`

**Interfaces:**
- Produces:
  - `type KeyKind int` 及常量 `KeyRune KeyEnter KeyBackspace KeyLeft KeyRight KeyHome KeyEnd KeyCtrlU KeyCtrlW KeyCtrlC KeyCtrlD KeyEsc`
  - `type Key struct { Kind KeyKind; Rune rune }`
  - `func pump(r io.Reader, out chan<- Key)` —— 持续解码，r 结束时 `close(out)`。

- [ ] **Step 1: 写失败测试** — `agent/keys_test.go`

```go
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
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./agent/ -run TestPump -v`
Expected: FAIL（`undefined: pump` / `Key` / `KeyRune` …）

- [ ] **Step 3: 实现** — `agent/keys.go`

```go
package agent

// keys.go：输入 pump —— 把原始字节流解码成中立的「按键事件」。
// 它是全程唯一读 stdin 的地方；行编辑器与打断监听都只消费它产出的 Key。
// 负责：UTF-8 多字节拼字、ESC[ 转义序列（方向键 / Home / End）、裸 ESC 判定。

import (
	"io"
	"unicode/utf8"
)

type KeyKind int

const (
	KeyRune      KeyKind = iota // 可见字符，值在 Key.Rune
	KeyEnter                    // 回车
	KeyBackspace               // 退格
	KeyLeft
	KeyRight
	KeyHome
	KeyEnd
	KeyCtrlU // 删整行
	KeyCtrlW // 删词
	KeyCtrlC
	KeyCtrlD
	KeyEsc // 裸 ESC
)

// Key 是一次解码出的按键。
type Key struct {
	Kind KeyKind
	Rune rune
}

// 读取状态：区分「拿到字节 / 超时（暂无输入）/ 流结束」。
const (
	readByteOK = iota
	readTimeout
	readEnd
)

// pump 持续从 r 读字节、解码成 Key 推进 out；r 结束（EOF/错误）时 close(out) 并返回。
//
// r 应是设了 VMIN=0/VTIME>0 的 raw tty：无输入时 Read 返回 (0,nil)，被本函数当作
// readTimeout（用于判定裸 ESC）。测试可传 bytes.Reader：到结尾返回 EOF → readEnd。
func pump(r io.Reader, out chan<- Key) {
	defer close(out)
	var buf [1]byte
	next := func() (byte, int) {
		n, err := r.Read(buf[:])
		if n > 0 {
			return buf[0], readByteOK
		}
		if err == nil {
			return 0, readTimeout
		}
		return 0, readEnd
	}
	for {
		b, st := next()
		switch st {
		case readEnd:
			return
		case readTimeout:
			continue
		}
		switch {
		case b == 0x1b: // ESC：裸 ESC 或 CSI 序列开头
			if k, ok := decodeEsc(next); ok {
				out <- k
			}
		case b == '\r', b == '\n':
			out <- Key{Kind: KeyEnter}
		case b == 0x7f, b == 0x08:
			out <- Key{Kind: KeyBackspace}
		case b == 0x15:
			out <- Key{Kind: KeyCtrlU}
		case b == 0x17:
			out <- Key{Kind: KeyCtrlW}
		case b == 0x03:
			out <- Key{Kind: KeyCtrlC}
		case b == 0x04:
			out <- Key{Kind: KeyCtrlD}
		case b == 0x01:
			out <- Key{Kind: KeyHome}
		case b == 0x05:
			out <- Key{Kind: KeyEnd}
		case b < 0x20:
			// 其它控制字符：忽略。
		case b < 0x80:
			out <- Key{Kind: KeyRune, Rune: rune(b)}
		default:
			// UTF-8 多字节：按首字节确定长度，补齐后解码。
			if r, ok := decodeUTF8(b, next); ok {
				out <- Key{Kind: KeyRune, Rune: r}
			}
		}
	}
}

// decodeEsc 在已读到 ESC 后调用：再读一字节判定。
//   - 来 '[' 或 'O' → 读 CSI 序列，映射方向键 / Home / End；
//   - 超时 / 结束 / 其它 → 判为裸 ESC。
func decodeEsc(next func() (byte, int)) (Key, bool) {
	b, st := next()
	if st != readByteOK || (b != '[' && b != 'O') {
		// 无后续（裸 ESC）。若 st==readByteOK 但非 [/O，那个字节被丢弃以保持简单
		// —— 方向键以外的转义序列本就不支持。
		return Key{Kind: KeyEsc}, true
	}
	var params []byte
	for {
		c, st := next()
		if st != readByteOK {
			return Key{}, false
		}
		if c >= 0x40 && c <= 0x7e { // CSI 终止符
			return mapCSI(c, params)
		}
		params = append(params, c) // 参数字节（数字 / ';'）
	}
}

// mapCSI 把 CSI 终止符 + 参数映射成方向 / Home / End；未识别返回 (_, false)。
func mapCSI(final byte, params []byte) (Key, bool) {
	switch final {
	case 'D':
		return Key{Kind: KeyLeft}, true
	case 'C':
		return Key{Kind: KeyRight}, true
	case 'H':
		return Key{Kind: KeyHome}, true
	case 'F':
		return Key{Kind: KeyEnd}, true
	case '~':
		switch string(params) {
		case "1", "7":
			return Key{Kind: KeyHome}, true
		case "4", "8":
			return Key{Kind: KeyEnd}, true
		}
	}
	return Key{}, false // ↑↓、Delete 等不支持
}

// decodeUTF8 在已读到一个 >=0x80 的首字节后，补齐并解码出 rune。
func decodeUTF8(first byte, next func() (byte, int)) (rune, bool) {
	var n int
	switch {
	case first&0xE0 == 0xC0:
		n = 2
	case first&0xF0 == 0xE0:
		n = 3
	case first&0xF8 == 0xF0:
		n = 4
	default:
		return 0, false // 非法首字节（孤立的延续字节等）
	}
	bs := make([]byte, 1, 4)
	bs[0] = first
	for len(bs) < n {
		c, st := next()
		if st != readByteOK {
			return 0, false
		}
		bs = append(bs, c)
	}
	r, size := utf8.DecodeRune(bs)
	if r == utf8.RuneError || size != n {
		return 0, false
	}
	return r, true
}
```

- [ ] **Step 4: 运行确认通过**

Run: `go test ./agent/ -run TestPump -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
gofmt -w agent/keys.go agent/keys_test.go
git add agent/keys.go agent/keys_test.go
git commit -m "$(printf 'feat(agent): 输入 pump，把字节流解码成按键事件\n\nCo-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>')"
```

---

### Task 3: 纯行编辑器（lineeditor.go）

**Files:**
- Create: `agent/lineeditor.go`
- Test: `agent/lineeditor_test.go`

**Interfaces:**
- Consumes: `Key`/`KeyKind`（Task 2）、`runesWidth`/`displayWidth`（Task 1）。
- Produces:
  - `type Outcome int` 及常量 `OutcomeSubmit OutcomeEOF OutcomeInterrupt`
  - `func ReadLine(keys <-chan Key, out io.Writer, prompt string) (string, Outcome)` —— prompt 为**单行**串（可含 ANSI 颜色，**不含换行**）。

- [ ] **Step 1: 写失败测试** — `agent/lineeditor_test.go`

```go
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
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./agent/ -run TestReadLine -v`
Expected: FAIL（`undefined: ReadLine` / `Outcome`）

- [ ] **Step 3: 实现** — `agent/lineeditor.go`

```go
package agent

// lineeditor.go：纯行编辑器。从「按键 channel」读 Key，维护 []rune 缓冲 + 光标，
// 把行实时重绘到 out，回车返回整行。不碰 tty、不碰 os.Stdin —— 可用喂 Key 来单测。
// 显示宽度（中文占 2 列）一律走 width.go，从根上修好「中文退格残影」。

import (
	"fmt"
	"io"
)

// Outcome 说明 ReadLine 为何结束。
type Outcome int

const (
	OutcomeSubmit    Outcome = iota // 回车提交，line 有效
	OutcomeEOF                      // 空行 Ctrl-D 或输入流关闭：请求退出
	OutcomeInterrupt                // Ctrl-C：放弃本行，重来
)

// ReadLine 从 keys 读键编辑一行，返回最终文本与结局。
//   - prompt 是「单行」提示符（可含 ANSI 颜色码，但不要含换行，否则重绘会滚屏）。
//   - 调用方负责在调用前安排好垂直间距（如先打一个换行）。
func ReadLine(keys <-chan Key, out io.Writer, prompt string) (string, Outcome) {
	var buf []rune
	cursor := 0 // 光标在 buf 中的 rune 下标，0..len(buf)
	promptW := displayWidth(prompt)

	redraw := func() {
		// 回行首 → 重打提示符 + 全部内容 → 清到行尾。
		fmt.Fprintf(out, "\r%s%s\033[K", prompt, string(buf))
		// 光标移到正确列：提示符宽 + 光标前内容宽。col==0 时只回行首
		// （CSI 0 C 会被当作 1，必须避开）。
		col := promptW + runesWidth(buf[:cursor])
		if col > 0 {
			fmt.Fprintf(out, "\r\033[%dC", col)
		} else {
			fmt.Fprint(out, "\r")
		}
	}

	redraw()
	for k := range keys {
		switch k.Kind {
		case KeyRune:
			buf = append(buf, 0)
			copy(buf[cursor+1:], buf[cursor:])
			buf[cursor] = k.Rune
			cursor++
		case KeyBackspace:
			if cursor > 0 {
				buf = append(buf[:cursor-1], buf[cursor:]...)
				cursor--
			}
		case KeyLeft:
			if cursor > 0 {
				cursor--
			}
		case KeyRight:
			if cursor < len(buf) {
				cursor++
			}
		case KeyHome:
			cursor = 0
		case KeyEnd:
			cursor = len(buf)
		case KeyCtrlU:
			buf = buf[:0]
			cursor = 0
		case KeyCtrlW:
			i := cursor
			for i > 0 && buf[i-1] == ' ' { // 先跳过紧邻的空格
				i--
			}
			for i > 0 && buf[i-1] != ' ' { // 再删到上一个空格
				i--
			}
			buf = append(buf[:i], buf[cursor:]...)
			cursor = i
		case KeyEsc:
			buf = buf[:0] // 裸 ESC = 清空当前行
			cursor = 0
		case KeyEnter:
			fmt.Fprint(out, "\r\n")
			return string(buf), OutcomeSubmit
		case KeyCtrlC:
			fmt.Fprint(out, "\r\n")
			return "", OutcomeInterrupt
		case KeyCtrlD:
			if len(buf) == 0 {
				fmt.Fprint(out, "\r\n")
				return "", OutcomeEOF
			}
			continue // 非空行忽略 Ctrl-D，且不必重绘
		}
		redraw()
	}
	// keys 关闭（输入流结束）→ 等价 EOF。
	return string(buf), OutcomeEOF
}
```

- [ ] **Step 4: 运行确认通过**

Run: `go test ./agent/ -run TestReadLine -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
gofmt -w agent/lineeditor.go agent/lineeditor_test.go
git add agent/lineeditor.go agent/lineeditor_test.go
git commit -m "$(printf 'feat(agent): rune/宽度感知的纯行编辑器\n\nCo-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>')"
```

---

### Task 4: raw 模式开关（rawmode.go）

**Files:**
- Create: `agent/rawmode.go`

**Interfaces:**
- Produces: `func EnableRaw() (restore func(), enabled bool)` —— 导出，供 `main` 调用。

> **说明：** 这是唯一直接操作真实终端的「脏」层，自动化测试会真切换开发者终端、留下信号 goroutine，故**不写单测**，靠 `go build`/`go vet` + 手动冒烟验证。这是本计划里唯一没有自动化测试的任务，刻意为之。

- [ ] **Step 1: 实现** — `agent/rawmode.go`

```go
package agent

// rawmode.go：唯一直接操作终端模式的「脏」层。用 stty（零依赖）把控制终端切到
// raw（cbreak）模式，并提供还原。非 tty / 无 stty 时返回 enabled=false，调用方降级。

import (
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
)

// EnableRaw 尝试把当前控制终端切到 raw 模式。
//   - 成功：返回 (restore, true)；restore 还原终端，幂等、可安全多次调用。
//   - 失败（非 tty / 无 stty）：返回 (no-op, false)，调用方应回退到行模式。
//
// 关键参数 `min 0 time 1`（VMIN=0/VTIME=0.1s）：让读带超时，从而能区分裸 ESC
// 与方向键序列（见 keys.go）。`-icanon -echo` 把行编辑交给我们自己做；`-isig`
// 让 Ctrl-C 变成普通字节由上层解释。
func EnableRaw() (restore func(), enabled bool) {
	saved, err := stty("-g")
	if err != nil {
		return func() {}, false
	}
	if _, err := stty("-icanon", "-isig", "-iexten", "-echo", "min", "0", "time", "1"); err != nil {
		return func() {}, false
	}

	var once sync.Once
	restore = func() {
		once.Do(func() { _, _ = stty(saved) })
	}

	// 信号兜底：被 kill（SIGTERM/SIGHUP）时 defer 不执行，这里先还原再退出，
	// 绝不把用户 shell 留在 raw 模式。isig 已关，Ctrl-C 不再产生 SIGINT。
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-ch
		restore()
		os.Exit(1)
	}()

	return restore, true
}

// stty 执行一次 stty，作用于 os.Stdin 指向的终端，返回其标准输出。
func stty(args ...string) (string, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}
```

- [ ] **Step 2: 构建与静态检查**

Run: `go build ./... && go vet ./... && gofmt -l agent/rawmode.go`
Expected: 无输出（编译通过、vet 干净、已格式化）

- [ ] **Step 3: 提交**

```bash
git add agent/rawmode.go
git commit -m "$(printf 'feat(agent): stty raw 模式开关（含信号兜底还原）\n\nCo-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>')"
```

---

### Task 5: 终端 UI 整合（ui.go + terminal.go）

**Files:**
- Modify: `agent/ui.go`（`UI` 接口加 `ReadLine` 与 `WatchInterrupt`）
- Modify: `agent/terminal.go`（`TerminalUI` 加 raw 路径、新方法、新构造器）
- Modify: `agent/integration_test.go`（给 `fakeUI` 补两个新方法）
- Test: `agent/terminal_raw_test.go`

**Interfaces:**
- Consumes: `pump`/`Key`（Task 2）、`ReadLine`/`Outcome`（Task 3）。
- Produces:
  - `UI` 接口新增 `ReadLine(prompt string) (string, Outcome)`、`WatchInterrupt(cancel context.CancelFunc) (stop func())`
  - `func NewRawTerminalUI(in io.Reader, out io.Writer) *TerminalUI`
  - `NewTerminalUI(in io.Reader, out io.Writer) *TerminalUI` 签名**不变**（保持非 raw 行为，现有测试不动）。

- [ ] **Step 1: 写失败测试** — `agent/terminal_raw_test.go`

```go
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
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./agent/ -run TestRawTerminalUI -v`
Expected: FAIL（`undefined: NewRawTerminalUI` 等）

- [ ] **Step 3: 改 `agent/ui.go`** —— 给 `UI` 接口加两个方法，并 import `context`

把现有 `import "zsh-agent/llm"` 改为：

```go
import (
	"context"

	"zsh-agent/llm"
)
```

在 `UI` 接口里（`Sink()` 之后、`ConfirmTool` 之前）加 `ReadLine`，并在末尾加 `WatchInterrupt`，使接口变为：

```go
type UI interface {
	// Sink 返回本回合用于「实时显示模型输出」的 sink。
	Sink() OutputSink
	// ReadLine 读取用户的一行输入（终端实现支持 raw 模式下的 rune/宽度感知行编辑）。
	// 返回输入文本与结局（提交 / EOF 退出 / 中断重来）。
	ReadLine(prompt string) (string, Outcome)
	// ConfirmTool 在执行某个工具前征求用户同意。返回 true 表示允许执行。
	ConfirmTool(name, preview string) bool
	// ToolOutput 显示工具执行的输出。
	ToolOutput(s string)
	// WatchInterrupt 在一段「可取消操作」期间监听打断键（ESC / Ctrl-C），命中即调
	// cancel。返回的 stop 在操作结束后调用以停止监听、交还键盘。非 raw 实现返回 no-op。
	WatchInterrupt(cancel context.CancelFunc) (stop func())
}
```

- [ ] **Step 4: 改 `agent/terminal.go`** —— raw 路径、新方法、新构造器

把 import 块改为（加 `context`）：

```go
import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
)
```

把 `TerminalUI` 结构体与构造器替换为：

```go
type TerminalUI struct {
	in   *bufio.Reader // 非 raw 回退时按行读
	out  io.Writer
	keys chan Key // raw 模式下由 pump 喂键
	raw  bool
}

// NewTerminalUI 构造「非 raw（行模式）」终端 UI：沿用内核行编辑，简单稳妥，
// 但不支持中文退格修复与打断。管道运行 / 非 tty 时用它。
func NewTerminalUI(in io.Reader, out io.Writer) *TerminalUI {
	return &TerminalUI{in: bufio.NewReader(in), out: out}
}

// NewRawTerminalUI 构造「raw 模式」终端 UI：自起 pump 独占读 in，提供 rune/宽度
// 感知的行编辑与 ESC/Ctrl-C 打断。调用方须已通过 EnableRaw 把终端切到 raw。
func NewRawTerminalUI(in io.Reader, out io.Writer) *TerminalUI {
	t := &TerminalUI{out: out, keys: make(chan Key, 256), raw: true}
	go pump(in, t.keys)
	return t
}
```

在文件末尾追加新方法（`Sink`/`ToolOutput` 保持不变）：

```go
// ReadLine 读取一行用户输入。raw 模式走 rune/宽度感知行编辑器；否则回退按行读。
func (t *TerminalUI) ReadLine(prompt string) (string, Outcome) {
	if !t.raw {
		fmt.Fprint(t.out, "\n"+prompt)
		line, err := t.in.ReadString('\n')
		if err == io.EOF {
			return "", OutcomeEOF
		}
		return strings.TrimSpace(line), OutcomeSubmit
	}
	fmt.Fprint(t.out, "\n") // 空行分隔，打一次（不进重绘，避免滚屏）
	drainKeys(t.keys)       // 丢弃陈旧 type-ahead
	line, oc := ReadLine(t.keys, t.out, prompt)
	return strings.TrimSpace(line), oc
}

// WatchInterrupt 在可取消操作期间监听 ESC / Ctrl-C，命中即 cancel。
// 返回的 stop 停止监听并等待 goroutine 退出，交还键盘所有权。
func (t *TerminalUI) WatchInterrupt(cancel context.CancelFunc) func() {
	if !t.raw {
		return func() {} // 非 raw：不支持打断
	}
	stopCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stopCh:
				return
			case k, ok := <-t.keys:
				if !ok {
					return
				}
				if k.Kind == KeyEsc || k.Kind == KeyCtrlC {
					cancel()
				}
			}
		}
	}()
	return func() {
		close(stopCh)
		<-done
	}
}

// drainKeys 非阻塞地清空 keys 里残留的陈旧按键。
func drainKeys(keys <-chan Key) {
	for {
		select {
		case <-keys:
		default:
			return
		}
	}
}
```

把现有 `ConfirmTool` 整个函数体替换为（兼容 raw / 非 raw；非 raw 行为与现有测试一致）：

```go
func (t *TerminalUI) ConfirmTool(name, preview string) bool {
	fmt.Fprintf(t.out, "\n  \033[33m▶ %s %s\033[0m\n", name, preview)
	const prompt = "  执行？[Y/n] "

	var line string
	if t.raw {
		drainKeys(t.keys)
		l, oc := ReadLine(t.keys, t.out, prompt)
		if oc != OutcomeSubmit { // ESC / Ctrl-C / Ctrl-D 在确认处 = 拒绝
			fmt.Fprint(t.out, "  （已拒绝）\n")
			return false
		}
		line = l
	} else {
		fmt.Fprint(t.out, prompt)
		l, _ := t.in.ReadString('\n')
		line = l
	}

	if answer := strings.ToLower(strings.TrimSpace(line)); answer == "n" || answer == "no" {
		fmt.Fprint(t.out, "  （已拒绝）\n")
		return false
	}
	return true
}
```

> 说明：spec 写「ESC 在确认处 = 拒绝该工具并中止本回合」；此处实现为**拒绝该工具**（返回 false），由模型得知被拒后自行决定，不强制清空整回合 —— 保持 `ConfirmTool` 返回 bool 的简单契约。若日后要「确认处 ESC 即中止整回合」，再扩展接口。

- [ ] **Step 5: 改 `agent/integration_test.go`** —— 给 `fakeUI` 补两个新方法

确认 import 块含 `"context"`（已有）。在 `fakeUI` 的 `ToolOutput` 方法之后加：

```go
func (f *fakeUI) ReadLine(prompt string) (string, Outcome) { return "", OutcomeEOF }
func (f *fakeUI) WatchInterrupt(cancel context.CancelFunc) func() { return func() {} }
```

- [ ] **Step 6: 运行测试（新 + 旧全过）**

Run: `go test ./agent/ -v`
Expected: PASS（含新 `TestRawTerminalUI*` 与全部既有 terminal / integration / clamp 测试）

- [ ] **Step 7: 提交**

```bash
gofmt -w agent/ui.go agent/terminal.go agent/integration_test.go agent/terminal_raw_test.go
git add agent/ui.go agent/terminal.go agent/integration_test.go agent/terminal_raw_test.go
git commit -m "$(printf 'feat(agent): TerminalUI 接入 raw 行编辑与打断监听\n\nCo-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>')"
```

---

### Task 6: 核心循环接入打断与回滚（agent.go）

**Files:**
- Modify: `agent/agent.go`（`Run` 用 `WatchInterrupt` 括住 Chat 与 tool.Run，取消即回滚）
- Test: `agent/interrupt_test.go`

**Interfaces:**
- Consumes: `UI.WatchInterrupt`（Task 5）。
- 保持：`clampToolOutput`/`maxToolOutputRunes` 截断逻辑**原样保留**。

- [ ] **Step 1: 写失败测试** — `agent/interrupt_test.go`

```go
package agent

import (
	"context"
	"testing"

	"zsh-agent/llm"
	"zsh-agent/tools"
)

// blockingProvider 的 Chat 阻塞到 ctx 取消，再返回 ctx.Err()。
type blockingProvider struct{}

func (blockingProvider) Chat(ctx context.Context, _ []llm.Message, _ []llm.ToolSpec, _ llm.StreamSink) (*llm.Response, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// interruptUI 的 WatchInterrupt 立刻 cancel，模拟用户一进生成就按 ESC。
type interruptUI struct{}

func (interruptUI) Sink() OutputSink                       { return nopSink{} }
func (interruptUI) ReadLine(string) (string, Outcome)      { return "", OutcomeEOF }
func (interruptUI) ConfirmTool(string, string) bool        { return true }
func (interruptUI) ToolOutput(string)                      {}
func (interruptUI) WatchInterrupt(cancel context.CancelFunc) func() {
	cancel()
	return func() {}
}

type nopSink struct{}

func (nopSink) OnReasoning(string) {}
func (nopSink) OnContent(string)   {}
func (nopSink) Close()             {}

func TestAgentRun_InterruptRollsBack(t *testing.T) {
	ag := New(blockingProvider{}, tools.NewRegistry(), interruptUI{})
	history := []llm.Message{
		{Role: llm.RoleSystem, Content: "sys"},
		{Role: llm.RoleUser, Content: "hi"},
	}
	out, err := ag.Run(context.Background(), history)
	if err != context.Canceled {
		t.Fatalf("期望 context.Canceled，得到 %v", err)
	}
	if len(out) != len(history) {
		t.Fatalf("历史应回滚到 %d 条，实际 %d", len(history), len(out))
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./agent/ -run TestAgentRun_InterruptRollsBack -v`
Expected: FAIL（当前 `Run` 不返回 `context.Canceled`，且不调 `WatchInterrupt`）

- [ ] **Step 3: 替换 `agent/agent.go` 的 `Run` 函数体**

把 `func (a *Agent) Run(...)` 整个函数替换为（保留上方注释；**保留 clampToolOutput**）：

```go
func (a *Agent) Run(ctx context.Context, history []llm.Message) ([]llm.Message, error) {
	startLen := len(history) // 打断时回滚到这里（含本回合的 user 消息）
	for step := 0; step < maxSteps; step++ {
		// 1. 问模型；WatchInterrupt 期间按 ESC/Ctrl-C 会 cancel 掉 cctx，
		//    令正在读的 SSE/HTTP 立刻报错返回。
		sink := a.ui.Sink()
		cctx, cancel := context.WithCancel(ctx)
		stop := a.ui.WatchInterrupt(cancel)
		resp, err := a.provider.Chat(cctx, history, a.tools.Specs(), sink)
		stop()
		canceled := cctx.Err() == context.Canceled
		cancel()
		sink.Close()
		if canceled {
			return history[:startLen], context.Canceled // 回滚本回合 append
		}
		if err != nil {
			return history, err
		}

		// 2. 记进历史。
		history = append(history, resp.Message)

		// 3. 不再调工具 → 收工。
		if resp.StopReason != "tool_calls" {
			return history, nil
		}

		// 4. 逐个执行工具（先确认；执行期间同样可打断）。
		for _, call := range resp.Message.ToolCalls {
			tool, ok := a.tools.Get(call.Name)
			if !ok {
				history = append(history, llm.ToolResult(call.ID, "未知工具: "+call.Name, true))
				continue
			}
			if !a.ui.ConfirmTool(call.Name, string(call.Args)) {
				history = append(history, llm.ToolResult(call.ID, "用户拒绝执行此命令。", true))
				continue
			}

			tctx, tcancel := context.WithCancel(ctx)
			tstop := a.ui.WatchInterrupt(tcancel)
			out, rerr := tool.Run(tctx, call.Args)
			tstop()
			tcanceled := tctx.Err() == context.Canceled
			tcancel()
			if tcanceled {
				return history[:startLen], context.Canceled
			}
			if rerr != nil {
				out += "\n[执行错误: " + rerr.Error() + "]"
			}
			// UI 展示完整输出；回填历史的副本做截断（保护上下文窗口）。
			a.ui.ToolOutput(out)
			history = append(history, llm.ToolResult(call.ID, clampToolOutput(out, maxToolOutputRunes), rerr != nil))
		}
	}

	return history, fmt.Errorf("达到最大步数 %d，已停止（可能陷入工具循环）", maxSteps)
}
```

- [ ] **Step 4: 运行测试（新 + 旧全过）**

Run: `go test ./agent/ -v`
Expected: PASS（新打断测试 + 既有 integration / clamp 测试都过 —— 确认截断逻辑未被破坏）

- [ ] **Step 5: 提交**

```bash
gofmt -w agent/agent.go agent/interrupt_test.go
git add agent/agent.go agent/interrupt_test.go
git commit -m "$(printf 'feat(agent): 核心循环支持 ESC 打断与整回合回滚\n\nCo-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>')"
```

---

### Task 7: 接线与降级（main.go）

**Files:**
- Modify: `main.go`

**Interfaces:**
- Consumes: `agent.EnableRaw`（Task 4）、`agent.NewRawTerminalUI`/`agent.NewTerminalUI`（Task 5）、`agent.OutcomeEOF`/`agent.OutcomeInterrupt`（Task 3）、`Run` 返回 `context.Canceled`（Task 6）。

- [ ] **Step 1: 重写 `main.go`**

把 import 块改为（去掉不再用的 `bufio`/`io`/`strings`）：

```go
import (
	"context"
	"fmt"
	"os"

	"zsh-agent/agent"
	"zsh-agent/config"
	"zsh-agent/llm"
	"zsh-agent/tools"
)
```

`defaultSystemPrompt` 常量保持不变。把 `func main()` 整体替换为：

```go
func main() {
	// 1. 读配置。
	path := os.Getenv("AGENT_CONFIG")
	if path == "" {
		path = "config.json"
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "加载配置失败:", err)
		fmt.Fprintln(os.Stderr, "提示：可参考 config.example.json 创建 config.json，或用 AGENT_CONFIG 指定路径。")
		os.Exit(1)
	}

	// 2. 尝试进 raw 模式：成功则启用 rune/宽度行编辑 + ESC 打断；失败（非 tty /
	//    无 stty）则降级为普通行模式。defer 还原，保证退出时终端干净。
	restore, raw := agent.EnableRaw()
	defer restore()

	// 3. 按配置组装各层。
	provider := llm.NewOpenAIProvider(cfg.BaseURL, cfg.APIKey, cfg.Model, cfg.MaxTokens, cfg.Stream, cfg.InsecureSkipVerify)
	reg := tools.NewRegistry()
	reg.Register(tools.Bash{}) // 想加工具？实现 tools.Tool 后在这里再 Register 一行即可。

	var ui agent.UI
	if raw {
		ui = agent.NewRawTerminalUI(os.Stdin, os.Stdout)
	} else {
		ui = agent.NewTerminalUI(os.Stdin, os.Stdout)
	}
	ag := agent.New(provider, reg, ui)

	// 4. 初始对话历史：开头放一条 system 消息。
	system := cfg.SystemPrompt
	if system == "" {
		system = defaultSystemPrompt
	}
	history := []llm.Message{{Role: llm.RoleSystem, Content: system}}

	// 5. REPL：读一行 → 跑一个回合 → 循环。
	fmt.Printf("zsh-agent 已就绪（后端 %s，模型 %s）。输入需求后回车，Ctrl+D 退出。\n", cfg.BaseURL, cfg.Model)
	if raw {
		fmt.Println("（生成中可按 ESC 或 Ctrl-C 打断）")
	}
	for {
		line, oc := ui.ReadLine("\033[36m你 ›\033[0m ")
		if oc == agent.OutcomeEOF {
			fmt.Println()
			return
		}
		if oc == agent.OutcomeInterrupt {
			continue // 放弃本行，重新给提示符
		}
		if line == "" {
			continue
		}

		history = append(history, llm.Message{Role: llm.RoleUser, Content: line})

		updated, err := ag.Run(context.Background(), history)
		if err == context.Canceled {
			fmt.Println("（已打断）")
			history = history[:len(history)-1] // 丢掉这条 user，回到干净状态
			continue
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "调用出错:", err)
			history = history[:len(history)-1] // 出错时丢掉刚才那条 user，便于干净重试
			continue
		}
		history = updated
	}
}
```

- [ ] **Step 2: 构建 + 全量测试 + 格式检查**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l .`
Expected: 测试全过；`gofmt -l .` 无输出

- [ ] **Step 3: 手动冒烟验证**（需要真实终端 + 可用的 `config.json`）

```bash
go run .
```
逐项确认：
1. 在 `你 ›` 处输入中文（如「你好世界」），按 **退格**：每次干净删掉**一个整字**，无残影、无乱码。
2. 左右方向键能在中文之间移动光标；Ctrl-U 清整行；Ctrl-W 删一个词。
3. 发一个会让模型持续输出的需求，生成中按 **ESC**：立刻停下、打印「（已打断）」、回到提示符，可马上继续输入。
4. 让模型跑一个较慢的 bash 命令，执行中按 **ESC**：命令被中止、回到提示符。
5. 空行按 **Ctrl-D**：干净退出，且退出后终端**回显正常**（不是 raw 残留）。
6. 回归：英文输入、`[Y/n]` 确认、工具输出展示等与之前一致。

- [ ] **Step 4: 提交**

```bash
git add main.go
git commit -m "$(printf 'feat: main 接入 raw 模式输入与 ESC 打断（非 tty 自动降级）\n\nCo-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>')"
```

---

## 收尾

全部任务完成后：
- `go build ./...`、`go vet ./...`、`go test ./...`、`gofmt -l .` 应全部干净。
- 分支 `feat/rawmode-input-esc` 上是：1 个设计文档提交 + 1 个计划文档提交 + 7 个实现提交。
- 用 `superpowers:finishing-a-development-branch` 决定合并 / PR / 清理。

## 自检对照（spec → task 覆盖）

- 中文退格修复 → Task 1（宽度）+ Task 3（行编辑器，`TestReadLine_ChineseBackspace`）+ Task 5（`TestRawTerminalUI_ReadLineChineseBackspace`）。
- ESC/Ctrl-C 打断 → Task 2（裸 ESC 解码）+ Task 5（`WatchInterrupt`）+ Task 6（回滚，`TestAgentRun_InterruptRollsBack`）。
- 中等编辑能力（← →、Home/End、Ctrl-U/W）→ Task 2 解码 + Task 3 处理 + 测试。
- 单 pump 串行用键 → Task 2（pump）+ Task 5（`ReadLine`/`ConfirmTool`/`WatchInterrupt` 互斥、`drainKeys`）。
- raw 模式（stty）+ 信号兜底还原 → Task 4。
- 非 tty 优雅降级 → Task 5（`NewTerminalUI` 非 raw 路径）+ Task 7（`EnableRaw` 失败分支）。
- 整回合回滚（Run 回滚 + main 丢 user）→ Task 6 + Task 7。
- 保留工具输出截断 → Task 6（显式保留 `clampToolOutput`）。

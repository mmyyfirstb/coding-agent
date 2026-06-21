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
	KeyBackspace                // 退格
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
	// utf8.RuneError(U+FFFD) 本身是合法字符，只有 size==1 时才说明解码失败。
	if (r == utf8.RuneError && size == 1) || size != n {
		return 0, false
	}
	return r, true
}

package terminal

// lineeditor.go：纯行编辑器。从「按键 channel」读 Key，维护 []rune 缓冲 + 光标，
// 把行实时重绘到 out，回车返回整行。不碰 tty、不碰 os.Stdin —— 可用喂 Key 来单测。
// 显示宽度（中文占 2 列）一律走 width.go，从根上修好「中文退格残影」。

import (
	"fmt"
	"io"

	"zsh-agent/agent"
)

// ReadLine 从 keys 读键编辑一行，返回最终文本与结局。
//   - prompt 是「单行」提示符（可含 ANSI 颜色码，但不要含换行，否则重绘会滚屏）。
//   - 调用方负责在调用前安排好垂直间距（如先打一个换行）。
//   - escCancels=true 时，ESC 立即返回 ("", OutcomeCancel)，适合工具确认等「ESC=拒绝」场景；
//     escCancels=false 时，ESC 仅清空当前行继续编辑，适合主 REPL 提示符。
func ReadLine(keys <-chan Key, out io.Writer, prompt string, escCancels bool) (string, agent.Outcome) {
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
			if escCancels {
				// escCancels 模式（如工具确认处）：ESC = 取消，立即返回。
				fmt.Fprint(out, "\r\n")
				return "", agent.OutcomeCancel
			}
			// 普通模式（主 REPL）：ESC = 清空当前行，继续编辑。
			buf = buf[:0]
			cursor = 0
		case KeyEnter:
			fmt.Fprint(out, "\r\n")
			return string(buf), agent.OutcomeSubmit
		case KeyCtrlC:
			fmt.Fprint(out, "\r\n")
			return "", agent.OutcomeInterrupt
		case KeyCtrlD:
			if len(buf) == 0 {
				fmt.Fprint(out, "\r\n")
				return "", agent.OutcomeEOF
			}
			continue // 非空行忽略 Ctrl-D，且不必重绘
		}
		redraw()
	}
	// keys 关闭（输入流结束）→ 等价 EOF。
	return string(buf), agent.OutcomeEOF
}

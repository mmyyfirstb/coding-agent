package terminal

// lineeditor.go：纯行编辑器。从「按键 channel」读 Key，维护 []rune 缓冲 + 光标，
// 把行实时重绘到 out，回车返回整行。不碰 tty、不碰 os.Stdin —— 可用喂 Key 来单测。
// 显示宽度（中文占 2 列）一律走 width.go，从根上修好「中文退格残影」。

import (
	"fmt"
	"io"
	"strings"

	"zsh-agent/agent"
)

// ReadLine 从 keys 读键编辑一行，返回最终文本与结局。
//   - prompt 是「单行」提示符（可含 ANSI 颜色码，但不要含换行，否则重绘会滚屏）。
//   - 调用方负责在调用前安排好垂直间距（如先打一个换行）。
//   - escCancels=true 时，ESC 立即返回 ("", OutcomeCancel)，适合工具确认等「ESC=拒绝」场景；
//     escCancels=false 时，ESC 仅清空当前行继续编辑，适合主 REPL 提示符。
func ReadLine(keys <-chan Key, out io.Writer, prompt string, escCancels bool, cols int) (string, agent.Outcome) {
	var buf []rune
	cursor := 0 // 光标在 buf 中的 rune 下标，0..len(buf)
	promptW := displayWidth(prompt)
	if cols < 1 {
		cols = 80 // 兜底：宽度未知时按 80 列折行
	}

	// 多行重绘状态：记住上一帧占了几行、光标当时在第几列，
	// 这样下一帧能先回到首行、清掉所有旧行，再整段重排——
	// 从根上修掉「内容超过一行宽度后不停刷旧行」的重影。
	// 算法移植自 linenoise 的 refreshMultiLine，并改成「显示列宽」感知（中文占 2 列）。
	oldRows := 1   // 上一帧内容占的物理行数（>=1）
	oldColpos := 0 // 上一帧光标的显示列偏移（用于算它当时在第几行）

	redraw := func() {
		blen := runesWidth(buf)          // 内容总显示宽
		cpos := runesWidth(buf[:cursor]) // 光标前内容显示宽
		rows := (promptW + blen + cols - 1) / cols
		if rows < 1 {
			rows = 1
		}
		rpos := (promptW + oldColpos + cols) / cols // 上一帧光标所在物理行（1 基）

		var b strings.Builder
		// 1) 先下移到上一帧的最后一行。
		if down := oldRows - rpos; down > 0 {
			fmt.Fprintf(&b, "\033[%dB", down)
		}
		// 2) 自下而上逐行「清行 + 上移」。
		for j := 0; j < oldRows-1; j++ {
			b.WriteString("\r\033[K\033[1A")
		}
		// 3) 清掉首行，回到起点。
		b.WriteString("\r\033[K")
		// 4) 重打提示符 + 全部内容（终端会按需自动折行）。
		b.WriteString(prompt)
		b.WriteString(string(buf))
		// 5) 边缘补行：光标在末尾且正好填满整行时，终端不会真正换行，
		//    需手动 \n 把后续光标定位推到新行（否则定位会偏）。
		if len(buf) > 0 && cursor == len(buf) && (promptW+blen)%cols == 0 {
			b.WriteString("\n\r")
			rows++
		}
		// 6) 上移到光标应在的行。
		rpos2 := (promptW + cpos + cols) / cols
		if up := rows - rpos2; up > 0 {
			fmt.Fprintf(&b, "\033[%dA", up)
		}
		// 7) 定位到目标列（CSI 0 C 会被当作 1，col==0 时只回行首）。
		if col := (promptW + cpos) % cols; col > 0 {
			fmt.Fprintf(&b, "\r\033[%dC", col)
		} else {
			b.WriteString("\r")
		}
		oldColpos = cpos
		oldRows = rows
		fmt.Fprint(out, b.String())
	}

	// finish 在回车 / 中断等收尾时，把光标从当前行移到整段输入的下一行，
	// 保证已输入内容原样留在屏上、后续输出另起一行。
	finish := func() {
		rpos := (promptW + oldColpos + cols) / cols
		if down := oldRows - rpos; down > 0 {
			fmt.Fprintf(out, "\033[%dB", down)
		}
		fmt.Fprint(out, "\r\n")
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
				finish()
				return "", agent.OutcomeCancel
			}
			// 普通模式（主 REPL）：ESC = 清空当前行，继续编辑。
			buf = buf[:0]
			cursor = 0
		case KeyEnter:
			finish()
			return string(buf), agent.OutcomeSubmit
		case KeyCtrlC:
			finish()
			return "", agent.OutcomeInterrupt
		case KeyCtrlD:
			if len(buf) == 0 {
				finish()
				return "", agent.OutcomeEOF
			}
			continue // 非空行忽略 Ctrl-D，且不必重绘
		}
		redraw()
	}
	// keys 关闭（输入流结束）→ 等价 EOF。
	return string(buf), agent.OutcomeEOF
}

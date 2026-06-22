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

	// 多行重绘状态：记住上一帧占了几行、光标当时在第几「物理行」，
	// 这样下一帧能先回到首行、清掉所有旧行，再整段重排——
	// 从根上修掉「内容超过一行宽度后不停刷旧行」的重影。
	// 算法移植自 linenoise 的 refreshMultiLine，但行列一律用 wrapRowCol 按真实折行
	// 模拟（中文占 2 列、放不下时整字符挪行、上一行末尾留白），不再用线性 /cols 估算——
	// 后者会忽略「全角字符跨边界时上一行尾部留的那一列空白」，导致光标列偏 1、嵌进汉字中间。
	oldRows := 1      // 上一帧内容占的物理行数（>=1）
	oldCursorRow := 0 // 上一帧光标停在第几物理行（0 基）

	redraw := func() {
		endRow, endCol := wrapRowCol(buf, promptW, cols)          // 整段末尾光标落点
		curRow, curCol := wrapRowCol(buf[:cursor], promptW, cols) // 目标光标落点
		rows := endRow + 1

		var b strings.Builder
		// 1) 从上一帧光标所在行下移到上一帧最后一行。
		if down := oldRows - 1 - oldCursorRow; down > 0 {
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
		// 5) 边缘补行：内容正好填满整行时，终端停在 pending-wrap（光标贴右边沿但未真正换行），
		//    手动 \n 把光标推到真正的新行行首，后续定位才不会偏。
		if endCol == cols {
			b.WriteString("\n\r")
			rows++
			endRow++ // 光标被推到新行行首；endCol 此后不再用，无需归零
		}
		// 6) 从末尾光标行上移到目标光标行。
		if up := endRow - curRow; up > 0 {
			fmt.Fprintf(&b, "\033[%dA", up)
		}
		// 7) 定位到目标列（CSI 0 C 会被当作 1，col==0 时只回行首）。
		b.WriteString("\r")
		if curCol > 0 {
			fmt.Fprintf(&b, "\033[%dC", curCol)
		}
		oldRows = rows
		oldCursorRow = curRow
		fmt.Fprint(out, b.String())
	}

	// finish 在回车 / 中断等收尾时，把光标从当前行移到整段输入的下一行，
	// 保证已输入内容原样留在屏上、后续输出另起一行。
	finish := func() {
		if down := oldRows - 1 - oldCursorRow; down > 0 {
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

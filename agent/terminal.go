package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
)

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

// Sink 返回一个新的 terminalSink，负责把本回合模型输出的思考 / 正文实时打到终端。
func (t *TerminalUI) Sink() OutputSink {
	return &terminalSink{out: t.out}
}

// terminalSink 把模型输出的增量实时显示到终端，并负责加「思考 / 助手」标签与配色。
//
// 它是有状态的——必须记住「思考是否已开头」「正文是否已开头」，才能在正确时机打标签、
// 处理思考→正文的换行切换。每个回合用一个新的（见 TerminalUI.Sink）。
//
// 标签体系与用户输入的青色「你 ›」对应：
//   - 思考：行首暗灰「思考」标签，整段暗灰（模型内部推演，视觉上让位但仍可见）；
//   - 正文：行首绿色加粗「助手」标签，正文默认色。
type terminalSink struct {
	out         io.Writer
	inReasoning bool // 思考已开头（此刻暗灰还开着，未重置）
	hasContent  bool // 正文已开头（「助手」标签已打）
}

// OnReasoning 显示一段思考增量。首段先打暗灰「思考」标签并开启暗灰。
func (s *terminalSink) OnReasoning(delta string) {
	if !s.inReasoning {
		fmt.Fprint(s.out, "\033[2m思考 ")
		s.inReasoning = true
	}
	fmt.Fprint(s.out, delta)
}

// OnContent 显示一段正文增量。若此前在显示思考，先关暗灰并换行结束思考块，
// 再在正文首段打绿色加粗「助手」标签。
func (s *terminalSink) OnContent(delta string) {
	if s.inReasoning {
		fmt.Fprint(s.out, "\033[0m\n") // 关暗灰 + 换行，结束思考块
		s.inReasoning = false
	}
	if !s.hasContent {
		fmt.Fprint(s.out, "\033[1;32m助手\033[0m ")
		s.hasContent = true
	}
	fmt.Fprint(s.out, delta)
}

// Close 收尾：补一个换行让后续输出（工具确认 / 下一个提示符）另起一行，
// 并确保暗灰被重置。本回合若一个字都没输出（模型只发了工具调用），则什么都不打。
func (s *terminalSink) Close() {
	switch {
	case s.inReasoning:
		fmt.Fprint(s.out, "\033[0m\n") // 只有思考、没有正文：关暗灰 + 换行
	case s.hasContent:
		fmt.Fprint(s.out, "\n") // 正文结尾补换行
	}
}

func (t *TerminalUI) ConfirmTool(name, preview string) bool {
	fmt.Fprintf(t.out, "\n  \033[33m▶ %s %s\033[0m\n", name, preview)
	const prompt = "  执行？[Y/n] "

	var line string
	if t.raw {
		drainKeys(t.keys)
		// escCancels=true：ESC 直接返回 OutcomeCancel，在确认处表示拒绝。
		// Ctrl-C → OutcomeInterrupt，Ctrl-D → OutcomeEOF，均视为拒绝。
		l, oc := ReadLine(t.keys, t.out, prompt, true)
		if oc != OutcomeSubmit {
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

// ToolOutput 把工具执行的输出打印出来，整体用暗灰（\033[2m...\033[0m）包一层，
// 让命令输出在视觉上「让位」给左侧顶格的对话文字，强化分组。
//
// 关于换行：工具的输出可能本来就以换行结尾（命令行常见），无条件再补一个会多出空行。
// 所以只在"没有以换行结尾"时才补一个，既保证下一条输出另起一行，又不会平白多出空行。
// 注意补的换行要放在暗灰重置（\033[0m）之后，避免把换行也算进染色区间。
//
// 边界情况：若某条命令自己输出了带颜色的内容（如 ls --color、彩色 git diff），
// 其内部的颜色重置码可能提前取消这里的暗灰——对纯文本输出无影响，属可接受。
func (t *TerminalUI) ToolOutput(s string) {
	fmt.Fprintf(t.out, "\033[2m%s\033[0m", s)
	if !strings.HasSuffix(s, "\n") {
		fmt.Fprint(t.out, "\n")
	}
}

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
	// escCancels=false：主 REPL 提示符，ESC 只清空当前行，不返回 OutcomeCancel。
	line, oc := ReadLine(t.keys, t.out, prompt, false)
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

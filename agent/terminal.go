package agent

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// TerminalUI 是 UI 接口的"终端实现"：直接和坐在命令行前的人交互。
//
// 它把输入输出都做成可注入的字段，而不是写死 os.Stdin / os.Stdout：
//   - 真跑的时候传 os.Stdin / os.Stdout；
//   - 写测试的时候传 strings.Reader / bytes.Buffer，就能断言交互结果。
//
// 这是 ui.go 里抽象的"换一个 UI 实现即可"的具体例子。
type TerminalUI struct {
	in  *bufio.Reader // 包一层 bufio，方便按行读用户输入
	out io.Writer     // 所有展示给人看的内容都写到这里
}

// NewTerminalUI 用给定的输入、输出构造一个终端 UI。
// in 会被包进 bufio.Reader，这样后面可以用 ReadString 一次读一整行。
func NewTerminalUI(in io.Reader, out io.Writer) *TerminalUI {
	return &TerminalUI{
		in:  bufio.NewReader(in),
		out: out,
	}
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

// ConfirmTool 在执行工具前征求用户同意，是这套 UI 的"安全闸"。
//
// 交互设计上的两个取舍：
//  1. 用黄色高亮把"即将执行的命令"标出来（ANSI \033[33m...\033[0m）。
//     黄色 = "注意，马上要动手了"，让人一眼看到 agent 准备做什么。
//  2. 默认 Yes（提示 [Y/n]，大写 Y 表示回车即同意）。
//     因为绝大多数情况下用户就是想让它跑，默认放行能少按一次键；
//     只有用户明确表达拒绝时才拦下来。
//
// 返回值语义（什么算"否"、什么算"是"）：
//   - 仅当用户输入去掉空格、转小写后等于 "n" 或 "no" 时，判为拒绝，
//     打印"（已拒绝）"并返回 false；
//   - 其余一切情况都算同意，返回 true——
//     包括直接回车（空输入）、输入别的内容、甚至读到 EOF / 出错。
//     这里刻意选择"出错也放行"以保持行为简单、可预测：
//     这一层只负责"明确说不就别跑"，不替用户做更复杂的判断。
func (t *TerminalUI) ConfirmTool(name, preview string) bool {
	// 黄色一行：标出工具名 + 参数预览。前面留一个换行让它更醒目。
	// 缩进两格，把「工具区」和左侧顶格的对话文字在视觉上分开成一组。
	// 这里刻意保持黄色、不调暗——它是执行前的安全闸，黄色 = “要动手了，注意看”，
	// 调暗会削弱警示。参数原样打印（不解析），保住 UI 层不依赖具体工具的分层。
	fmt.Fprintf(t.out, "\n  \033[33m▶ %s %s\033[0m\n", name, preview)
	// 提示语同样缩进、不带换行，让光标停在同一行等用户输入。
	fmt.Fprint(t.out, "  执行？[Y/n] ")

	// 读一整行。即使读取出错（比如 EOF），也按"默认 Yes"继续往下判断。
	line, _ := t.in.ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))

	// 只有明确说"不"才拒绝。
	if answer == "n" || answer == "no" {
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

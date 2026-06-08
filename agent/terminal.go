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

// AssistantText 把模型的文字回复打印出来，末尾补一个换行，
// 让下一条输出从新行开始、读起来不黏在一起。
func (t *TerminalUI) AssistantText(s string) {
	fmt.Fprintln(t.out, s)
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
	// 黄色一行：标出工具名 + 参数预览。前后各留一个换行让它更醒目。
	fmt.Fprintf(t.out, "\n\033[33m▶ %s %s\033[0m\n", name, preview)
	// 提示语不带换行，让光标停在同一行等用户输入。
	fmt.Fprint(t.out, "执行？[Y/n] ")

	// 读一整行。即使读取出错（比如 EOF），也按"默认 Yes"继续往下判断。
	line, _ := t.in.ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))

	// 只有明确说"不"才拒绝。
	if answer == "n" || answer == "no" {
		fmt.Fprint(t.out, "（已拒绝）\n")
		return false
	}
	return true
}

// ToolOutput 把工具执行的输出原样打印出来。
//
// 这里不无条件地加换行：工具的输出可能本来就以换行结尾（命令行常见），
// 那样再补一个会多出一个空行。所以只在"没有以换行结尾"时才补一个，
// 既保证下一条输出另起一行，又不会平白多出空行。
func (t *TerminalUI) ToolOutput(s string) {
	fmt.Fprint(t.out, s)
	if !strings.HasSuffix(s, "\n") {
		fmt.Fprint(t.out, "\n")
	}
}

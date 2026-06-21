package agent

import (
	"fmt"
	"unicode/utf8"
)

// clamp.go 只做一件事：把「过长的工具输出」压到一个上限内，再喂回模型历史。
//
// 为什么需要它：本 agent 会把每条工具结果原样追加进对话历史（agent.go），
// 而 bash 之类的命令很容易吐出几十 KB（cat 大文件 / ls -R / 构建日志）。
// 这些内容会迅速塞满模型的上下文窗口，稀释注意力；一旦越过窗口上限，后端
// （如 Ollama）还会从最前面静默丢弃——连 system prompt 一起丢。
//
// 策略：超限时只保留「头 + 尾」，中间用一行标记替代并注明省略了多少字符。
// 命令输出最有用的信息通常在开头（在做什么）和结尾（报错 / 退出码），中间
// 多为重复内容，省掉影响最小。

// clampToolOutput 把 s 压到至多 max 个「字符（rune）」。
//
//   - 未超限：原样返回。
//   - 超限：返回 头(max/2) + 省略标记 + 尾(max/2)。
//
// 注意按 rune 而非 byte 切，避免把中文等多字节字符切成半个、产生非法 UTF-8。
func clampToolOutput(s string, max int) string {
	// 未超限直接返回，常见路径零开销（仅一次 O(n) 计数）。
	if utf8.RuneCountInString(s) <= max {
		return s
	}

	rs := []rune(s)
	half := max / 2
	head := string(rs[:half])
	tail := string(rs[len(rs)-half:])
	omitted := len(rs) - 2*half

	return fmt.Sprintf("%s\n...[已省略中间 %d 个字符]...\n%s", head, omitted, tail)
}

package terminal

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

// wrapRowCol 模拟把 rs 逐字符摆进宽度为 cols 的终端（从第 0 行第 start 列起），
// 返回摆放结束后光标停在第几行第几列（均 0 基，行从 0 起算）。
// 终端折行规则：当前行放不下整个字符（col+w > cols）时，该字符整体挪到下一行行首、
// 上一行尾部留空——全角字符不跨行折断。内容正好填满（col==cols）时不提前换行，
// 维持「pending wrap」语义，交由调用方的边缘补行逻辑处理。
// 这正是行编辑器定位光标的依据：线性的 (start+宽)/cols 估算会漏掉跨边界留下的空白列。
func wrapRowCol(rs []rune, start, cols int) (row, col int) {
	col = start
	for _, r := range rs {
		w := runeWidth(r)
		if w == 0 {
			continue
		}
		if col+w > cols {
			row++
			col = 0
		}
		col += w
	}
	return
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

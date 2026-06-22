package terminal

// rawmode.go：唯一直接操作终端模式的「脏」层。用 stty（零依赖）把控制终端切到
// raw（cbreak）模式，并提供还原。非 tty / 无 stty 时返回 enabled=false，调用方降级。

import (
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
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

// PollingTTYReader 把一个「VMIN=0/VTIME>0 的 raw tty」读端（通常是 os.Stdin）适配回
// pump 所期望的契约：无输入超时时返回 (0, nil) 而非 (0, io.EOF)。
//
// 背景（这是「go run 直接退出」的真凶）：Go 的 os.File 默认 ZeroReadIsEOF=true，会把
// 底层 read(2) 返回 0 字节（在 raw tty 即「本次超时、暂无输入」）当成 io.EOF 上报。pump
// 据此判 readEnd → close(keys) → 整个 REPL 在第一次 100ms 超时后立刻退出。
//
// 对真实交互 raw tty 而言，read 返回 0 字节永远只意味着「超时」，绝不意味着流结束——
// 流真正结束（终端 / pty 关闭）会以非 EOF 错误（如 EIO）上报。因此这里只把 io.EOF 改写成
// 「超时」(0,nil)，其余错误一律透传，让 pump 仍能在终端关闭时正常收尾退出。
func PollingTTYReader(r io.Reader) io.Reader { return pollingTTYReader{r} }

type pollingTTYReader struct{ r io.Reader }

func (p pollingTTYReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n == 0 && err == io.EOF {
		return 0, nil // raw tty 读超时：不是真 EOF（见 PollingTTYReader 注释）
	}
	return n, err
}

// terminalWidth 通过 `stty size` 取当前终端列数（用于多行重绘正确折行）。
// 与 EnableRaw 一样走 stty，保持「零第三方依赖」；任何异常都回退 80 列。
func terminalWidth() int {
	out, err := stty("size")
	if err != nil {
		return 80
	}
	// `stty size` 输出形如 "rows cols"。
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return 80
	}
	cols, err := strconv.Atoi(fields[1])
	if err != nil || cols <= 0 {
		return 80
	}
	return cols
}

// stty 执行一次 stty，作用于 os.Stdin 指向的终端，返回其标准输出。
func stty(args ...string) (string, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

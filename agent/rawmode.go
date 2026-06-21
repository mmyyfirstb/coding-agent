package agent

// rawmode.go：唯一直接操作终端模式的「脏」层。用 stty（零依赖）把控制终端切到
// raw（cbreak）模式，并提供还原。非 tty / 无 stty 时返回 enabled=false，调用方降级。

import (
	"os"
	"os/exec"
	"os/signal"
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

// stty 执行一次 stty，作用于 os.Stdin 指向的终端，返回其标准输出。
func stty(args ...string) (string, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

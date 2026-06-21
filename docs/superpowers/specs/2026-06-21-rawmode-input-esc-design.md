# raw 模式输入 + rune/宽度感知行编辑 + ESC 打断

日期：2026-06-21

## 背景与动机

两个问题，根因相同，故一起设计、一起实现：

1. **中文删不掉**：提示符 `你 ›` 与工具确认 `[Y/n]` 都用 `bufio.Reader.ReadString('\n')`，终端处于默认 **canonical（cooked）模式**，backspace 由内核 tty 行规程在「按回车之前」处理掉，程序只在回车后拿到整行。中文字符**多字节（UTF-8 3 字节）+ 全角（占 2 显示列）**，canonical 模式的擦除回显按「一字符 = 一列」算，删全角时只擦掉右半格、左半个字残影留屏，看起来「删不掉」。即便 `iutf8` 开着、字节其实删对了，显示宽度这一维内核也不跟踪 —— 这是 canonical 模式的天生短板，改不动。
2. **无法打断**：生成 / 工具执行过程中没法按 ESC 中止。底层其实已就绪 —— `ctx` 贯穿全链路（`openai.go` 的 HTTP 用 `NewRequestWithContext`，`bash.go` 用 `CommandContext`），只差一个「按 ESC → cancel()」的触发器。

唯一彻底的解法是**把输入接管过来**：进 raw 模式，自己写一个按 rune + 按显示宽度的行编辑器。这套基建同时也是 ESC 打断所需的 —— 一次解决两件事。

## 目标 / 非目标

**目标**
- 修复：中文（多字节 + 全角）在提示符能用退格正确删除。
- 新增：生成 / 工具执行中按 ESC 打断，回到提示符随时重输。
- 附带（「中等」编辑能力）：← →、Home/End、Ctrl-U（删整行）、Ctrl-W（删词）、Ctrl-C、Ctrl-D。

**非目标（YAGNI）**
- ↑↓ 历史回溯、命令补全、Windows 支持、多行编辑。

**铁律（不可破）**
- 纯 Go 标准库、**零第三方依赖**；一文件一概念；重中文注释；`agent.go` 核心循环尽量不动。

## 选型决策

| 点 | 候选 | 选定 | 理由 |
|---|---|---|---|
| 进 raw 模式 | `stty` shell 命令 / `golang.org/x/term` | **`stty`** | 零依赖、契合「纯标准库」铁律；本就是 shell agent |
| 两输入态不打架 | 单 input pump / 读超时+停止标志 | **单 input pump** | 全程一个 goroutine 独占读 stdin，根治 `main.go` 注释里「两 reader 互偷输入」的坑 |
| 显示宽度 | 自写 CJK 区间表 / 第三方 runewidth | **自写表** | 零依赖 |

## 架构与分层（新增 4 个文件，职责单一、可独立测）

```
agent/keys.go       —— 输入 pump：唯一读 os.Stdin 者，把字节流解码成 Key 事件推进 channel。
                       负责 UTF-8 解码 + ESC[ 转义序列识别（方向键 / Home / End）。
agent/lineeditor.go —— 纯行编辑器：从「Key 来源」读键，维护 []rune 缓冲 + 光标（rune 下标），
                       渲染到 io.Writer，返回整行。不碰 tty、不碰 os.Stdin → 可单测。
agent/rawmode.go    —— 唯一的「脏」层：用 stty 进 / 出 raw 模式，返回 restore 函数。
agent/width.go      —— runeWidth(r)：CJK / 全角 = 2，组合 / 零宽 = 0，其余 = 1。纯函数，表驱动。
```

**关键解耦**：行编辑器是纯的（键从接口来、字往 `io.Writer` 去），raw 模式那点脏活单独关在 `rawmode.go`。于是编辑器能用「喂一串字节、断言最终字符串与渲染输出」来单测，不需要真 tty —— 契合项目「接口隔离」的风格。

**谁拥有这些**：`agent/terminal.go` 的 `TerminalUI` 把上面几块组合起来 —— 启动时建 `keys` channel 并起 `pump`，对外暴露三个**互斥**的用键方式：`ReadLine`（提示符）、`ConfirmTool`（y/n，内部也走 `lineeditor`）、`WatchInterrupt`（监听打断）。`pump` 是**唯一**读 `os.Stdin` 的 goroutine，常驻；三个用键方式都不直接读 stdin，而是 `select` 那个 `keys` channel。这样「谁在用键」由调用时序串行决定，彻底无并发读。

### 关键：监听打断不能和 ConfirmTool 抢键

一个回合内 `agent.Run` 会依次：Chat → （可能）ConfirmTool → tool.Run。打断监听必须只包住 **Chat** 和 **tool.Run**（阻塞、非交互），**避开 ConfirmTool**（交互、自己在读键）。否则监听 goroutine 和 ConfirmTool 会同时从 `keys` channel 取键，又回到「两 reader 互偷输入」。

做法：给 `UI` 接口加一个能力，由 `Run` 在每个可取消操作前后括起来：

```go
// WatchInterrupt 启动对 ESC / Ctrl-C 的监听，命中即调 cancel。
// 返回的 stop 在可取消操作结束后调用，停止监听、交还键的所有权。
WatchInterrupt(cancel context.CancelFunc) (stop func())
```

监听 goroutine 用 `select { case <-stopCh: return; case k := <-keys: ... }`，`stop()` 关 `stopCh` 后它**立即**退出（select 走 stopCh 分支，无需等下一个键）—— 这正是「pump 读 stdin、消费者只 select channel」解耦的价值：消费者永不阻塞在 `os.Read` 上，能被瞬间叫停。

### Key 事件类型

pump 把原始字节解码成中立的 `Key`，供编辑器与 watcher 共同消费：

```go
type KeyKind int
const (
    KeyRune  KeyKind = iota // 一个可见字符（含中文），值在 Key.Rune
    KeyEnter                 // \r 或 \n
    KeyBackspace            // 0x7f / 0x08
    KeyLeft; KeyRight        // ESC[D / ESC[C
    KeyHome; KeyEnd          // ESC[H / ESC[F（也接受 Ctrl-A / Ctrl-E）
    KeyCtrlU; KeyCtrlW       // 删整行 / 删词
    KeyCtrlC; KeyCtrlD       // 中断 / EOF
    KeyEsc                   // 裸 ESC
)
type Key struct { Kind KeyKind; Rune rune }
```

### ReadLine 返回的结局（outcome）

`ReadLine` 不止返回字符串，还要告诉调用方「为什么结束」，避免歧义：

```go
type Outcome int
const (
    OutcomeSubmit Outcome = iota // 回车提交，line 有效
    OutcomeEOF                   // 空行 Ctrl-D：请求退出 REPL
    OutcomeInterrupt             // Ctrl-C：放弃本行，重新给提示符
)
func ReadLine(keys <-chan Key, out io.Writer, prompt string) (line string, oc Outcome)
```

- 裸 ESC 在 idle 提示符处 = **清空当前行**，属 `ReadLine` 内部行为，不结束读取、不返回。
- Ctrl-C → `OutcomeInterrupt`；空行 Ctrl-D → `OutcomeEOF`；回车 → `OutcomeSubmit`。
- channel 关闭（pump 因 stdin EOF 退出）等价 `OutcomeEOF`。

## 数据流（一个 pump，多个串行消费者）

```
启动（main）：
  restore := rawmode.enableRaw(); defer restore()        // 整个 REPL 期间 raw
  ui := NewTerminalUI(os.Stdin, os.Stdout)               // 内部建 keys channel + 起 pump

每回合（main）：
  ① line, oc := ui.ReadLine("你 ›")     // 消费 keys 建行；OutcomeEOF→退出，Interrupt→重来
  ② history = append(history, user(line))
  ③ history, err := ag.Run(ctx, history)
  ④ if err == context.Canceled:         // 被打断
       打印「（已打断）」
       history = history[:len(history)-1]   // 丢掉这条 user（复用现有报错路径），回到干净提示符
     else if err != nil: 现有报错处理

agent.Run 内（每个可取消操作自包裹）：
  startLen := len(history)
  for step:
    cctx, cancel := context.WithCancel(ctx)
    stop := ui.WatchInterrupt(cancel)
    resp, err := provider.Chat(cctx, history, specs, ui.Sink())
    stop()
    if cctx.Err()==Canceled: return history[:startLen], context.Canceled   // 回滚本回合 append
    ... ui.ConfirmTool(...) ...          // 此刻无监听 goroutine，独占读键
    tctx, tcancel := context.WithCancel(ctx); tstop := ui.WatchInterrupt(tcancel)
    out, err := tool.Run(tctx, args); tstop()
    if tctx.Err()==Canceled: return history[:startLen], context.Canceled
```

**消费者全程串行**：`ReadLine`（回合间）→ `WatchInterrupt` 监听（Chat 时）→ `ConfirmTool`（确认时）→ `WatchInterrupt` 监听（tool.Run 时）。任一时刻只有一个在 `select` keys，互不抢。pump 常驻读 stdin、缓冲进 256 长 channel。生成期间监听 goroutine 取到的非 ESC/Ctrl-C 键被丢弃；监听停止到下次 `ReadLine` 之间的 type-ahead 会留在 channel，`ReadLine` 起手先把 channel 里的陈旧键 drain 掉，避免误带入。

## ESC 语义 & 历史处理（决策汇总）

- **生成中按 ESC（或 Ctrl-C）** → `cancel()`：HTTP/SSE 读取因 ctx 取消立即报错返回；**同时**正在跑的 bash 命令被杀（`CommandContext`）。一键全停。ESC 与 Ctrl-C 在生成期间等价 —— 都打断本回合（因为我们关了 isig，Ctrl-C 不再是信号，由 watcher 自行解释）。
- **半句**：屏幕已显示的保留可见（让你知道说到哪），**不进历史**。
- **整回合回滚（两段配合，净效果 = 回到发问之前）**：
  - `agent.Run` 进入时记 `startLen := len(history)`（此时 history 已含本回合的 user 消息）；被取消时 `return history[:startLen], context.Canceled`，丢掉本回合追加的 assistant / tool 步。
  - `main` 见 `err == context.Canceled` 时，再 `history = history[:len(history)-1]` 丢掉那条 user 消息（**复用现有报错路径**，main.go:98-99 本就这么干），得干净提示符。
  - ⚠️ **已知简化**：已执行命令对磁盘的副作用无法撤销，这里只丢对话记录。ESC = 「这轮对话作废、回到干净提示符」，但磁盘上已发生的改动仍在。
- **打断后** `main` 打印一行 `（已打断）`（而非「调用出错」），靠 `err == context.Canceled` 区分。
- **idle 提示符按 ESC** → 清空当前行（标准 readline 习惯，`ReadLine` 内部处理）。
- **Ctrl-C**：idle 处 = 放弃当前行重来（`OutcomeInterrupt`）；生成 / 工具执行中 = 等价 ESC 打断本回合。**Ctrl-D 空行** → 退出（`OutcomeEOF`，等价旧的 EOF 语义）。

## 宽度与重绘

- 每次编辑后**整行重绘**：`\r` 回行首 → 重打提示符 + 缓冲 → `\033[K` 清到行尾 → 用「提示符显示宽 + 光标前各 rune 宽度之和」把光标定到正确列（`\r` 后 `\033[<col>C`）。避免逐键光标算术的脆弱。
- 光标定位、左右移动一律按 `runeWidth` 的**显示列**算（全角 2 列），从根上解决「半个残影」。
- 提示符含 ANSI 颜色码（零宽），其显示宽度按「剥掉转义序列后的可见字符」计算。

## 错误处理 / 边界

- **非 tty / stty 不可用**（如管道运行、CI）：`rawmode.enable` 失败时优雅降级回旧的 `ReadString` 行模式，功能照常、只是没有 raw 增强，绝不崩。
- **裸 ESC 与方向键区分**：tty 设为 `min 0 time 1`（VMIN=0 / VTIME=0.1s），pump 的 `Read` 因此带 0.1s 超时。pump 读到 `0x1b` 后再做一次读：0.1s 内若来 `[` → 继续读 CSI 序列（方向键 / Home / End）；超时（读到 0 字节）→ 判裸 ESC，立刻发 `KeyEsc`。代价：pump 空闲时每 0.1s 醒一次（CPU 可忽略）。**注意**：这正是 stty 必须用 `time 1` 而非纯阻塞 `min 1 time 0` 的原因 —— 纯阻塞下裸 ESC 要等下一个按键才能判定，打断会失灵。
- **`ConfirmTool` 的 `[Y/n]`** 同样改用这套输入（否则那里中文一样删不掉）；ESC 在确认处 = 拒绝该工具并中止本回合。
- **退出还原**：`restore()` 用 `defer` 覆盖正常返回与 panic 两条路径。但**信号杀进程时 defer 不执行**，故 `rawmode` 另起一个 `signal.Notify`（SIGINT / SIGTERM / SIGHUP）的 goroutine：收到信号 → 先 `restore()` 还原 stty → 再以默认行为退出。双保险确保**任何**退出路径都不把用户 shell 留在 raw 模式（无回显 / 乱码）。注意 isig 已关、Ctrl-C 不再产生 SIGINT，所以该 handler 主要兜底 `kill` / SIGTERM / SIGHUP。

## 测试策略

- `lineeditor`：喂字节 / Key 序列（含中文 3 字节 + 退格 + 方向键 + Ctrl-U/W），断言最终行字符串与渲染输出。**无需真 tty。**
- `width`：表驱动（CJK = 2 / ascii = 1 / 组合 = 0 / 制表等边界）。
- `keys` pump：喂原始字节，断言解码出的 Key 序列（含 `ESC[D` → KeyLeft、裸 ESC、UTF-8 多字节拼字）。
- `Run` 打断：假 Provider 阻塞在 ctx，假 UI 的 `WatchInterrupt` 立刻触发 cancel；断言 `Run` 返回 `context.Canceled` 且历史回滚到 `startLen`（本回合 assistant/tool 的 append 全没了、入参里的 user 消息仍在 —— 丢 user 那步由 main 负责，另测）。
- raw / stty 这层薄，靠手测 + 非 tty 环境跳过的冒烟测试。
- 全程保持 `go build ./...`、`go vet ./...`、`go test ./...`、`gofmt -l .` 干净。

## 改动清单（自底向上）

### 新增
- `agent/width.go`：`runeWidth(rune) int` + CJK/全角区间表。
- `agent/keys.go`：`Key` / `KeyKind` 类型；`pump(r io.Reader, out chan<- Key)`（UTF-8 + CSI 解码）。
- `agent/lineeditor.go`：`ReadLine(keys <-chan Key, out io.Writer, prompt string) (string, Outcome)`，纯逻辑（见上「ReadLine 返回的结局」）。
- `agent/rawmode.go`：`enableRaw() (restore func(), err error)`，封装 `stty -g` 存档 / `stty -icanon -isig -iexten -echo min 0 time 1` / 还原。

### 改动
- `agent/ui.go`：`UI` 接口加两个方法 —— `ReadLine(prompt string) (string, Outcome)`（取代 main 里的 `stdin.ReadString`）与 `WatchInterrupt(cancel context.CancelFunc) (stop func())`。`Outcome` 类型定义在 `lineeditor.go`。
- `agent/agent.go`：`Run` 进入记 `startLen`；用 `ui.WatchInterrupt` 把 `provider.Chat` 与 `tool.Run` 各自括起来（前后 `WithCancel` / `stop()`）；任一被取消则 `return history[:startLen], context.Canceled`。改动集中、不触碰工具循环主结构。
- `agent/terminal.go`：`TerminalUI` 构造时建 `keys` channel 并起 `pump`；实现 `ReadLine`（包 `lineeditor`）、`WatchInterrupt`（监听 goroutine）；`ConfirmTool` 的 y/n 改走 `lineeditor`（否则那里中文也删不掉），ESC = 拒绝该工具并中止本回合。`Sink`/`ToolOutput` 不变。
- `main.go`：启动 `rawmode.enableRaw()` + `defer restore()`；`stdin.ReadString` 换成 `ui.ReadLine`；`Run` 返回 `context.Canceled` 时打印 `（已打断）` 并丢掉该 user 消息；保留非 tty 降级路径（`enableRaw` 失败 → 旧的 `bufio` 行读 + 不支持打断）。

### 不改
- `llm/` 各文件（ctx 已就绪，无需动）；`tools/`（`CommandContext` 已就绪）；`config/`。

## 守住的约束

零第三方依赖（stty + 自写宽度表）；`agent.go` 改动集中在「用 `WatchInterrupt` 括住可取消操作 + 取消即回滚」，不改工具循环主结构；新文件各自一个概念、可独立单测；提交前四件套（build / vet / test / gofmt）干净。

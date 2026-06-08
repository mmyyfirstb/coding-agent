package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"zsh-agent/llm"
)

// Bash 是"执行一条本地 zsh 命令"的工具。
//
// 设计要点（务必理解，否则容易踩坑）：
//   - 每次 Run 都是一个全新的 `zsh -c <command>` 子进程。子进程退出后，它的
//     工作目录、环境变量、shell 函数 / alias 全部随之消失。所以 cd、export、
//     alias 这类"改变 shell 状态"的操作 **不会** 影响下一次调用。
//   - 如果确实需要状态连续（比如先 cd 再执行某命令），请把它们用 `&&` 串进
//     同一条 command 里，例如：`cd /tmp && pwd`。
//   - 本工具只负责"执行"。是否需要 y/n 让用户确认危险命令，是 UI 层 / agent
//     主循环的职责，不在这里实现。
type Bash struct{}

// 编译期断言：确保 Bash 满足 Tool 接口。若签名不符，这里会直接编译失败，
// 比等到运行时才发现要友好得多。
var _ Tool = Bash{}

// Spec 返回给模型看的工具声明。
//
// Description 用中文把"每次调用是独立子进程、状态不跨调用保留"这件事讲清楚，
// 让模型知道要保持上下文就得在一条命令里用 && 串起来。
func (Bash) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "bash",
		Description: "在本地执行一条 zsh 命令，返回合并后的 stdout+stderr 文本。\n" +
			"注意：每次调用都是一个独立的 `zsh -c` 子进程，因此 cd / export / alias " +
			"等对 shell 状态的修改不会在多次调用之间保留。如果需要状态连续，请用 `&&` " +
			"把多条命令串进同一次调用，例如 `cd /tmp && ls`。",
		// 参数 schema：只有一个必填字符串 command。
		// 这里用 json.RawMessage 直接写死 JSON，避免引入额外依赖。
		Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"要执行的 zsh 命令"}},"required":["command"]}`),
	}
}

// Run 执行模型给定的命令。
//
// 返回值约定（对接 Tool 接口语义）：
//   - 返回 error != nil 时，调用方会把它当作"工具失败"回填给模型。
//   - 所以这里要小心区分两类"出错"：
//     1) 命令本身以非零退出码结束（如 `exit 7`、`grep 没匹配到`）。这其实是
//     一个**正常的执行结果**，模型需要看到退出码来判断下一步，因此我们把
//     退出码附在输出末尾，并把 error 清空（runErr = nil）。
//     2) 连子进程都没起来（如系统里压根没有 zsh、无法 fork）。这才是真正的
//     工具失败，保留 error 原样返回。
func (Bash) Run(ctx context.Context, args json.RawMessage) (string, error) {
	// 1. 解析参数。
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}
	// 空命令直接拒绝：执行空字符串没有意义，且容易掩盖模型的参数错误。
	if strings.TrimSpace(in.Command) == "" {
		return "", errors.New("command 不能为空")
	}

	// 2. 执行命令。
	// 用 CommandContext，这样上层 ctx 被取消 / 超时时，子进程会被杀掉。
	// CombinedOutput 把 stdout 和 stderr 合并成一份文本——模型只需要看"发生了
	// 什么"，不需要区分两个流，合并反而更省事。
	out, runErr := exec.CommandContext(ctx, "zsh", "-c", in.Command).CombinedOutput()
	result := string(out)

	// 3. 处理执行结果。
	if runErr != nil {
		// 是不是"命令跑完了但退出码非零"？是的话当作正常结果。
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			// 把退出码附到输出末尾，让模型能看到，并清空 error（不算工具失败）。
			result += fmt.Sprintf("\n[退出码 %d]", ee.ExitCode())
			runErr = nil
		}
		// 否则（比如无法启动 zsh）保留 runErr，按工具失败返回给上层。
	}

	// 4. 兜底：命令成功但没有任何输出（如 `true`），给个占位文本，
	// 免得模型收到一个空字符串不知所措。
	if strings.TrimSpace(result) == "" {
		result = "（无输出）"
	}

	return result, runErr
}

// zsh-agent: 一个最小但结构清晰、便于学习与扩展的命令行 agent。
//
// 它通过「OpenAI 兼容」接口（/v1/chat/completions）调用一个大模型作为大脑，
// 让模型调用 bash 工具来操作本地 zsh，完成用户用自然语言提出的需求。
//
// 全项目按"会变的东西各用一个接口隔离"来分层，main 只负责把它们组装起来：
//
//	config.Load        读 JSON 配置（后端地址 / key / 模型 / …）
//	      │
//	      ▼
//	llm.OpenAIProvider 把"问模型"封装成统一的 Provider 接口（含纯 IP 自动免校验 TLS）
//	tools.Registry     注册可供模型调用的工具（这里只有 bash）
//	agent.TerminalUI   负责与终端用户交互（展示 / y-n 确认）
//	      │
//	      ▼
//	agent.Agent.Run    核心循环：问模型→执行工具→喂回结果，直到模型收尾
//
// 想换模型后端 / 加工具 / 改交互方式，分别替换上面对应的一块即可，main 与核心循环基本不动。
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"zsh-agent/agent"
	"zsh-agent/config"
	"zsh-agent/llm"
	"zsh-agent/tools"
)

// defaultSystemPrompt 是配置里没写 system_prompt 时使用的内置系统提示。
const defaultSystemPrompt = `你是一个帮用户操作本地 zsh 的助手。
通过调用 bash 工具来执行命令。
注意：每次调用都是独立的 zsh -c 子进程——cd、export、alias 都不会在多次调用之间保留。
如果需要状态延续，把多条命令用 && 串在一条里发过来。
文字回复保持简短，主要工作通过调用工具完成。`

func main() {
	// 1. 读配置：默认读项目根目录的 config.json，可用环境变量 AGENT_CONFIG 覆盖路径。
	path := os.Getenv("AGENT_CONFIG")
	if path == "" {
		path = "config.json"
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "加载配置失败:", err)
		fmt.Fprintln(os.Stderr, "提示：可参考 config.example.json 创建 config.json，或用 AGENT_CONFIG 指定路径。")
		os.Exit(1)
	}

	// 2. 共享一个 stdin 读取器。
	//    关键：终端 UI 在确认工具时也要读 stdin，若 main 与 UI 各自 new 一个
	//    bufio.Reader，两个缓冲区会互相"偷"对方还没读的输入。
	//    这里只建一个 *bufio.Reader 传给 UI——bufio.NewReader 收到已经是
	//    *bufio.Reader 的入参时会原样返回，于是全程共用同一个缓冲区，不会打架。
	stdin := bufio.NewReader(os.Stdin)

	// 3. 按配置组装各层。
	provider := llm.NewOpenAIProvider(cfg.BaseURL, cfg.APIKey, cfg.Model, cfg.MaxTokens, cfg.Stream, cfg.InsecureSkipVerify)
	reg := tools.NewRegistry()
	reg.Register(tools.Bash{}) // 想加工具？实现 tools.Tool 后在这里再 Register 一行即可。
	ui := agent.NewTerminalUI(stdin, os.Stdout)
	ag := agent.New(provider, reg, ui)

	// 4. 初始对话历史：开头放一条 system 消息。
	system := cfg.SystemPrompt
	if system == "" {
		system = defaultSystemPrompt
	}
	history := []llm.Message{{Role: llm.RoleSystem, Content: system}}

	// 5. REPL：读一行用户输入 → 跑一个 agent 回合 → 循环，Ctrl+D 退出。
	fmt.Printf("zsh-agent 已就绪（后端 %s，模型 %s）。输入需求后回车，Ctrl+D 退出。\n", cfg.BaseURL, cfg.Model)
	for {
		// 青色「你 ›」标签：和模型回复的绿色「助手」标签对应，
		// 让回看屏幕时一眼能分清哪行是自己输入的。你打的字保持默认色。
		fmt.Print("\n\033[36m你 ›\033[0m ")
		line, err := stdin.ReadString('\n')
		if err == io.EOF {
			fmt.Println()
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// 把用户输入作为一条 user 消息加进历史。
		history = append(history, llm.Message{Role: llm.RoleUser, Content: line})

		updated, err := ag.Run(context.Background(), history)
		if err != nil {
			fmt.Fprintln(os.Stderr, "调用出错:", err)
			// 出错时丢掉刚才那条 user 消息，让用户能干净重试。
			history = history[:len(history)-1]
			continue
		}
		history = updated
	}
}

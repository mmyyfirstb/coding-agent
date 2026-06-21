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
	"context"
	"fmt"
	"os"

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
	// 1. 读配置。
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

	// 2. 尝试进 raw 模式：成功则启用 rune/宽度行编辑 + ESC 打断；失败（非 tty /
	//    无 stty）则降级为普通行模式。defer 还原，保证退出时终端干净。
	restore, raw := agent.EnableRaw()
	defer restore()

	// 3. 按配置组装各层。
	provider := llm.NewOpenAIProvider(cfg.BaseURL, cfg.APIKey, cfg.Model, cfg.MaxTokens, cfg.Stream, cfg.InsecureSkipVerify)
	reg := tools.NewRegistry()
	reg.Register(tools.Bash{}) // 想加工具？实现 tools.Tool 后在这里再 Register 一行即可。

	var ui agent.UI
	if raw {
		// 把 os.Stdin 适配回 raw tty 应有的「超时=(0,nil)」语义：否则 Go 的 os.File 会把
		// VMIN=0/VTIME 的读超时当 io.EOF，让输入 pump 在第一次空闲超时就误判流结束并退出。
		ui = agent.NewRawTerminalUI(agent.PollingTTYReader(os.Stdin), os.Stdout)
	} else {
		ui = agent.NewTerminalUI(os.Stdin, os.Stdout)
	}
	ag := agent.New(provider, reg, ui)

	// 4. 初始对话历史：开头放一条 system 消息。
	system := cfg.SystemPrompt
	if system == "" {
		system = defaultSystemPrompt
	}
	history := []llm.Message{{Role: llm.RoleSystem, Content: system}}

	// 5. REPL：读一行 → 跑一个回合 → 循环。
	fmt.Printf("zsh-agent 已就绪（后端 %s，模型 %s）。输入需求后回车，Ctrl+D 退出。\n", cfg.BaseURL, cfg.Model)
	if raw {
		fmt.Println("（生成中可按 ESC 或 Ctrl-C 打断）")
	}
	for {
		line, oc := ui.ReadLine("\033[36m你 ›\033[0m ")
		if oc == agent.OutcomeEOF {
			fmt.Println()
			return
		}
		if oc == agent.OutcomeInterrupt {
			continue // 放弃本行，重新给提示符
		}
		if line == "" {
			continue
		}

		history = append(history, llm.Message{Role: llm.RoleUser, Content: line})

		updated, err := ag.Run(context.Background(), history)
		if err == context.Canceled {
			fmt.Println("（已打断）")
			// 保留你这条输入，并补一条 assistant 占位标记「本回合被打断」：
			// 这样下回合模型仍记得你问过什么，且保持 user/assistant 交替合法。
			// updated 是 Run 回滚后的历史——含本回合 user、不含半截回复（见 Agent.Run）。
			history = append(updated, llm.Message{Role: llm.RoleAssistant, Content: "（上一条回复被你打断了）"})
			continue
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "调用出错:", err)
			history = history[:len(history)-1] // 出错时丢掉刚才那条 user，便于干净重试
			continue
		}
		history = updated
	}
}

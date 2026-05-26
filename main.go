// zsh-agent: 用 Anthropic Messages API 操作本地 zsh 的最小 agent。
//
// 数据流概览：
//
//   用户输入 ──▶ messages ──▶ POST /v1/messages ──▶ Claude
//                  ▲                                    │
//                  │           stop_reason != tool_use  │
//                  │       ┌────────────────────────────┘
//                  │       ▼ 否则
//                  │   读取 tool_use blocks → 用户确认 → zsh -c
//                  │       │
//                  └────── 把 tool_result 追加进 messages，继续调
//
// 关键概念：
//   - messages 是整个对话历史，每次调 API 都把完整历史一起发过去（API 本身无状态）
//   - Claude 返回的 content 是若干 ContentBlock，可能混合 text + tool_use
//   - 工具调用结果以 tool_result block 的形式由我们追加为下一条 user 消息

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
)

const (
	apiURL       = "https://api.anthropic.com/v1/messages"
	defaultModel = "claude-sonnet-4-6"
	apiVersion   = "2023-06-01"
	maxTokens    = 4096
)

const systemPrompt = `你是一个帮用户操作本地 zsh 的助手。
通过调用 bash 工具来执行命令。
注意：每次调用都是独立的 zsh -c 子进程——cd、export、alias 都不会在多次调用之间保留。
如果需要状态延续，把多条命令用 && 串在一条里发过来。
文字回复保持简短，主要工作通过 tool_use 完成。`

// ====================================================================
// 协议类型：和 Anthropic Messages API 报文一一对应
// ====================================================================

// ContentBlock 是 Anthropic 协议中"消息内容"的基本单位。
//
// 一条 Message 的 Content 是一个 ContentBlock 数组，通过 Type 区分形态：
//
//   Type = "text"        ── 纯文本（assistant 和 user 都可能用）
//                           关键字段：Text
//
//   Type = "tool_use"    ── Claude 请求调用工具（assistant → 我们）
//                           关键字段：ID（本次调用的唯一标识）
//                                   Name（工具名）
//                                   Input（参数 JSON，按 Tool.InputSchema 填）
//
//   Type = "tool_result" ── 我们把工具执行结果送回（user → Claude）
//                           关键字段：ToolUseID（对应哪次 tool_use 的 ID）
//                                   Content（结果文本）
//                                   IsError（执行是否失败）
//
// 这里用一个 struct + omitempty 装下三种形态是为了少写代码；
// 学习时记得：每种 Type 只有对应那几个字段有意义，其它都不会出现在线上报文里。
type ContentBlock struct {
	Type string `json:"type"`

	// text block 字段
	Text string `json:"text,omitempty"`

	// tool_use block 字段（由 Claude 填）
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result block 字段（由我们填）
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// 工厂函数：让调用处一眼能看出在构造哪一种 block。
func textBlock(s string) ContentBlock {
	return ContentBlock{Type: "text", Text: s}
}

func toolResultBlock(toolUseID, content string, isError bool) ContentBlock {
	return ContentBlock{
		Type:      "tool_result",
		ToolUseID: toolUseID,
		Content:   content,
		IsError:   isError,
	}
}

// Message 是一条对话消息：来自用户还是助手，加上内容数组。
type Message struct {
	Role    string         `json:"role"` // "user" 或 "assistant"
	Content []ContentBlock `json:"content"`
}

// Tool 是声明给 Claude 看的"我有哪些工具可以调"。
// InputSchema 是工具参数的 JSON Schema，Claude 会按这个 schema 生成 Input。
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type apiRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system,omitempty"`
	Messages  []Message `json:"messages"`
	Tools     []Tool    `json:"tools,omitempty"`
}

type apiResponse struct {
	Content []ContentBlock `json:"content"`

	// StopReason 告诉我们 Claude 为什么停下：
	//   "end_turn"   ── 说完了，等用户接话
	//   "tool_use"   ── 想调用工具，等我们把 tool_result 发回去
	//   "max_tokens" ── token 用完了
	StopReason string `json:"stop_reason"`

	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// ====================================================================
// 工具定义
// ====================================================================

var bashTool = Tool{
	Name: "bash",
	Description: "在用户本地执行一条 zsh 命令。返回 stdout+stderr 合并的文本和退出码。" +
		"注意：每次调用都是独立的 zsh -c 子进程，cd/export/alias 不会跨调用保留。",
	InputSchema: json.RawMessage(`{
		"type": "object",
		"properties": {
			"command": {
				"type": "string",
				"description": "要执行的 zsh 命令"
			}
		},
		"required": ["command"]
	}`),
}

// ====================================================================
// API 调用
// ====================================================================

// callAPI 把当前消息历史发送到 Anthropic Messages API，返回 Claude 的回复。
func callAPI(messages []Message) (*apiResponse, error) {
	body, err := json.Marshal(apiRequest{
		Model:     defaultModel,
		MaxTokens: maxTokens,
		System:    systemPrompt,
		Messages:  messages,
		Tools:     []Tool{bashTool},
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", os.Getenv("ANTHROPIC_API_KEY"))
	req.Header.Set("anthropic-version", apiVersion)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var out apiResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w（原始报文: %s）", err, string(raw))
	}
	if out.Error != nil {
		return nil, fmt.Errorf("API 报错 %s: %s", out.Error.Type, out.Error.Message)
	}
	return &out, nil
}

// ====================================================================
// 工具执行
// ====================================================================

// runCommand 向用户展示命令，等待 y/n 确认后用 zsh -c 执行。
// 返回 (输出文本, 是否被用户拒绝)。
func runCommand(cmd string, reader *bufio.Reader) (string, bool) {
	fmt.Printf("\n\033[33m$ %s\033[0m\n", cmd)
	fmt.Print("执行？[Y/n] ")
	line, _ := reader.ReadString('\n')
	switch strings.TrimSpace(strings.ToLower(line)) {
	case "n", "no":
		fmt.Println("（已拒绝）")
		return "用户拒绝执行此命令。", true
	}

	c := exec.Command("zsh", "-c", cmd)
	out, err := c.CombinedOutput()
	result := string(out)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result += fmt.Sprintf("\n[退出码 %d]", exitErr.ExitCode())
		} else {
			result += fmt.Sprintf("\n[执行错误: %v]", err)
		}
	}
	if strings.TrimSpace(result) == "" {
		result = "（无输出）"
	}
	fmt.Print(result)
	if !strings.HasSuffix(result, "\n") {
		fmt.Println()
	}
	return result, false
}

// ====================================================================
// Agent 主循环
// ====================================================================

// runAgentLoop 处理用户的一个回合：
// 反复调 API，每次返回 tool_use 就执行工具，把 tool_result 续到 messages 里再调，
// 直到 Claude 不再请求工具（stop_reason != "tool_use"）。
//
// 返回更新后的 messages（已包含本回合所有 assistant 回复和 tool_result）。
func runAgentLoop(messages []Message, reader *bufio.Reader) ([]Message, error) {
	for {
		resp, err := callAPI(messages)
		if err != nil {
			return messages, err
		}

		// 1. 把 assistant 的整段回复（text + tool_use 混合）记进历史。
		messages = append(messages, Message{Role: "assistant", Content: resp.Content})

		// 2. 把 assistant 的文字部分打印给用户看。
		for _, b := range resp.Content {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				fmt.Println(b.Text)
			}
		}

		// 3. 如果没有 tool 调用请求，本回合结束。
		if resp.StopReason != "tool_use" {
			return messages, nil
		}

		// 4. 有 tool_use：依次执行每一个，结果打包成一条 user 消息发回。
		var results []ContentBlock
		for _, b := range resp.Content {
			if b.Type != "tool_use" {
				continue
			}
			if b.Name != "bash" {
				results = append(results, toolResultBlock(b.ID, "未知工具: "+b.Name, true))
				continue
			}
			var input struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal(b.Input, &input); err != nil {
				results = append(results, toolResultBlock(b.ID, "参数 JSON 解析失败: "+err.Error(), true))
				continue
			}
			out, declined := runCommand(input.Command, reader)
			results = append(results, toolResultBlock(b.ID, out, declined))
		}
		messages = append(messages, Message{Role: "user", Content: results})
	}
}

// ====================================================================
// 入口
// ====================================================================

func main() {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		fmt.Fprintln(os.Stderr, "未设置 ANTHROPIC_API_KEY 环境变量")
		os.Exit(1)
	}

	reader := bufio.NewReader(os.Stdin)
	var messages []Message
	fmt.Println("zsh-agent 已就绪。输入需求后回车，Ctrl+D 退出。")

	for {
		fmt.Print("\n\033[36m>\033[0m ")
		userLine, err := reader.ReadString('\n')
		if err == io.EOF {
			fmt.Println()
			return
		}
		userLine = strings.TrimSpace(userLine)
		if userLine == "" {
			continue
		}

		// 用户输入作为一条 user message 加进历史。
		messages = append(messages, Message{
			Role:    "user",
			Content: []ContentBlock{textBlock(userLine)},
		})

		// 进入 agent 多步循环，直到 Claude 给出最终文字回复。
		updated, err := runAgentLoop(messages, reader)
		if err != nil {
			fmt.Fprintf(os.Stderr, "API 错误: %v\n", err)
			// 出错时丢掉刚才那条 user 消息，让用户能干净地重试。
			messages = messages[:len(messages)-1]
			continue
		}
		messages = updated
	}
}

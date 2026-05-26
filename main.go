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
	apiURL        = "https://api.anthropic.com/v1/messages"
	defaultModel  = "claude-sonnet-4-6"
	apiVersion    = "2023-06-01"
	maxTokens     = 4096
	systemPrompt  = "You operate the user's local zsh shell via the `bash` tool. " +
		"Each call runs in a fresh zsh -c subprocess — cd, export, aliases do NOT persist between calls. " +
		"If you need to chain stateful operations, combine them in one command with &&. " +
		"Keep your text responses brief; do the work via tool calls."
)

// ContentBlock is a polymorphic block used in both directions.
// Fields are populated based on Type; unused fields are omitted on the wire.
type ContentBlock struct {
	Type string `json:"type"`

	// text block
	Text string `json:"text,omitempty"`

	// tool_use block (assistant -> us)
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result block (us -> assistant)
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

type Message struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

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
	Content    []ContentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

var bashTool = Tool{
	Name: "bash",
	Description: "Execute a zsh command on the user's local machine. " +
		"Returns combined stdout+stderr plus exit status. " +
		"Each invocation is a fresh `zsh -c` subprocess: cd/export/aliases do not persist.",
	InputSchema: json.RawMessage(`{
		"type": "object",
		"properties": {
			"command": {
				"type": "string",
				"description": "The zsh command to execute."
			}
		},
		"required": ["command"]
	}`),
}

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
		return nil, fmt.Errorf("api %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var out apiResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w (body: %s)", err, string(raw))
	}
	if out.Error != nil {
		return nil, fmt.Errorf("api error %s: %s", out.Error.Type, out.Error.Message)
	}
	return &out, nil
}

// runCommand prompts the user, then either executes the command or returns
// a declined result. The boolean is true when the user declined.
func runCommand(cmd string, reader *bufio.Reader) (string, bool) {
	fmt.Printf("\n\033[33m$ %s\033[0m\n", cmd)
	fmt.Print("Run? [Y/n] ")
	line, _ := reader.ReadString('\n')
	switch strings.TrimSpace(strings.ToLower(line)) {
	case "n", "no":
		fmt.Println("(declined)")
		return "User declined to run this command.", true
	}

	c := exec.Command("zsh", "-c", cmd)
	out, err := c.CombinedOutput()
	result := string(out)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result += fmt.Sprintf("\n[exit %d]", exitErr.ExitCode())
		} else {
			result += fmt.Sprintf("\n[exec error: %v]", err)
		}
	}
	if strings.TrimSpace(result) == "" {
		result = "(no output)"
	}
	fmt.Print(result)
	if !strings.HasSuffix(result, "\n") {
		fmt.Println()
	}
	return result, false
}

func main() {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		fmt.Fprintln(os.Stderr, "ANTHROPIC_API_KEY not set")
		os.Exit(1)
	}

	reader := bufio.NewReader(os.Stdin)
	var messages []Message
	fmt.Println("zsh-agent ready. Type a request; Ctrl+D to exit.")

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

		messages = append(messages, Message{
			Role:    "user",
			Content: []ContentBlock{{Type: "text", Text: userLine}},
		})

		// Inner loop: keep calling the API until the assistant stops requesting tools.
		for {
			resp, err := callAPI(messages)
			if err != nil {
				fmt.Fprintf(os.Stderr, "api error: %v\n", err)
				messages = messages[:len(messages)-1] // drop the user turn so retry is clean
				break
			}
			messages = append(messages, Message{Role: "assistant", Content: resp.Content})

			// Print any prose the assistant included.
			for _, b := range resp.Content {
				if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
					fmt.Println(b.Text)
				}
			}

			if resp.StopReason != "tool_use" {
				break
			}

			// Execute every tool_use block in this turn and send the results back.
			var results []ContentBlock
			for _, b := range resp.Content {
				if b.Type != "tool_use" {
					continue
				}
				if b.Name != "bash" {
					results = append(results, ContentBlock{
						Type: "tool_result", ToolUseID: b.ID,
						Content: "unknown tool: " + b.Name, IsError: true,
					})
					continue
				}
				var input struct {
					Command string `json:"command"`
				}
				if err := json.Unmarshal(b.Input, &input); err != nil {
					results = append(results, ContentBlock{
						Type: "tool_result", ToolUseID: b.ID,
						Content: "invalid input json: " + err.Error(), IsError: true,
					})
					continue
				}
				out, declined := runCommand(input.Command, reader)
				results = append(results, ContentBlock{
					Type: "tool_result", ToolUseID: b.ID,
					Content: out, IsError: declined,
				})
			}
			messages = append(messages, Message{Role: "user", Content: results})
		}
	}
}

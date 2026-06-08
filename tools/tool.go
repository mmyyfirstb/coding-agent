// Package tools 定义工具抽象与注册表。
//
// "工具"是 agent 能在外部世界做的事（执行命令、读文件、发请求…）。
// 加一个新工具 = 实现 Tool 接口 + 在 main 里 Register 一行，核心循环零改动。
package tools

import (
	"context"
	"encoding/json"

	"zsh-agent/llm"
)

// Tool 是一个可被模型调用的工具。
type Tool interface {
	// Spec 返回给模型看的工具声明（名字、描述、参数 schema）。
	Spec() llm.ToolSpec
	// Run 真正执行工具。args 是模型按 Spec().Parameters 生成的参数 JSON。
	// 返回执行结果文本；返回 error 时，调用方会把它当作"工具失败"回填给模型。
	Run(ctx context.Context, args json.RawMessage) (string, error)
}

// Registry 管理已注册的工具，按名字查找。
type Registry struct {
	tools map[string]Tool
}

// NewRegistry 创建一个空注册表。
func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

// Register 注册一个工具（以 Spec().Name 为键，重名会覆盖）。
func (r *Registry) Register(t Tool) {
	r.tools[t.Spec().Name] = t
}

// Get 按名字取工具。
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Specs 返回所有已注册工具的声明，供 Provider 发给模型。
func (r *Registry) Specs() []llm.ToolSpec {
	specs := make([]llm.ToolSpec, 0, len(r.tools))
	for _, t := range r.tools {
		specs = append(specs, t.Spec())
	}
	return specs
}

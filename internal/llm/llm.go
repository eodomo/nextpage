// Package llm defines a provider-neutral chat interface. The agent loop only
// talks to Provider, so adding a new backend (OpenAI-compatible, Anthropic,
// ...) means implementing this interface and registering it in New.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
)

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type ToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	Thinking   string     `json:"thinking,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolName   string     `json:"tool_name,omitempty"`
}

// ToolSpec describes a tool to the model. Schema is a JSON Schema object.
type ToolSpec struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

type Request struct {
	Model    string
	Messages []Message
	Tools    []ToolSpec
	Think    bool
	Options  map[string]any
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

func (u *Usage) Add(o Usage) {
	u.PromptTokens += o.PromptTokens
	u.CompletionTokens += o.CompletionTokens
}

// Delta is an incremental piece of streamed output.
type Delta struct {
	Content  string
	Thinking string
}

type Response struct {
	Message    Message
	Usage      Usage
	DoneReason string
}

type Provider interface {
	Name() string
	// Chat streams a completion, calling onDelta for each text/thinking
	// fragment, and returns the fully assembled message (including any tool
	// calls) when the model is done.
	Chat(ctx context.Context, req Request, onDelta func(Delta)) (Response, error)
	ListModels(ctx context.Context) ([]string, error)
}

type ProviderConfig struct {
	Kind     string // "ollama" (default)
	BaseURL  string
	Username string
	Password string
}

func New(cfg ProviderConfig) (Provider, error) {
	switch cfg.Kind {
	case "", "ollama":
		return NewOllama(cfg)
	default:
		return nil, fmt.Errorf("unknown provider %q", cfg.Kind)
	}
}

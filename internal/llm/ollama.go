package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"

	"github.com/ollama/ollama/api"
)

type basicAuthTransport struct {
	username string
	password string
	base     http.RoundTripper
}

func (t *basicAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.SetBasicAuth(t.username, t.password)
	return t.base.RoundTrip(req)
}

type Ollama struct {
	client *api.Client
}

func NewOllama(cfg ProviderConfig) (*Ollama, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("no server URL configured (set OLLAMASERVER or \"server\" in settings)")
	}
	base, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse server URL: %w", err)
	}
	httpClient := &http.Client{}
	if cfg.Username != "" || cfg.Password != "" {
		httpClient.Transport = &basicAuthTransport{
			username: cfg.Username,
			password: cfg.Password,
			base:     http.DefaultTransport,
		}
	}
	return &Ollama{client: api.NewClient(base, httpClient)}, nil
}

func (o *Ollama) Name() string { return "ollama" }

func (o *Ollama) ListModels(ctx context.Context) ([]string, error) {
	resp, err := o.client.List(ctx)
	if err != nil {
		return nil, explain(err)
	}
	names := make([]string, 0, len(resp.Models))
	for _, m := range resp.Models {
		names = append(names, m.Name)
	}
	sort.Strings(names)
	return names, nil
}

func (o *Ollama) Chat(ctx context.Context, req Request, onDelta func(Delta)) (Response, error) {
	tools, err := toOllamaTools(req.Tools)
	if err != nil {
		return Response{}, err
	}
	stream := true
	areq := &api.ChatRequest{
		Model:    req.Model,
		Messages: toOllamaMessages(req.Messages),
		Stream:   &stream,
		Tools:    tools,
		Options:  req.Options,
	}
	if req.Think != nil {
		areq.Think = &api.ThinkValue{Value: *req.Think}
	}

	var out Response
	out.Message.Role = RoleAssistant
	err = o.client.Chat(ctx, areq, func(resp api.ChatResponse) error {
		m := resp.Message
		if m.Content != "" || m.Thinking != "" {
			out.Message.Content += m.Content
			out.Message.Thinking += m.Thinking
			if onDelta != nil {
				onDelta(Delta{Content: m.Content, Thinking: m.Thinking})
			}
		}
		for _, tc := range m.ToolCalls {
			args, _ := json.Marshal(tc.Function.Arguments)
			id := tc.ID
			if id == "" {
				id = fmt.Sprintf("call_%d", len(out.Message.ToolCalls))
			}
			out.Message.ToolCalls = append(out.Message.ToolCalls, ToolCall{
				ID:   id,
				Name: tc.Function.Name,
				Args: args,
			})
		}
		if resp.Done {
			out.DoneReason = resp.DoneReason
			out.Usage = Usage{
				PromptTokens:     resp.PromptEvalCount,
				CompletionTokens: resp.EvalCount,
			}
		}
		return nil
	})
	return out, explain(err)
}

// explain turns HTTP auth failures, which Ollama's client reports vaguely,
// into an actionable message.
func explain(err error) error {
	code := 0
	var se api.StatusError
	var sp *api.StatusError
	var ae api.AuthorizationError
	var ap *api.AuthorizationError
	switch {
	case errors.As(err, &se):
		code = se.StatusCode
	case errors.As(err, &sp):
		code = sp.StatusCode
	case errors.As(err, &ae):
		code = ae.StatusCode
	case errors.As(err, &ap):
		code = ap.StatusCode
	}
	if code == http.StatusUnauthorized || code == http.StatusForbidden {
		return fmt.Errorf("the server rejected the login (HTTP %d); check the basic auth username and password", code)
	}
	return err
}

func toOllamaMessages(msgs []Message) []api.Message {
	out := make([]api.Message, 0, len(msgs))
	for _, m := range msgs {
		am := api.Message{
			Role:       string(m.Role),
			Content:    m.Content,
			Thinking:   m.Thinking,
			ToolName:   m.ToolName,
			ToolCallID: m.ToolCallID,
		}
		for i, tc := range m.ToolCalls {
			args := api.NewToolCallFunctionArguments()
			if len(tc.Args) > 0 {
				_ = json.Unmarshal(tc.Args, &args)
			}
			am.ToolCalls = append(am.ToolCalls, api.ToolCall{
				ID: tc.ID,
				Function: api.ToolCallFunction{
					Index:     i,
					Name:      tc.Name,
					Arguments: args,
				},
			})
		}
		out = append(out, am)
	}
	return out
}

func toOllamaTools(specs []ToolSpec) (api.Tools, error) {
	var tools api.Tools
	for _, s := range specs {
		var params api.ToolFunctionParameters
		if err := json.Unmarshal(s.Schema, &params); err != nil {
			return nil, fmt.Errorf("tool %s: bad schema: %w", s.Name, err)
		}
		if params.Properties == nil {
			params.Properties = api.NewToolPropertiesMap()
		}
		tools = append(tools, api.Tool{
			Type: "function",
			Function: api.ToolFunction{
				Name:        s.Name,
				Description: s.Description,
				Parameters:  params,
			},
		})
	}
	return tools, nil
}

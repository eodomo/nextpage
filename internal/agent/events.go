package agent

import (
	"encoding/json"

	"github.com/eodomo/nextpage/internal/llm"
	"github.com/eodomo/nextpage/internal/tools"
)

// Event is anything the agent reports while running. Front ends (the TUI,
// print mode) receive them through the emit callback passed to Run.
// Events with a Reply channel block the agent until answered.
type Event interface{ isEvent() }

// Every event carries the name of the agent that produced it: "" for the
// main agent, or the sub-agent's task description.
type TextDelta struct {
	Agent string
	Text  string
}

type ThinkingDelta struct {
	Agent string
	Text  string
}

// AssistantMessage is the complete assistant message after streaming ends.
type AssistantMessage struct {
	Agent   string
	Message llm.Message
}

type ToolStart struct {
	Agent   string
	ID      string
	Name    string
	Subject string
	Input   json.RawMessage
}

type ToolEnd struct {
	Agent  string
	ID     string
	Name   string
	Result tools.Result
}

type RememberScope int

const (
	RememberNone    RememberScope = iota
	RememberSession               // until the program exits
	RememberProject               // saved to .nextpage/settings.local.json
)

type PermissionReply struct {
	Allow    bool
	Remember RememberScope
	Feedback string // optional guidance when denying
}

type PermissionRequest struct {
	Agent      string
	ToolName   string
	Subject    string
	Input      json.RawMessage
	Preview    string // e.g. a diff of the pending edit
	Suggestion string // rule that "don't ask again" would add
	Reply      chan PermissionReply
}

type QuestionRequest struct {
	Question tools.Question
	Reply    chan string
}

type PlanRequest struct {
	Plan  string
	Reply chan bool
}

type NoticeLevel int

const (
	NoticeInfo NoticeLevel = iota
	NoticeWarn
	NoticeError
)

// Notice is a message for the user that is not part of the conversation
// (hook output, compaction, retries...).
type Notice struct {
	Level NoticeLevel
	Text  string
}

type UsageUpdate struct {
	Total         llm.Usage
	ContextTokens int
}

type TodosUpdate struct {
	Todos []tools.Todo
}

func (TextDelta) isEvent()         {}
func (ThinkingDelta) isEvent()     {}
func (AssistantMessage) isEvent()  {}
func (ToolStart) isEvent()         {}
func (ToolEnd) isEvent()           {}
func (PermissionRequest) isEvent() {}
func (QuestionRequest) isEvent()   {}
func (PlanRequest) isEvent()       {}
func (Notice) isEvent()            {}
func (UsageUpdate) isEvent()       {}
func (TodosUpdate) isEvent()       {}

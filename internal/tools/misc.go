package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// ---- WebFetch ----

type WebFetchTool struct{}

func (WebFetchTool) Name() string { return "WebFetch" }
func (WebFetchTool) Kind() Kind   { return KindNetwork }
func (WebFetchTool) Description() string {
	return `Fetches a URL and returns its content as plain text (HTML is stripped to readable text).
Use it to read documentation or other web pages the user references. HTTP URLs are upgraded to HTTPS.`
}
func (WebFetchTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","description":"The URL to fetch"}},"required":["url"]}`)
}
func (WebFetchTool) Subject(in json.RawMessage) string {
	a, _ := decode[struct{ URL string }](in)
	return a.URL
}

var (
	reScriptStyle = regexp.MustCompile(`(?is)<(script|style|noscript|svg)[^>]*>.*?</(script|style|noscript|svg)>`)
	reBlockTags   = regexp.MustCompile(`(?i)</?(p|div|br|h[1-6]|li|tr|section|article|header|footer|pre|blockquote)[^>]*>`)
	reTags        = regexp.MustCompile(`(?s)<[^>]+>`)
	reBlankLines  = regexp.MustCompile(`\n\s*\n\s*\n+`)
	reSpaces      = regexp.MustCompile(`[ \t]+`)
)

func (WebFetchTool) Run(ctx context.Context, env *Env, in json.RawMessage) Result {
	a, err := decode[struct{ URL string }](in)
	if err != nil {
		return Errorf("%v", err)
	}
	u := strings.TrimSpace(a.URL)
	if strings.HasPrefix(u, "http://") {
		u = "https://" + strings.TrimPrefix(u, "http://")
	} else if !strings.HasPrefix(u, "https://") {
		u = "https://" + u
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Errorf("%v", err)
	}
	req.Header.Set("User-Agent", "nextpage/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Errorf("%v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return Errorf("%v", err)
	}
	text := string(body)
	if strings.Contains(resp.Header.Get("Content-Type"), "html") {
		text = htmlToText(text)
	}
	out := fmt.Sprintf("URL: %s\nStatus: %s\n\n%s", resp.Request.URL, resp.Status, truncate(text, 100000))
	return Result{
		Output:  out,
		Display: fmt.Sprintf("%s (%d bytes)", resp.Status, len(body)),
		IsError: resp.StatusCode >= 400,
	}
}

func htmlToText(s string) string {
	s = reScriptStyle.ReplaceAllString(s, "")
	s = reBlockTags.ReplaceAllString(s, "\n")
	s = reTags.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = reSpaces.ReplaceAllString(s, " ")
	s = reBlankLines.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// ---- TodoWrite ----

type TodoWriteTool struct{}

func (TodoWriteTool) Name() string { return "TodoWrite" }
func (TodoWriteTool) Kind() Kind   { return KindInternal }
func (TodoWriteTool) Description() string {
	return `Creates and updates a structured task list for the current session. Use it proactively for multi-step tasks (3+ steps) so progress is visible to the user.
- Always send the complete list; it replaces the previous one.
- Each todo has content (imperative, e.g. "Run tests"), activeForm (present continuous, e.g. "Running tests") and status: pending, in_progress or completed.
- Keep exactly one task in_progress at a time and mark tasks completed as soon as they are done.`
}
func (TodoWriteTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"todos":{"type":"array","items":{"type":"object","properties":{
"content":{"type":"string"},
"status":{"type":"string","enum":["pending","in_progress","completed"]},
"activeForm":{"type":"string"}},"required":["content","status"]}}},"required":["todos"]}`)
}
func (TodoWriteTool) Run(ctx context.Context, env *Env, in json.RawMessage) Result {
	a, err := decode[struct {
		Todos []Todo `json:"todos"`
	}](in)
	if err != nil {
		return Errorf("%v", err)
	}
	env.SetTodos(a.Todos)
	done := 0
	for _, t := range a.Todos {
		if t.Status == "completed" {
			done++
		}
	}
	return Result{
		Output:  "Todo list updated. Continue with the current task.",
		Display: fmt.Sprintf("%d/%d done", done, len(a.Todos)),
	}
}

// ---- Task (sub-agents) ----

type TaskTool struct{}

func (TaskTool) Name() string { return "Task" }
func (TaskTool) Kind() Kind   { return KindInternal }
func (TaskTool) Description() string {
	return `Launches a sub-agent with its own fresh context to handle a self-contained task autonomously, such as a broad codebase search or a focused investigation. The sub-agent cannot see this conversation, so the prompt must contain everything it needs. It returns a single final report.
subagent_type "general-purpose" has all tools; other types are defined by the user (see the agents listed in the system prompt).`
}
func (TaskTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"description":{"type":"string","description":"Short (3-5 word) description of the task"},
"prompt":{"type":"string","description":"The full task for the agent"},
"subagent_type":{"type":"string","description":"Agent type (default general-purpose)"}},
"required":["description","prompt"]}`)
}
func (TaskTool) Subject(in json.RawMessage) string {
	a, _ := decode[struct{ Description string }](in)
	return a.Description
}
func (TaskTool) Run(ctx context.Context, env *Env, in json.RawMessage) Result {
	a, err := decode[struct {
		Description  string `json:"description"`
		Prompt       string `json:"prompt"`
		SubagentType string `json:"subagent_type"`
	}](in)
	if err != nil {
		return Errorf("%v", err)
	}
	if env.Spawn == nil {
		return Errorf("sub-agents are not available here")
	}
	out, err := env.Spawn(ctx, a.SubagentType, a.Description, a.Prompt)
	if err != nil {
		return Errorf("sub-agent failed: %v", err)
	}
	return Result{Output: out, Display: "Done"}
}

// ---- Skill ----

type SkillTool struct{}

func (SkillTool) Name() string { return "Skill" }
func (SkillTool) Kind() Kind   { return KindRead }
func (SkillTool) Description() string {
	return "Loads a skill: a packaged set of instructions for a particular kind of task. Available skills are listed in the system prompt. Call this before starting a task a skill covers, then follow its instructions."
}
func (SkillTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"skill":{"type":"string","description":"Skill name"}},"required":["skill"]}`)
}
func (SkillTool) Subject(in json.RawMessage) string {
	a, _ := decode[struct{ Skill string }](in)
	return a.Skill
}
func (SkillTool) Run(ctx context.Context, env *Env, in json.RawMessage) Result {
	a, err := decode[struct{ Skill string }](in)
	if err != nil {
		return Errorf("%v", err)
	}
	for _, s := range env.Skills {
		if s.Name == a.Skill {
			return Result{
				Output:  fmt.Sprintf("Skill %q loaded from %s (base directory: %s).\n\n%s", s.Name, s.Path, s.Dir, s.Body),
				Display: "Loaded " + s.Name,
			}
		}
	}
	return Errorf("unknown skill %q", a.Skill)
}

// ---- AskUserQuestion ----

type AskUserTool struct{}

func (AskUserTool) Name() string { return "AskUserQuestion" }
func (AskUserTool) Kind() Kind   { return KindInternal }
func (AskUserTool) Description() string {
	return "Asks the user a multiple-choice question when you are blocked on a decision only they can make. The user can always choose to type a different answer."
}
func (AskUserTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"question":{"type":"string"},
"options":{"type":"array","items":{"type":"string"},"description":"2-4 possible answers"}},
"required":["question","options"]}`)
}
func (AskUserTool) Run(ctx context.Context, env *Env, in json.RawMessage) Result {
	a, err := decode[struct {
		Question string   `json:"question"`
		Options  []string `json:"options"`
	}](in)
	if err != nil {
		return Errorf("%v", err)
	}
	if env.Ask == nil {
		return Errorf("no user is available to answer questions; make a sensible assumption and state it")
	}
	ans, err := env.Ask(ctx, Question{Question: a.Question, Options: a.Options})
	if err != nil {
		return Errorf("%v", err)
	}
	return Result{Output: "User answered: " + ans, Display: ans}
}

// ---- ExitPlanMode ----

type ExitPlanModeTool struct{}

func (ExitPlanModeTool) Name() string { return "ExitPlanMode" }
func (ExitPlanModeTool) Kind() Kind   { return KindInternal }
func (ExitPlanModeTool) Description() string {
	return "Use in plan mode when you have finished researching and have a concrete implementation plan. Presents the plan (markdown) to the user for approval; if approved, plan mode ends and you may start implementing."
}
func (ExitPlanModeTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"plan":{"type":"string","description":"The plan, in markdown"}},"required":["plan"]}`)
}
func (ExitPlanModeTool) Run(ctx context.Context, env *Env, in json.RawMessage) Result {
	a, err := decode[struct{ Plan string }](in)
	if err != nil {
		return Errorf("%v", err)
	}
	if env.ExitPlan == nil {
		return Errorf("plan approval is not available here")
	}
	ok, err := env.ExitPlan(ctx, a.Plan)
	if err != nil {
		return Errorf("%v", err)
	}
	if !ok {
		return Result{Output: "The user rejected the plan. Stay in plan mode and ask what they would like changed.", IsError: true, Display: "Plan rejected"}
	}
	return Result{Output: "The user approved the plan. Plan mode is off; start implementing it now.", Display: "Plan approved"}
}

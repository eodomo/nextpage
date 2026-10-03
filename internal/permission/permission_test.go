package permission

import (
	"encoding/json"
	"testing"

	"github.com/eodomo/nextpage/internal/tools"
)

func in(v map[string]any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func TestCheck(t *testing.T) {
	c := NewChecker("/repo", ModeDefault,
		[]string{"Bash(git diff:*)", "Edit(src/**)", "WebFetch(domain:go.dev)"},
		[]string{"Read(secrets/**)"}, nil)

	cases := []struct {
		name string
		tool tools.Tool
		in   json.RawMessage
		want Behavior
	}{
		{"read allowed", tools.ReadTool{}, in(map[string]any{"file_path": "/repo/main.go"}), Allow},
		{"read denied by rule", tools.ReadTool{}, in(map[string]any{"file_path": "/repo/secrets/key"}), Deny},
		{"bash prefix rule", tools.BashTool{}, in(map[string]any{"command": "git diff --stat"}), Allow},
		{"bash prefix needs word boundary", tools.BashTool{}, in(map[string]any{"command": "git diffx"}), Ask},
		{"safe command", tools.BashTool{}, in(map[string]any{"command": "ls -la"}), Allow},
		{"chained command not safe", tools.BashTool{}, in(map[string]any{"command": "ls; rm -rf /"}), Ask},
		{"edit glob", tools.EditTool{}, in(map[string]any{"file_path": "/repo/src/a/b.go"}), Allow},
		{"edit rule covers write", tools.WriteTool{}, in(map[string]any{"file_path": "/repo/src/new.go"}), Allow},
		{"edit outside glob", tools.EditTool{}, in(map[string]any{"file_path": "/repo/main.go"}), Ask},
		{"webfetch subdomain", tools.WebFetchTool{}, in(map[string]any{"url": "https://pkg.go.dev/x"}), Allow},
		{"webfetch other", tools.WebFetchTool{}, in(map[string]any{"url": "https://example.com"}), Ask},
	}
	for _, tc := range cases {
		if got := c.Check(tc.tool, tc.in).Behavior; got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestModes(t *testing.T) {
	c := NewChecker("/repo", ModePlan, nil, nil, nil)
	edit := in(map[string]any{"file_path": "/repo/x.go"})
	if got := c.Check(tools.EditTool{}, edit).Behavior; got != Deny {
		t.Errorf("plan mode edit: got %v, want Deny", got)
	}
	c.SetMode(ModeAcceptEdits)
	if got := c.Check(tools.EditTool{}, edit).Behavior; got != Allow {
		t.Errorf("acceptEdits edit: got %v, want Allow", got)
	}
	if got := c.Check(tools.BashTool{}, in(map[string]any{"command": "make"})).Behavior; got != Ask {
		t.Errorf("acceptEdits bash: got %v, want Ask", got)
	}
	c.SetMode(ModeDefault)
	if m := c.CycleMode(); m != ModeAcceptEdits {
		t.Errorf("cycle: got %v", m)
	}
	c.CycleMode()
	if m := c.CycleMode(); m != ModeDefault {
		t.Errorf("cycle should skip bypass unless allowed, got %v", m)
	}
}

func TestSuggestRule(t *testing.T) {
	if got := SuggestRule(tools.BashTool{}, in(map[string]any{"command": "go test ./..."})); got != "Bash(go test:*)" {
		t.Errorf("got %q", got)
	}
	if got := SuggestRule(tools.BashTool{}, in(map[string]any{"command": "touch a.txt"})); got != "Bash(touch:*)" {
		t.Errorf("got %q", got)
	}
}

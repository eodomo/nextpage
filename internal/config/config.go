// Package config loads layered settings, mirroring Claude Code's scheme:
//
//	~/.nextpage/settings.json            user
//	<project>/.nextpage/settings.json    project (checked in)
//	<project>/.nextpage/settings.local.json  local (gitignored)
//	<project>/.mcp.json                  project MCP servers
//
// Later layers override scalars and append to lists. Env vars and CLI flags
// are applied on top by the caller.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const DirName = ".nextpage"

type Permissions struct {
	Allow       []string `json:"allow,omitempty"`
	Deny        []string `json:"deny,omitempty"`
	Ask         []string `json:"ask,omitempty"`
	DefaultMode string   `json:"defaultMode,omitempty"`
}

type HookCommand struct {
	Type    string `json:"type"` // only "command" is supported
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"` // seconds
}

type HookMatcher struct {
	Matcher string        `json:"matcher,omitempty"` // regex on tool name; empty matches all
	Hooks   []HookCommand `json:"hooks"`
}

type MCPServer struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

type Settings struct {
	Provider    string                   `json:"provider,omitempty"`
	Server      string                   `json:"server,omitempty"`
	Model       string                   `json:"model,omitempty"`
	NumCtx      int                      `json:"numCtx,omitempty"`
	Temperature *float64                 `json:"temperature,omitempty"`
	Think       *bool                    `json:"think,omitempty"`
	AutoCompact *bool                    `json:"autoCompact,omitempty"`
	MaxTurns    int                      `json:"maxTurns,omitempty"`
	MaxTokens   int                      `json:"maxTokens,omitempty"` // output cap per model reply
	Permissions Permissions              `json:"permissions"`
	Hooks       map[string][]HookMatcher `json:"hooks,omitempty"`
	Env         map[string]string        `json:"env,omitempty"`
	MCPServers  map[string]MCPServer     `json:"mcpServers,omitempty"`

	// Learning platform (profile "learn").
	Profile      string `json:"profile,omitempty"`      // "learn" (default) or "code"
	CoursesDir   string `json:"coursesDir,omitempty"`   // where course notes are written
	LessonFormat string `json:"lessonFormat,omitempty"` // "markdown" (default) or "latex"
	PassPercent  int    `json:"passPercent,omitempty"`  // checkpoint pass mark
}

// Paths records where each settings layer lives for this project.
type Paths struct {
	ProjectRoot string
	UserDir     string
	User        string
	Project     string
	Local       string
	MCP         string
}

func ResolvePaths(cwd string) Paths {
	home, _ := os.UserHomeDir()
	root := ProjectRoot(cwd)
	userDir := filepath.Join(home, DirName)
	return Paths{
		ProjectRoot: root,
		UserDir:     userDir,
		User:        filepath.Join(userDir, "settings.json"),
		Project:     filepath.Join(root, DirName, "settings.json"),
		Local:       filepath.Join(root, DirName, "settings.local.json"),
		MCP:         filepath.Join(root, ".mcp.json"),
	}
}

// ProjectRoot is the git toplevel containing cwd, or cwd itself.
func ProjectRoot(cwd string) string {
	out, err := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel").Output()
	if err == nil {
		if p := strings.TrimSpace(string(out)); p != "" {
			return p
		}
	}
	return cwd
}

func Load(p Paths) (Settings, error) {
	var s Settings
	for _, path := range []string{p.User, p.Project, p.Local} {
		layer, err := readSettings(path)
		if err != nil {
			return s, err
		}
		s.merge(layer)
	}
	mcp, err := readMCPFile(p.MCP)
	if err != nil {
		return s, err
	}
	for name, srv := range mcp {
		if s.MCPServers == nil {
			s.MCPServers = map[string]MCPServer{}
		}
		s.MCPServers[name] = srv
	}
	return s, nil
}

func readSettings(path string) (Settings, error) {
	var s Settings
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

func readMCPFile(path string) (map[string]MCPServer, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f struct {
		MCPServers map[string]MCPServer `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f.MCPServers, nil
}

func (s *Settings) merge(o Settings) {
	if o.Provider != "" {
		s.Provider = o.Provider
	}
	if o.Server != "" {
		s.Server = o.Server
	}
	if o.Model != "" {
		s.Model = o.Model
	}
	if o.NumCtx != 0 {
		s.NumCtx = o.NumCtx
	}
	if o.Temperature != nil {
		s.Temperature = o.Temperature
	}
	if o.Think != nil {
		s.Think = o.Think
	}
	if o.AutoCompact != nil {
		s.AutoCompact = o.AutoCompact
	}
	if o.MaxTurns != 0 {
		s.MaxTurns = o.MaxTurns
	}
	if o.MaxTokens != 0 {
		s.MaxTokens = o.MaxTokens
	}
	if o.Profile != "" {
		s.Profile = o.Profile
	}
	if o.CoursesDir != "" {
		s.CoursesDir = o.CoursesDir
	}
	if o.LessonFormat != "" {
		s.LessonFormat = o.LessonFormat
	}
	if o.PassPercent != 0 {
		s.PassPercent = o.PassPercent
	}
	s.Permissions.Allow = append(s.Permissions.Allow, o.Permissions.Allow...)
	s.Permissions.Deny = append(s.Permissions.Deny, o.Permissions.Deny...)
	s.Permissions.Ask = append(s.Permissions.Ask, o.Permissions.Ask...)
	if o.Permissions.DefaultMode != "" {
		s.Permissions.DefaultMode = o.Permissions.DefaultMode
	}
	for event, matchers := range o.Hooks {
		if s.Hooks == nil {
			s.Hooks = map[string][]HookMatcher{}
		}
		s.Hooks[event] = append(s.Hooks[event], matchers...)
	}
	for k, v := range o.Env {
		if s.Env == nil {
			s.Env = map[string]string{}
		}
		s.Env[k] = v
	}
	for k, v := range o.MCPServers {
		if s.MCPServers == nil {
			s.MCPServers = map[string]MCPServer{}
		}
		s.MCPServers[k] = v
	}
}

// SetValue sets a top-level key in a settings file, creating it if needed
// and preserving everything else in it.
func SetValue(path, key string, value any) error {
	return modify(path, func(raw map[string]any) { raw[key] = value })
}

// AddAllowRule appends a permission rule to the given settings file
// (normally settings.local.json), creating it if needed. Unknown fields in
// the file are preserved.
func AddAllowRule(path, rule string) error {
	return modify(path, func(raw map[string]any) {
		perms, _ := raw["permissions"].(map[string]any)
		if perms == nil {
			perms = map[string]any{}
		}
		allow, _ := perms["allow"].([]any)
		for _, r := range allow {
			if r == rule {
				return
			}
		}
		perms["allow"] = append(allow, rule)
		raw["permissions"] = perms
	})
}

func modify(path string, fn func(raw map[string]any)) error {
	raw := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fn(raw)
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// Connection is the model server login saved from the web app's settings.
// It overrides OLLAMASERVER/MODEL/OLLAMAUSER/OLLAMAPASS when present.
type Connection struct {
	Server   string `json:"server"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Model    string `json:"model,omitempty"`
}

func ConnectionPath(userDir string) string {
	return filepath.Join(userDir, "connection.json")
}

func LoadConnection(path string) (Connection, error) {
	var c Connection
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(data, &c)
}

// SaveConnection writes the file readable only by its owner, since it holds
// a password.
func SaveConnection(path string, c Connection) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

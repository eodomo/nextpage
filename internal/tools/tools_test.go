package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pat, path string
		want      bool
	}{
		{"**/*.go", "main.go", true},
		{"**/*.go", "internal/tools/bash.go", true},
		{"src/**", "src/a/b.ts", true},
		{"src/*.ts", "src/a/b.ts", false},
		{"**/*.{ts,tsx}", "web/app.tsx", true},
		{"*.md", "docs/x.md", false},
	}
	for _, c := range cases {
		if got := MatchGlob(c.pat, c.path); got != c.want {
			t.Errorf("MatchGlob(%q, %q) = %v, want %v", c.pat, c.path, got, c.want)
		}
	}
}

func TestDiff(t *testing.T) {
	got := Diff("a\nb\nc\n", "a\nB\nc\n")
	want := "  a\n- b\n+ B\n  c"
	if got != want {
		t.Errorf("Diff:\n%s\nwant:\n%s", got, want)
	}
}

func run(t *testing.T, tool Tool, env *Env, args map[string]any) Result {
	t.Helper()
	b, _ := json.Marshal(args)
	return tool.Run(context.Background(), env, b)
}

func TestEditRequiresRead(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	os.WriteFile(p, []byte("one two two\n"), 0o644)
	env := NewEnv(dir, dir)

	if r := run(t, EditTool{}, env, map[string]any{"file_path": "f.txt", "old_string": "one", "new_string": "1"}); !r.IsError {
		t.Fatal("edit without read should fail")
	}
	run(t, ReadTool{}, env, map[string]any{"file_path": "f.txt"})
	if r := run(t, EditTool{}, env, map[string]any{"file_path": "f.txt", "old_string": "two", "new_string": "2"}); !r.IsError {
		t.Fatal("ambiguous edit should fail")
	}
	if r := run(t, EditTool{}, env, map[string]any{"file_path": "f.txt", "old_string": "two", "new_string": "2", "replace_all": true}); r.IsError {
		t.Fatalf("replace_all failed: %s", r.Output)
	}
	data, _ := os.ReadFile(p)
	if string(data) != "one 2 2\n" {
		t.Errorf("content = %q", data)
	}
}

func TestBashPersistsCwd(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	env := NewEnv(dir, dir)
	run(t, BashTool{}, env, map[string]any{"command": "cd sub"})
	r := run(t, BashTool{}, env, map[string]any{"command": "pwd"})
	if !strings.HasSuffix(strings.TrimSpace(r.Output), "/sub") {
		t.Errorf("pwd = %q", r.Output)
	}
	if r := run(t, BashTool{}, env, map[string]any{"command": "exit 3"}); !r.IsError {
		t.Error("non-zero exit should be an error")
	}
}

func TestGrep(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\nfunc Foo() {}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("Foo\n"), 0o644)
	env := NewEnv(dir, dir)
	r := run(t, GrepTool{}, env, map[string]any{"pattern": "Foo", "glob": "*.go", "output_mode": "content"})
	if !strings.Contains(r.Output, "a.go:2:func Foo") || strings.Contains(r.Output, "b.txt") {
		t.Errorf("grep output = %q", r.Output)
	}
}

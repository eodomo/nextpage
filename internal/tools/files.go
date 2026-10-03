package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ---- Read ----

type ReadTool struct{}

func (ReadTool) Name() string { return "Read" }
func (ReadTool) Kind() Kind   { return KindRead }
func (ReadTool) Description() string {
	return `Reads a file from the local filesystem. The file_path may be absolute or relative to the working directory.
- By default reads up to 2000 lines from the start of the file; use offset and limit for long files.
- Output is in cat -n format: line numbers start at 1, followed by a tab, then the line.
- Lines longer than 2000 characters are truncated.
- You must Read a file before editing or overwriting it.`
}
func (ReadTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"file_path":{"type":"string","description":"Path of the file to read"},
"offset":{"type":"integer","description":"1-based line number to start reading from"},
"limit":{"type":"integer","description":"Number of lines to read"}},
"required":["file_path"]}`)
}
func (ReadTool) Subject(in json.RawMessage) string {
	a, _ := decode[struct {
		FilePath string `json:"file_path"`
	}](in)
	return a.FilePath
}

func (ReadTool) Run(ctx context.Context, env *Env, in json.RawMessage) Result {
	a, err := decode[struct {
		FilePath string `json:"file_path"`
		Offset   int    `json:"offset"`
		Limit    int    `json:"limit"`
	}](in)
	if err != nil {
		return Errorf("%v", err)
	}
	path := env.Abs(a.FilePath)
	fi, err := os.Stat(path)
	if err != nil {
		return Errorf("%v", err)
	}
	if fi.IsDir() {
		return Errorf("%s is a directory; use Bash with ls or Glob to list it", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Errorf("%v", err)
	}
	if isBinary(data) {
		return Errorf("%s appears to be a binary file (%d bytes)", path, len(data))
	}
	env.MarkRead(path)
	if len(data) == 0 {
		return Result{Output: "<system-reminder>The file exists but is empty.</system-reminder>"}
	}

	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	start := max(a.Offset, 1)
	limit := a.Limit
	if limit <= 0 {
		limit = 2000
	}
	if start > len(lines) {
		return Errorf("offset %d is past the end of the file (%d lines)", start, len(lines))
	}
	end := min(start-1+limit, len(lines))
	var b strings.Builder
	for i := start - 1; i < end; i++ {
		line := lines[i]
		if len(line) > 2000 {
			line = line[:2000] + "..."
		}
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, line)
	}
	if end < len(lines) {
		fmt.Fprintf(&b, "\n... (%d more lines; use offset/limit to read further)\n", len(lines)-end)
	}
	return Result{
		Output:  b.String(),
		Display: fmt.Sprintf("Read %d lines", end-start+1),
	}
}

func isBinary(data []byte) bool {
	n := min(len(data), 8000)
	return bytes.IndexByte(data[:n], 0) >= 0
}

// ---- Write ----

type WriteTool struct{}

func (WriteTool) Name() string { return "Write" }
func (WriteTool) Kind() Kind   { return KindEdit }
func (WriteTool) Description() string {
	return `Writes a file to the local filesystem, overwriting it if it exists.
- If the file already exists you must Read it first, or this will fail.
- Prefer Edit for modifying existing files; use Write for new files or complete rewrites.
- Parent directories are created as needed.`
}
func (WriteTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"file_path":{"type":"string","description":"Path of the file to write"},
"content":{"type":"string","description":"Full content to write"}},
"required":["file_path","content"]}`)
}
func (WriteTool) Subject(in json.RawMessage) string { return ReadTool{}.Subject(in) }

func (WriteTool) Run(ctx context.Context, env *Env, in json.RawMessage) Result {
	a, err := decode[struct {
		FilePath string `json:"file_path"`
		Content  string `json:"content"`
	}](in)
	if err != nil {
		return Errorf("%v", err)
	}
	path := env.Abs(a.FilePath)
	if err := env.CheckFresh(path); err != nil {
		return Errorf("%v", err)
	}
	old, _ := os.ReadFile(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Errorf("%v", err)
	}
	if err := os.WriteFile(path, []byte(a.Content), 0o644); err != nil {
		return Errorf("%v", err)
	}
	env.MarkRead(path)
	verb := "Created"
	if old != nil {
		verb = "Updated"
	}
	return Result{
		Output:  fmt.Sprintf("%s %s (%d lines)", verb, path, strings.Count(a.Content, "\n")+1),
		Display: Diff(string(old), a.Content),
	}
}

// ---- Edit ----

type EditTool struct{}

func (EditTool) Name() string { return "Edit" }
func (EditTool) Kind() Kind   { return KindEdit }
func (EditTool) Description() string {
	return `Performs an exact string replacement in a file.
- You must Read the file before editing it.
- old_string must match the file exactly, including whitespace and indentation. Do not include the line-number prefix from Read output.
- The edit fails if old_string is not unique in the file; include more surrounding context or set replace_all.
- Use replace_all to rename something across the whole file.`
}
func (EditTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"file_path":{"type":"string","description":"Path of the file to modify"},
"old_string":{"type":"string","description":"Exact text to replace"},
"new_string":{"type":"string","description":"Replacement text (must differ from old_string)"},
"replace_all":{"type":"boolean","description":"Replace every occurrence (default false)"}},
"required":["file_path","old_string","new_string"]}`)
}
func (EditTool) Subject(in json.RawMessage) string { return ReadTool{}.Subject(in) }

func (EditTool) Run(ctx context.Context, env *Env, in json.RawMessage) Result {
	a, err := decode[struct {
		FilePath   string `json:"file_path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}](in)
	if err != nil {
		return Errorf("%v", err)
	}
	path := env.Abs(a.FilePath)
	if a.OldString == a.NewString {
		return Errorf("old_string and new_string are identical")
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) && a.OldString == "" {
		// Creating a new file via Edit with empty old_string.
		return WriteTool{}.Run(ctx, env, mustJSON(map[string]string{"file_path": a.FilePath, "content": a.NewString}))
	}
	if err != nil {
		return Errorf("%v", err)
	}
	if err := env.CheckFresh(path); err != nil {
		return Errorf("%v", err)
	}
	content := string(data)
	n := strings.Count(content, a.OldString)
	switch {
	case a.OldString == "":
		return Errorf("old_string is empty but the file already exists")
	case n == 0:
		return Errorf("old_string not found in %s", path)
	case n > 1 && !a.ReplaceAll:
		return Errorf("old_string appears %d times in %s; add more context to make it unique or set replace_all", n, path)
	}
	var updated string
	if a.ReplaceAll {
		updated = strings.ReplaceAll(content, a.OldString, a.NewString)
	} else {
		updated = strings.Replace(content, a.OldString, a.NewString, 1)
	}
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return Errorf("%v", err)
	}
	env.MarkRead(path)
	return Result{
		Output:  fmt.Sprintf("Edited %s (%d replacement(s))", path, max(1, boolInt(a.ReplaceAll)*n)),
		Display: Diff(a.OldString, a.NewString),
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

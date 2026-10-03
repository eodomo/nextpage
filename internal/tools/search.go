package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// skipDirs are never descended into by Glob and Grep.
var skipDirs = map[string]bool{".git": true, "node_modules": true, ".hg": true, ".svn": true}

// MatchGlob reports whether a slash-separated path matches pattern, which may
// use *, ?, [...], {a,b} and ** (any number of directories).
func MatchGlob(pattern, path string) bool {
	for _, p := range expandBraces(pattern) {
		if matchSegments(strings.Split(p, "/"), strings.Split(path, "/")) {
			return true
		}
	}
	return false
}

func matchSegments(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(segs); i++ {
				if matchSegments(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		if ok, _ := filepath.Match(pat[0], segs[0]); !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}

func expandBraces(p string) []string {
	start := strings.IndexByte(p, '{')
	if start < 0 {
		return []string{p}
	}
	depth := 0
	for end := start; end < len(p); end++ {
		switch p[end] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				var out []string
				for _, alt := range splitTopLevel(p[start+1 : end]) {
					out = append(out, expandBraces(p[:start]+alt+p[end+1:])...)
				}
				return out
			}
		}
	}
	return []string{p}
}

func splitTopLevel(s string) []string {
	var parts []string
	depth, last := 0, 0
	for i, c := range s {
		switch c {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, s[last:i])
				last = i + 1
			}
		}
	}
	return append(parts, s[last:])
}

// walkFiles calls fn for every regular file under root, skipping VCS and
// dependency directories. rel is slash-separated and relative to root.
func walkFiles(ctx context.Context, root string, fn func(path, rel string) error) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if path != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		return fn(path, filepath.ToSlash(rel))
	})
}

// ---- Glob ----

type GlobTool struct{}

func (GlobTool) Name() string { return "Glob" }
func (GlobTool) Kind() Kind   { return KindRead }
func (GlobTool) Description() string {
	return `Fast file pattern matching. Supports patterns like "**/*.go" or "src/**/*.{ts,tsx}".
Returns matching file paths sorted by modification time (newest first). Patterns without a slash match file names at any depth.`
}
func (GlobTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"pattern":{"type":"string","description":"Glob pattern to match"},
"path":{"type":"string","description":"Directory to search in (default: working directory)"}},
"required":["pattern"]}`)
}
func (GlobTool) Subject(in json.RawMessage) string {
	a, _ := decode[struct{ Pattern string }](in)
	return a.Pattern
}

func (GlobTool) Run(ctx context.Context, env *Env, in json.RawMessage) Result {
	a, err := decode[struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}](in)
	if err != nil {
		return Errorf("%v", err)
	}
	root := env.Cwd()
	if a.Path != "" {
		root = env.Abs(a.Path)
	}
	pattern := a.Pattern
	if !strings.Contains(pattern, "/") {
		pattern = "**/" + pattern
	}
	var matches []string
	err = walkFiles(ctx, root, func(path, rel string) error {
		if MatchGlob(pattern, rel) {
			matches = append(matches, path)
		}
		return nil
	})
	if err != nil {
		return Errorf("%v", err)
	}
	if len(matches) == 0 {
		return Result{Output: "No files found"}
	}
	sortByMtime(matches)
	const limit = 200
	extra := ""
	if len(matches) > limit {
		extra = fmt.Sprintf("\n... (%d more; narrow the pattern)", len(matches)-limit)
		matches = matches[:limit]
	}
	return Result{
		Output:  strings.Join(matches, "\n") + extra,
		Display: fmt.Sprintf("Found %d files", len(matches)),
	}
}

// ---- Grep ----

type GrepTool struct{}

func (GrepTool) Name() string { return "Grep" }
func (GrepTool) Kind() Kind   { return KindRead }
func (GrepTool) Description() string {
	return `Searches file contents with a regular expression (Go RE2 syntax).
- output_mode: "files_with_matches" (default, lists paths), "content" (matching lines with line numbers), or "count".
- Filter files with glob (e.g. "*.go", "**/*.{ts,tsx}").
- Use -i for case-insensitive search and -C for context lines in content mode.
Always use Grep for content search rather than running grep or rg through Bash.`
}
func (GrepTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"pattern":{"type":"string","description":"Regular expression to search for"},
"path":{"type":"string","description":"File or directory to search (default: working directory)"},
"glob":{"type":"string","description":"Only search files matching this glob"},
"output_mode":{"type":"string","enum":["content","files_with_matches","count"]},
"-i":{"type":"boolean","description":"Case insensitive"},
"-C":{"type":"integer","description":"Lines of context around matches (content mode)"},
"head_limit":{"type":"integer","description":"Limit output to the first N lines/entries"}},
"required":["pattern"]}`)
}
func (GrepTool) Subject(in json.RawMessage) string {
	a, _ := decode[struct{ Pattern string }](in)
	return a.Pattern
}

func (GrepTool) Run(ctx context.Context, env *Env, in json.RawMessage) Result {
	a, err := decode[struct {
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		Glob       string `json:"glob"`
		OutputMode string `json:"output_mode"`
		IgnoreCase bool   `json:"-i"`
		Context    int    `json:"-C"`
		HeadLimit  int    `json:"head_limit"`
	}](in)
	if err != nil {
		return Errorf("%v", err)
	}
	pat := a.Pattern
	if a.IgnoreCase {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return Errorf("invalid regex: %v", err)
	}
	root := env.Cwd()
	if a.Path != "" {
		root = env.Abs(a.Path)
	}
	glob := a.Glob
	if glob != "" && !strings.Contains(glob, "/") {
		glob = "**/" + glob
	}
	mode := a.OutputMode
	if mode == "" {
		mode = "files_with_matches"
	}
	limit := a.HeadLimit
	if limit <= 0 {
		limit = 500
	}

	var out []string
	var files []string
	searchFile := func(path, rel string) error {
		if glob != "" && !MatchGlob(glob, rel) {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		head := make([]byte, 8000)
		n, _ := f.Read(head)
		if isBinary(head[:n]) {
			return nil
		}
		_, _ = f.Seek(0, 0)

		var lines []string
		var hits []int
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if re.MatchString(line) {
				hits = append(hits, len(lines))
			}
			lines = append(lines, line)
		}
		if len(hits) == 0 {
			return nil
		}
		files = append(files, path)
		switch mode {
		case "count":
			out = append(out, fmt.Sprintf("%s:%d", path, len(hits)))
		case "content":
			last := -1
			for _, h := range hits {
				from := max(h-a.Context, last+1)
				to := min(h+a.Context, len(lines)-1)
				if last >= 0 && from > last+1 {
					out = append(out, "--")
				}
				for i := from; i <= to; i++ {
					sep := "-"
					if i == h {
						sep = ":"
					}
					l := lines[i]
					if len(l) > 500 {
						l = l[:500] + "..."
					}
					out = append(out, fmt.Sprintf("%s:%d%s%s", path, i+1, sep, l))
				}
				last = to
			}
		}
		if len(out) > limit || len(files) > limit {
			return fs.SkipAll
		}
		return nil
	}

	if fi, err := os.Stat(root); err == nil && !fi.IsDir() {
		_ = searchFile(root, filepath.Base(root))
	} else {
		if err := walkFiles(ctx, root, searchFile); err != nil && err != fs.SkipAll {
			return Errorf("%v", err)
		}
	}

	if mode == "files_with_matches" {
		sortByMtime(files)
		out = files
	}
	if len(out) == 0 {
		return Result{Output: "No matches found"}
	}
	extra := ""
	if len(out) > limit {
		out = out[:limit]
		extra = "\n... (output truncated; refine the search)"
	}
	return Result{
		Output:  truncate(strings.Join(out, "\n")+extra, maxOutput),
		Display: fmt.Sprintf("Found matches in %d files", len(files)),
	}
}

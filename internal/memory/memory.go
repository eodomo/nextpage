// Package memory loads persistent instruction files ("memory") that are
// injected into the system prompt, like Claude Code's CLAUDE.md:
//
//	~/.nextpage/NEXTPAGE.md                     user memory, all projects
//	NEXTPAGE.md / AGENTS.md / CLAUDE.md         from the working directory up to /
//	NEXTPAGE.local.md                           personal, gitignored
//
// Files may import others with "@path/to/file" on its own line (relative to
// the importing file, max depth 5).
package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var projectNames = []string{"NEXTPAGE.md", "AGENTS.md", "CLAUDE.md", "NEXTPAGE.local.md"}

type File struct {
	Path    string
	Content string
}

func Load(userDir, cwd string) []File {
	var files []File
	seen := map[string]bool{}
	add := func(p string) {
		if seen[p] {
			return
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return
		}
		seen[p] = true
		files = append(files, File{Path: p, Content: expandImports(string(data), filepath.Dir(p), 0, seen)})
	}

	add(filepath.Join(userDir, "NEXTPAGE.md"))

	// Walk from / down to cwd so more specific files come last.
	var dirs []string
	for d := cwd; ; d = filepath.Dir(d) {
		dirs = append(dirs, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		for _, n := range projectNames {
			add(filepath.Join(dirs[i], n))
		}
	}
	return files
}

func expandImports(content, dir string, depth int, seen map[string]bool) string {
	if depth >= 5 {
		return content
	}
	lines := strings.Split(content, "\n")
	inFence := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
		}
		if inFence || !strings.HasPrefix(trimmed, "@") || strings.ContainsAny(trimmed, " \t") {
			continue
		}
		p := trimmed[1:]
		if strings.HasPrefix(p, "~/") {
			home, _ := os.UserHomeDir()
			p = filepath.Join(home, p[2:])
		} else if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		if seen[p] {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		seen[p] = true
		lines[i] = expandImports(string(data), filepath.Dir(p), depth+1, seen)
	}
	return strings.Join(lines, "\n")
}

func Format(files []File) string {
	var b strings.Builder
	for _, f := range files {
		fmt.Fprintf(&b, "Contents of %s:\n\n%s\n\n", f.Path, strings.TrimSpace(f.Content))
	}
	return b.String()
}

// Append adds a bullet to a memory file, creating it if needed. Used by the
// "#" input shortcut.
func Append(path, note string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if fi, _ := f.Stat(); fi != nil && fi.Size() > 0 {
		data, _ := os.ReadFile(path)
		if !strings.HasSuffix(string(data), "\n") {
			fmt.Fprintln(f)
		}
	}
	_, err = fmt.Fprintf(f, "- %s\n", strings.TrimSpace(note))
	return err
}

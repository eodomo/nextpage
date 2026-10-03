// Package ext loads user-defined extensions written as markdown files with
// YAML-ish frontmatter: skills, sub-agent definitions and slash commands.
// Each kind is searched for in ~/.nextpage and <project>/.nextpage; project
// definitions override user ones with the same name.
//
//	<dir>/skills/<name>/SKILL.md
//	<dir>/agents/<name>.md
//	<dir>/commands/<name>.md   (subdirectories become name:sub)
package ext

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Skill struct {
	Name        string
	Description string
	Path        string
	Dir         string
	Body        string
}

type AgentDef struct {
	Name        string
	Description string
	Tools       []string // empty means all tools
	Model       string
	Prompt      string
	Path        string
}

type Command struct {
	Name         string
	Description  string
	ArgumentHint string
	Model        string
	Body         string
	Path         string
}

// ParseFrontmatter splits "---\nkey: value\n---\nbody". Values may be
// quoted; list values may be written as "a, b" or "[a, b]".
func ParseFrontmatter(src string) (map[string]string, string) {
	meta := map[string]string{}
	src = strings.TrimPrefix(src, "\ufeff")
	if !strings.HasPrefix(src, "---\n") && !strings.HasPrefix(src, "---\r\n") {
		return meta, src
	}
	rest := src[strings.Index(src, "\n")+1:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return meta, src
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"'`)
		meta[strings.TrimSpace(k)] = v
	}
	body := rest[end+4:]
	if i := strings.Index(body, "\n"); i >= 0 {
		body = body[i+1:]
	} else {
		body = ""
	}
	return meta, strings.TrimLeft(body, "\n")
}

func splitList(v string) []string {
	v = strings.Trim(v, "[]")
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.Trim(strings.TrimSpace(p), `"'`); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Dirs returns the extension base directories, lowest precedence first.
func Dirs(userDir, projectRoot string) []string {
	return []string{userDir, filepath.Join(projectRoot, ".nextpage")}
}

func LoadSkills(dirs []string) []Skill {
	byName := map[string]Skill{}
	for _, d := range dirs {
		paths, _ := filepath.Glob(filepath.Join(d, "skills", "*", "SKILL.md"))
		for _, p := range paths {
			data, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			meta, body := ParseFrontmatter(string(data))
			name := meta["name"]
			if name == "" {
				name = filepath.Base(filepath.Dir(p))
			}
			byName[name] = Skill{Name: name, Description: meta["description"], Path: p, Dir: filepath.Dir(p), Body: body}
		}
	}
	out := make([]Skill, 0, len(byName))
	for _, s := range byName {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func LoadAgents(dirs []string) []AgentDef {
	byName := map[string]AgentDef{}
	for _, d := range dirs {
		paths, _ := filepath.Glob(filepath.Join(d, "agents", "*.md"))
		for _, p := range paths {
			data, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			meta, body := ParseFrontmatter(string(data))
			name := meta["name"]
			if name == "" {
				name = strings.TrimSuffix(filepath.Base(p), ".md")
			}
			byName[name] = AgentDef{
				Name:        name,
				Description: meta["description"],
				Tools:       splitList(meta["tools"]),
				Model:       meta["model"],
				Prompt:      body,
				Path:        p,
			}
		}
	}
	out := make([]AgentDef, 0, len(byName))
	for _, a := range byName {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func LoadCommands(dirs []string) []Command {
	byName := map[string]Command{}
	for _, d := range dirs {
		root := filepath.Join(d, "commands")
		_ = filepath.WalkDir(root, func(p string, de os.DirEntry, err error) error {
			if err != nil || de.IsDir() || !strings.HasSuffix(p, ".md") {
				return nil
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			name := strings.ReplaceAll(strings.TrimSuffix(filepath.ToSlash(rel), ".md"), "/", ":")
			meta, body := ParseFrontmatter(string(data))
			desc := meta["description"]
			if desc == "" {
				desc = firstLine(body)
			}
			byName[name] = Command{
				Name:         name,
				Description:  desc,
				ArgumentHint: meta["argument-hint"],
				Model:        meta["model"],
				Body:         body,
				Path:         p,
			}
			return nil
		})
	}
	out := make([]Command, 0, len(byName))
	for _, c := range byName {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return s
}

// ExpandArgs substitutes $ARGUMENTS and positional $1..$9 in a command body.
func ExpandArgs(body, args string) string {
	fields := strings.Fields(args)
	for i := 9; i >= 1; i-- {
		v := ""
		if i <= len(fields) {
			v = fields[i-1]
		}
		body = strings.ReplaceAll(body, "$"+string(rune('0'+i)), v)
	}
	if strings.Contains(body, "$ARGUMENTS") {
		return strings.ReplaceAll(body, "$ARGUMENTS", args)
	}
	if args != "" {
		body += "\n\nARGUMENTS: " + args
	}
	return body
}

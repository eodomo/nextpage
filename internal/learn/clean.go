package learn

import (
	"fmt"
	"regexp"
	"strings"
)

// Small models sometimes write lessons as if LaTeX were Markdown: section
// titles as "\text{Title} \\" lines and whole stretches of prose wrapped in
// $$ ... $$, which would render as one giant equation. cleanLesson repairs
// those patterns and leaves real math alone.

var (
	reTextHeading = regexp.MustCompile(`^\s*\\text\{([^{}]+)\}\s*(?:\\\\)?\s*$`)
	reTextWrap    = regexp.MustCompile(`\\text\{([^{}]*)\}`)
	reWord        = regexp.MustCompile(`\b[A-Za-z]{2,}\b`)
)

func cleanLesson(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")

	// Pair up lines that open or close a display-math block.
	var delims []int
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "$$") && strings.Count(t, "$$") == 1 || strings.HasSuffix(t, "$$") && strings.Count(t, "$$") == 1 {
			delims = append(delims, i)
		}
	}
	unwrap := map[int]bool{}
	for k := 0; k < len(delims); k += 2 {
		start := delims[k]
		end := len(lines)
		if k+1 < len(delims) {
			end = delims[k+1]
		}
		if looksLikeProse(lines[start+1 : min(end, len(lines))]) {
			unwrap[start] = true
			if end < len(lines) {
				unwrap[end] = true
			}
		}
	}

	var out []string
	inProse := false
	for i, l := range lines {
		if unwrap[i] {
			l = strings.TrimSpace(strings.ReplaceAll(l, "$$", ""))
			inProse = !inProse
			if l == "" {
				continue
			}
		}
		if inProse || unwrap[i] {
			if m := reTextHeading.FindStringSubmatch(l); m != nil {
				l = "## " + strings.TrimSpace(m[1])
			} else {
				l = reTextWrap.ReplaceAllString(l, "$1")
				l = strings.TrimSuffix(strings.TrimRight(l, " "), `\\`)
			}
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// looksLikeProse reports whether a "math" block is really sentences: any
// line with eight or more ordinary words outside \text{} is prose.
func looksLikeProse(lines []string) bool {
	for _, l := range lines {
		if reTextHeading.MatchString(l) {
			continue
		}
		if len(reWord.FindAllString(reTextWrap.ReplaceAllString(l, ""), -1)) >= 8 {
			return true
		}
	}
	return false
}

// drawMermaid renders the learning path. The model's diagram contributes only
// its arrows between known section ids (small models often get mermaid
// syntax wrong); nodes and labels always come from the sections. Without
// usable arrows the path is linear.
func drawMermaid(model string, secs []*Section) string {
	ids := map[string]bool{}
	for _, s := range secs {
		ids[s.ID] = true
	}
	type edge struct{ from, to string }
	var edges []edge
	seen := map[edge]bool{}
	reached := map[string]bool{}
	// Drop bracketed labels so arrows between nodes are adjacent.
	stripped := regexp.MustCompile(`\[[^\]]*\]|\([^)]*\)|\{[^}]*\}`).ReplaceAllString(model, "")
	for _, m := range regexp.MustCompile(`([A-Za-z][A-Za-z0-9_]*)\s*-[-.]*>\s*(?:\|[^|]*\|\s*)?([A-Za-z][A-Za-z0-9_]*)`).FindAllStringSubmatch(stripped, -1) {
		e := edge{m[1], m[2]}
		if ids[e.from] && ids[e.to] && e.from != e.to && !seen[e] {
			seen[e] = true
			edges = append(edges, e)
			reached[e.from], reached[e.to] = true, true
		}
	}
	if len(secs) > 1 && len(reached) < len(secs) {
		edges = nil
		for i := 1; i < len(secs); i++ {
			edges = append(edges, edge{secs[i-1].ID, secs[i].ID})
		}
	}
	var b strings.Builder
	b.WriteString("flowchart TD\n")
	for i, s := range secs {
		label := strings.NewReplacer(`"`, "'", "[", "(", "]", ")").Replace(s.Title)
		// "1. Title" would be parsed by mermaid as a Markdown list.
		fmt.Fprintf(&b, "    %s[\"%d · %s\"]\n", s.ID, i+1, label)
	}
	for _, e := range edges {
		fmt.Fprintf(&b, "    %s --> %s\n", e.from, e.to)
	}
	return strings.TrimRight(b.String(), "\n")
}

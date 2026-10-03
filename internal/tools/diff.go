package tools

import (
	"fmt"
	"strings"
)

// Diff returns a compact line diff of a → b. Lines are prefixed with "+",
// "-" or " " and unchanged runs are collapsed to a few lines of context.
// The UI colors lines by their prefix.
func Diff(a, b string) string {
	al := splitLines(a)
	bl := splitLines(b)
	if len(al)*len(bl) > 4_000_000 {
		return fmt.Sprintf("(file too large to diff: %d → %d lines)", len(al), len(bl))
	}

	// LCS table, filled from the bottom right.
	lcs := make([][]int, len(al)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(bl)+1)
	}
	for i := len(al) - 1; i >= 0; i-- {
		for j := len(bl) - 1; j >= 0; j-- {
			if al[i] == bl[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}

	type line struct {
		op   byte
		text string
	}
	var ops []line
	i, j := 0, 0
	for i < len(al) || j < len(bl) {
		switch {
		case i < len(al) && j < len(bl) && al[i] == bl[j]:
			ops = append(ops, line{' ', al[i]})
			i++
			j++
		case i < len(al) && (j == len(bl) || lcs[i+1][j] >= lcs[i][j+1]):
			ops = append(ops, line{'-', al[i]})
			i++
		default:
			ops = append(ops, line{'+', bl[j]})
			j++
		}
	}

	const context = 3
	var out strings.Builder
	lastPrinted := -1
	near := func(k int) bool {
		for d := -context; d <= context; d++ {
			if k+d >= 0 && k+d < len(ops) && ops[k+d].op != ' ' {
				return true
			}
		}
		return false
	}
	for k, op := range ops {
		if !near(k) {
			continue
		}
		if lastPrinted >= 0 && k > lastPrinted+1 {
			out.WriteString(" ...\n")
		}
		fmt.Fprintf(&out, "%c %s\n", op.op, op.text)
		lastPrinted = k
	}
	return strings.TrimRight(out.String(), "\n")
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

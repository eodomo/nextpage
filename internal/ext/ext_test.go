package ext

import "testing"

func TestParseFrontmatter(t *testing.T) {
	meta, body := ParseFrontmatter("---\nname: review\ndescription: \"Review code\"\ntools: Read, Grep\n---\nDo it.\n")
	if meta["name"] != "review" || meta["description"] != "Review code" || body != "Do it.\n" {
		t.Errorf("meta=%v body=%q", meta, body)
	}
	if got := splitList(meta["tools"]); len(got) != 2 || got[1] != "Grep" {
		t.Errorf("tools = %v", got)
	}
}

func TestExpandArgs(t *testing.T) {
	if got := ExpandArgs("fix issue $1 in $ARGUMENTS", "42 web"); got != "fix issue 42 in 42 web" {
		t.Errorf("got %q", got)
	}
	if got := ExpandArgs("no placeholder", "x"); got != "no placeholder\n\nARGUMENTS: x" {
		t.Errorf("got %q", got)
	}
}

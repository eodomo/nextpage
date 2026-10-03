package learn

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eodomo/nextpage/internal/tools"
)

type harness struct {
	t     *testing.T
	m     *Manager
	env   *tools.Env
	tools map[string]tools.Tool
	// answers is replied to the next quiz.
	answers []string
	lastReq QuizRequest
}

func newHarness(t *testing.T) *harness {
	dir := t.TempDir()
	h := &harness{t: t, tools: map[string]tools.Tool{}}
	h.m = NewManager(Config{Dir: dir})
	h.env = tools.NewEnv(dir, dir)
	h.env.Interact = func(ctx context.Context, payload any) (any, error) {
		switch p := payload.(type) {
		case QuizRequest:
			h.lastReq = p
			return h.answers, nil
		case DirectoryRequest:
			return p.Default, nil
		}
		return nil, nil
	}
	for _, tl := range Tools(h.m) {
		h.tools[tl.Name()] = tl
	}
	return h
}

func (h *harness) call(name string, args any) tools.Result {
	h.t.Helper()
	b, _ := json.Marshal(args)
	return h.tools[name].Run(context.Background(), h.env, b)
}

func (h *harness) mustOK(name string, args any) tools.Result {
	h.t.Helper()
	r := h.call(name, args)
	if r.IsError {
		h.t.Fatalf("%s failed: %s", name, r.Output)
	}
	return r
}

func (h *harness) mustFail(name string, args any, contains string) {
	h.t.Helper()
	r := h.call(name, args)
	if !r.IsError || !strings.Contains(r.Output, contains) {
		h.t.Fatalf("%s: expected error containing %q, got %+v", name, contains, r)
	}
}

func mcQuiz(kind, section string) map[string]any {
	qs := []map[string]any{}
	for _, id := range []string{"q1", "q2", "q3"} {
		qs = append(qs, map[string]any{"id": id, "type": "multiple_choice", "prompt": "What is " + id + "?", "options": []string{"A) right", "B) wrong"}, "answer": "A"})
	}
	return map[string]any{"kind": kind, "section_id": section, "questions": qs}
}

var lesson = map[string]any{"title": "Intro", "content": strings.Repeat("Vectors have magnitude and direction. $v = (1, 2)$\n", 10)}

func withSection(m map[string]any, id string) map[string]any {
	out := map[string]any{"section_id": id}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func TestCourseWorkflow(t *testing.T) {
	h := newHarness(t)

	h.mustFail("GiveQuiz", mcQuiz("placement", ""), "no active course")
	h.mustOK("StartCourse", map[string]any{"topic": "linear algebra", "title": "Linear Algebra"})
	h.mustFail("SaveCoursePlan", map[string]any{"mermaid": "flowchart TD\n a", "sections": []map[string]any{{"id": "a", "title": "A", "objectives": []string{"x"}}}}, "placement quiz before planning")

	// Placement: answers are option text; one wrong.
	h.answers = []string{"A) right", "B) wrong", "A) right"}
	r := h.mustOK("GiveQuiz", mcQuiz("placement", ""))
	if !strings.Contains(r.Display, "2/3") {
		t.Fatalf("placement display = %q", r.Display)
	}
	if strings.Contains(mustJSON(h.lastReq), `"answer"`) {
		t.Fatal("quiz request leaked answers to the front end")
	}
	if c := h.m.Active(); c.Phase != PhasePlanning || c.Placement == nil {
		t.Fatalf("after placement: phase=%s", c.Phase)
	}

	// A diagram that doesn't use the section ids is replaced by a linear one.
	r = h.mustOK("SaveCoursePlan", map[string]any{"mermaid": "flowchart TD\n a --> b", "sections": "Vectors | add vectors\nMatrices | multiply"})
	if !strings.Contains(h.m.Active().Mermaid, `s1 --> s2`) {
		t.Fatalf("expected generated mermaid: %s\n%s", r.Output, h.m.Active().Mermaid)
	}
	// JSON arrays still work, and progress is kept for unchanged sections.
	h.mustOK("SaveCoursePlan", map[string]any{"mermaid": "flowchart TD\n s1[\"Vectors\"] --> s2[\"Matrices\"]", "sections": []map[string]any{
		{"id": "s1", "title": "Vectors", "objectives": []string{"add vectors"}},
		{"id": "s2", "title": "Matrices", "objectives": "multiply"},
	}})

	// A checkpoint before any lesson is refused; wrong section is refused.
	h.mustFail("GiveQuiz", mcQuiz("checkpoint", "s1"), "write a lesson")
	h.mustFail("WriteLesson", withSection(lesson, "s2"), "current section is \"s1\"")
	h.mustOK("WriteLesson", withSection(lesson, "s1"))

	// The checkpoint waits until the user has been back after the lesson.
	if r := h.call("GiveQuiz", mcQuiz("checkpoint", "s1")); !r.EndTurn || !strings.Contains(r.Display, "lesson for \"Vectors\" is ready") {
		t.Fatalf("expected the turn to end for reading, got %+v", r)
	}
	h.m.UserSpoke("ready")

	// Fail the checkpoint → remedial lesson required before re-quizzing.
	h.answers = []string{"B) wrong", "B) wrong", "A) right"}
	r = h.mustOK("GiveQuiz", mcQuiz("checkpoint", "s1"))
	if !strings.Contains(r.Output, "Not passed") {
		t.Fatalf("expected failure, got %s", r.Output)
	}
	h.mustFail("GiveQuiz", mcQuiz("checkpoint", "s1"), "write a lesson")
	h.mustOK("WriteLesson", withSection(lesson, "s1"))
	h.m.UserSpoke("ready")

	// Pass → advances to s2.
	h.answers = []string{"A) right", "A) right", "A) right"}
	r = h.mustOK("GiveQuiz", mcQuiz("checkpoint", "s1"))
	if !strings.Contains(r.Output, "Next section: \"s2\"") {
		t.Fatalf("expected advance, got %s", r.Output)
	}

	// Short answers wait for GradeQuiz.
	h.mustOK("WriteLesson", withSection(lesson, "s2"))
	h.m.UserSpoke("ready")
	q := map[string]any{"kind": "checkpoint", "section_id": "s2", "quiz": `Q: Define a matrix.
ANSWER: A rectangular array of numbers

Q: Which is the 2x2 identity?
A) [[1,0],[0,1]]
B) [[0,1],[1,0]]
ANSWER: A

Q: What is AB if A is 2x3 and B is 3x2?
ANSWER: A 2x2 matrix`}
	h.answers = []string{"a grid of numbers", "[[1,0],[0,1]]", "2 by 2"}
	r = h.mustOK("GiveQuiz", q)
	if !strings.Contains(r.Output, "GradeQuiz") || h.m.Active().Pending == nil {
		t.Fatalf("expected pending grading: %s", r.Output)
	}
	h.mustFail("WriteLesson", withSection(lesson, "s2"), "GradeQuiz")
	h.mustFail("GradeQuiz", map[string]any{"grades": "q1: correct | good"}, "still ungraded: q3")
	r = h.mustOK("GradeQuiz", map[string]any{"grades": "q1: correct | good\n- q3: Correct - Right.", "gaps": "none; really"})
	if !strings.Contains(r.Output, "course is complete") || h.m.Active().Phase != PhaseComplete {
		t.Fatalf("expected completion: %s", r.Output)
	}

	// Files for Obsidian.
	dir := filepath.Join(h.m.Dir(), "Linear Algebra")
	for _, f := range []string{PlanFile, "01 Vectors - Lesson 1.md", "01 Vectors - Lesson 2 (review).md", "02 Matrices - Lesson 1.md",
		"Quizzes/Placement Quiz.md", "Quizzes/01 Vectors - Checkpoint 1.md", "Quizzes/01 Vectors - Checkpoint 2.md", "Quizzes/02 Matrices - Checkpoint 1.md"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}
	plan, _ := os.ReadFile(filepath.Join(dir, PlanFile))
	for _, want := range []string{"```mermaid", "class s1,s2 done", "[[01 Vectors - Lesson 1]]", "- [x] **2. Matrices**"} {
		if !strings.Contains(string(plan), want) {
			t.Errorf("plan missing %q:\n%s", want, plan)
		}
	}

	// Courses are listed and can be re-opened by a new manager.
	m2 := NewManager(Config{Dir: h.m.Dir()})
	if list := m2.List(); len(list) != 1 || list[0].Passed != 2 {
		t.Fatalf("list = %+v", list)
	}
	if _, err := m2.Open("linear algebra"); err != nil {
		t.Fatal(err)
	}
	if _, err := m2.ReadFile("../../etc/passwd"); err == nil {
		t.Fatal("path traversal not refused")
	}
}

func TestCorrectOption(t *testing.T) {
	opts := []string{"A) four", "B) five", "C) six"}
	for answer, want := range map[string]int{"B": 1, "b)": 1, "(c)": 2, "2": 1, "five": 1, "B) five": 1, "seven": -1} {
		if got := correctOption(&Question{Options: opts, Answer: answer}); got != want {
			t.Errorf("correctOption(%q) = %d, want %d", answer, got, want)
		}
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestParseQuizText(t *testing.T) {
	qs := ParseQuizText(`Here is the quiz.

**Q1:** What is $2+2$?
A) 3
B) 4
   (the even one)
ANSWER: B
EXPLANATION: Basic addition.

Question 2: Explain why
the sky is blue.
Answer: Rayleigh scattering.
`)
	if len(qs) != 2 {
		t.Fatalf("got %d questions: %+v", len(qs), qs)
	}
	if qs[0].Type != TypeMultipleChoice || len(qs[0].Options) != 2 || qs[0].Options[1] != "4 (the even one)" || correctOption(&qs[0]) != 1 || qs[0].Explanation != "Basic addition." {
		t.Errorf("q1 = %+v", qs[0])
	}
	if qs[1].Type != TypeShortAnswer || qs[1].Prompt != "Explain why\nthe sky is blue." || qs[1].Answer != "Rayleigh scattering." {
		t.Errorf("q2 = %+v", qs[1])
	}
}

func TestParseQuizTextInline(t *testing.T) {
	qs := ParseQuizText("Q: What is a BST? A) A list B) An ordered tree C) A heap ANSWER: B\n\nQ: Search cost when unbalanced? A) O(1) B) O(log n) C) O(n) ANSWER: C Q: Why balance? ANSWER: To keep height O(log n).")
	if len(qs) != 3 {
		t.Fatalf("got %d questions: %+v", len(qs), qs)
	}
	if len(qs[0].Options) != 3 || qs[0].Options[1] != "An ordered tree" || correctOption(&qs[0]) != 1 {
		t.Errorf("q1 = %+v", qs[0])
	}
	if correctOption(&qs[1]) != 2 || qs[2].Type != TypeShortAnswer || qs[2].Answer != "To keep height O(log n)." {
		t.Errorf("q2/q3 = %+v %+v", qs[1], qs[2])
	}
}

func TestInlineSectionsAndCapture(t *testing.T) {
	h := newHarness(t)
	var capt tools.Capture
	h.env.CaptureReply = func(c tools.Capture) { capt = c }
	captured := func(text string) tools.Result { return capt.Handle(context.Background(), text) }

	h.mustOK("StartCourse", map[string]any{"topic": "BSTs"})
	h.answers = []string{"A) right", "A) right", "A) right"}
	h.mustOK("GiveQuiz", mcQuiz("placement", ""))
	h.mustOK("SaveCoursePlan", map[string]any{"sections": "s1 What a BST is | define it; spot invalid ones; s2 Searching | trace a search; s3 Balancing | explain height"})
	c := h.m.Active()
	if len(c.Sections) != 3 || c.Sections[1].ID != "s2" || c.Sections[1].Title != "Searching" || len(c.Sections[0].Objectives) != 2 {
		t.Fatalf("sections = %+v", c.Sections)
	}

	// Two-step lesson: the tool arms a capture, the next reply is saved.
	r := h.mustOK("WriteLesson", map[string]any{"section_id": "s1", "title": "BST basics"})
	if capt.Handle == nil || capt.Hidden || !strings.Contains(h.m.StatePrompt(), "next reply must be the complete lesson text") {
		t.Fatalf("capture not armed: %s", r.Output)
	}
	if res := captured("too short"); !res.IsError {
		t.Fatal("short capture accepted")
	}
	res := captured(strings.Repeat("A BST keeps smaller keys on the left. ", 10))
	if res.IsError || h.m.Awaiting() != "" || !strings.Contains(res.Display, "01 What a BST is - Lesson 1.md") {
		t.Fatalf("capture save: %+v", res)
	}
	if h.m.Active().Sections[0].LessonsSinceQuiz != 1 {
		t.Fatal("lesson not recorded")
	}
}

func TestDrawMermaid(t *testing.T) {
	secs := []*Section{{ID: "s1", Title: "Basics"}, {ID: "s2", Title: "Search"}, {ID: "s3", Title: `Balance "AVL"`}}
	// The model's broken one-line diagram: its arrows are kept, labels redrawn.
	got := drawMermaid(`flowchart TD s1[:Basics] --> s2[:Search] s2[:Search] --> s3[:Balance]`, secs)
	want := "flowchart TD\n    s1[\"1 · Basics\"]\n    s2[\"2 · Search\"]\n    s3[\"3 · Balance 'AVL'\"]\n    s1 --> s2\n    s2 --> s3"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	// Branches survive.
	if got := drawMermaid("s1 --> s2\ns1 --> s3", secs); !strings.Contains(got, "s1 --> s3") || strings.Contains(got, "s2 --> s3") {
		t.Errorf("branching lost:\n%s", got)
	}
	// Arrows that miss a section fall back to a linear path.
	if got := drawMermaid("s1 --> s2", secs); !strings.Contains(got, "s2 --> s3") {
		t.Errorf("expected linear fallback:\n%s", got)
	}
}

func TestCleanLesson(t *testing.T) {
	in := "$$\n\\text{What a BST is} \\\\\n\n\\text{A binary search tree} is a data structure that keeps keys ordered for fast lookups.\n\nReal math stays: $O(\\log n)$\n$$\n\nNow a real equation:\n$$\nh = \\lfloor \\log_2 n \\rfloor\n$$"
	got := cleanLesson(in)
	for _, want := range []string{"## What a BST is", "A binary search tree is a data structure", "$O(\\log n)$", "$$\nh = \\lfloor \\log_2 n \\rfloor\n$$"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.HasPrefix(got, "$$") {
		t.Errorf("prose block not unwrapped:\n%s", got)
	}
}

func TestResumeAndDuplicates(t *testing.T) {
	h := newHarness(t)
	h.mustOK("StartCourse", map[string]any{"topic": "graphs", "title": "Graph Theory"})
	h.mustFail("StartCourse", map[string]any{"topic": "Graph Theory"}, `Call OpenCourse with name "Graph Theory"`)
	h.mustOK("StartCourse", map[string]any{"topic": "Graph Theory", "new": true})

	// A fresh manager (a restart) picks up the most recent unfinished course.
	m2 := NewManager(Config{})
	if err := m2.SetDir(h.m.Dir()); err != nil {
		t.Fatal(err)
	}
	if c := m2.Active(); c == nil || c.Folder != "Graph Theory 2" {
		t.Fatalf("resumed %+v", c)
	}
}

func TestQuizAsCapturedReply(t *testing.T) {
	h := newHarness(t)
	var capt tools.Capture
	h.env.CaptureReply = func(c tools.Capture) { capt = c }
	h.mustOK("StartCourse", map[string]any{"topic": "BSTs"})
	r := h.mustOK("GiveQuiz", map[string]any{"kind": "placement"})
	if !capt.Hidden || capt.Name != "GiveQuiz" || !strings.Contains(r.Output, "next reply") {
		t.Fatalf("capture = %+v, result %+v", capt, r)
	}
	if res := capt.Handle(context.Background(), "just chatting"); !res.IsError {
		t.Fatal("unparseable quiz accepted")
	}
	h.answers = []string{"x", "x", "x"}
	res := capt.Handle(context.Background(), "Q: a? A) x B) y ANSWER: A\nQ: b? A) x B) y ANSWER: A\nQ: c? A) x B) y ANSWER: B")
	if res.IsError || !strings.Contains(res.Display, "2/3") || h.m.Active().Phase != PhasePlanning || h.m.Awaiting() != "" {
		t.Fatalf("captured quiz: %+v phase=%s", res, h.m.Active().Phase)
	}
}

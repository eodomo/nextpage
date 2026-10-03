package learn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/eodomo/nextpage/internal/tools"
)

// Tools returns the course tools bound to m.
func Tools(m *Manager) []tools.Tool {
	return []tools.Tool{
		&startCourse{m}, &openCourse{m}, &courseStatus{m}, &giveQuiz{m},
		&gradeQuiz{m}, &saveCoursePlan{m}, &writeLesson{m},
	}
}

func decode[T any](in json.RawMessage) (T, error) {
	var v T
	if len(in) == 0 {
		in = []byte("{}")
	}
	if err := json.Unmarshal(in, &v); err != nil {
		return v, fmt.Errorf("invalid tool input: %w", err)
	}
	return v, nil
}

func fail(err error) tools.Result { return tools.Result{Output: err.Error(), IsError: true} }

// ---- StartCourse ----

type startCourse struct{ m *Manager }

func (*startCourse) Name() string     { return "StartCourse" }
func (*startCourse) Kind() tools.Kind { return tools.KindInternal }
func (*startCourse) Description() string {
	return "Starts a new course on the topic the user wants to learn and makes it the active course. Call this once the topic is clear. Next you must give the placement quiz."
}
func (*startCourse) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"topic":{"type":"string","description":"What the user wants to learn, as they described it"},
"title":{"type":"string","description":"Short course title, e.g. \"Linear Algebra Foundations\""},
"new":{"type":"boolean","description":"Start over even if a course on this topic exists"}},
"required":["topic"]}`)
}
func (t *startCourse) Subject(in json.RawMessage) string {
	a, _ := decode[struct{ Topic string }](in)
	return a.Topic
}

func (t *startCourse) Run(ctx context.Context, env *tools.Env, in json.RawMessage) tools.Result {
	a, err := decode[struct {
		Topic, Title string
		New          bool `json:"new"`
	}](in)
	if err != nil {
		return fail(err)
	}
	if strings.TrimSpace(a.Topic) == "" {
		return fail(errors.New("topic is required"))
	}
	for _, name := range []string{a.Title, a.Topic} {
		if s, ok := t.m.find(name); ok && name != "" && !a.New {
			return fail(fmt.Errorf("a course %q already exists (%s, %d/%d sections passed). Call OpenCourse with name %q to continue it. Only if the user explicitly wants to start over, call StartCourse again with \"new\": true", s.Folder, s.Phase, s.Passed, s.Sections, s.Folder))
		}
	}
	if t.m.Dir() == "" {
		if err := askForDir(ctx, env, t.m); err != nil {
			return fail(err)
		}
	}
	c, err := t.m.Create(a.Topic, a.Title)
	if err != nil {
		return fail(err)
	}
	dir := t.m.folder(c)
	return tools.Result{
		Output:  fmt.Sprintf("Course %q created in %s. Now give the placement quiz with GiveQuiz (kind \"placement\").", c.Title, dir),
		Display: "Created " + dir,
	}
}

func askForDir(ctx context.Context, env *tools.Env, m *Manager) error {
	home, _ := os.UserHomeDir()
	def := filepath.Join(home, "nextpage-courses")
	if env.Interact == nil {
		return m.SetDir(def)
	}
	reply, err := env.Interact(ctx, DirectoryRequest{Default: def})
	if err != nil {
		return err
	}
	dir, _ := reply.(string)
	if strings.TrimSpace(dir) == "" {
		dir = def
	}
	return m.SetDir(dir)
}

// ---- OpenCourse ----

type openCourse struct{ m *Manager }

func (*openCourse) Name() string     { return "OpenCourse" }
func (*openCourse) Kind() tools.Kind { return tools.KindInternal }
func (*openCourse) Description() string {
	return "Re-opens an existing course (listed in the system prompt) so the user can continue where they left off."
}
func (*openCourse) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"Course folder or title"}},"required":["name"]}`)
}
func (t *openCourse) Subject(in json.RawMessage) string {
	a, _ := decode[struct{ Name string }](in)
	return a.Name
}
func (t *openCourse) Run(ctx context.Context, env *tools.Env, in json.RawMessage) tools.Result {
	a, err := decode[struct{ Name string }](in)
	if err != nil {
		return fail(err)
	}
	c, err := t.m.Open(a.Name)
	if err != nil {
		return fail(err)
	}
	return tools.Result{Output: fmt.Sprintf("Opened %q.\n\n%s", c.Title, t.m.StatePrompt()), Display: "Opened " + c.Title}
}

// ---- CourseStatus ----

type courseStatus struct{ m *Manager }

func (*courseStatus) Name() string     { return "CourseStatus" }
func (*courseStatus) Kind() tools.Kind { return tools.KindRead }
func (*courseStatus) Description() string {
	return "Shows the active course's progress and the required next step, or lists courses if none is active."
}
func (*courseStatus) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (t *courseStatus) Run(ctx context.Context, env *tools.Env, in json.RawMessage) tools.Result {
	return tools.Result{Output: t.m.StatePrompt()}
}

// ---- GiveQuiz ----

type giveQuiz struct{ m *Manager }

func (*giveQuiz) Name() string     { return "GiveQuiz" }
func (*giveQuiz) Kind() tools.Kind { return tools.KindInternal }
func (*giveQuiz) Description() string {
	return `Presents a quiz to the user and returns their graded answers. Never reveal answers in chat before the quiz.
- kind "placement": once, right after StartCourse. 6-10 questions from basics to advanced, to find what the user already knows.
- kind "checkpoint": after the user has read the current section's lesson. 4-8 questions on that section's objectives. The user must reach the pass mark to advance.
Write the questions in "quiz" as plain text, a blank line between questions:

Q: Which traversal visits a BST's keys in sorted order?
A) Preorder
B) Inorder
C) Postorder
ANSWER: B
EXPLANATION: Inorder visits left subtree, node, right subtree.

Q: Why can searching a BST take O(n) time?
ANSWER: If keys are inserted in sorted order the tree degenerates into a linked list.

Questions with A) B) C) options are multiple choice (ANSWER is the letter). Questions without options are short answer (ANSWER is a model answer for grading). Mix both kinds. Test understanding, not trivia. LaTeX math ($...$) is allowed.`
}
func (*giveQuiz) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"kind":{"type":"string","enum":["placement","checkpoint"]},
"section_id":{"type":"string","description":"For checkpoint quizzes: the current section's id, e.g. s2"},
"title":{"type":"string"},
"quiz":{"type":"string","description":"The questions in the Q:/A)/ANSWER: text format. If omitted, send them as your next reply instead"}},
"required":["kind"]}`)
}
func (t *giveQuiz) Subject(in json.RawMessage) string {
	a, _ := decode[struct{ Kind, Title string }](in)
	return strings.TrimSpace(a.Kind + " " + a.Title)
}

func (t *giveQuiz) Run(ctx context.Context, env *tools.Env, in json.RawMessage) tools.Result {
	a, err := decode[struct {
		Kind      string    `json:"kind"`
		SectionID string    `json:"section_id"`
		Title     string    `json:"title"`
		Quiz      Questions `json:"quiz"`
		Questions Questions `json:"questions"` // accepted for models that send JSON
	}](in)
	if err != nil {
		return fail(err)
	}
	if len(a.Questions) == 0 {
		a.Questions = a.Quiz
	}
	c := t.m.Active()
	if c == nil {
		return fail(t.m.noActive())
	}
	if c.Pending != nil {
		return fail(fmt.Errorf("quiz %q still has ungraded short answers; call GradeQuiz first", c.Pending.ID))
	}

	var quizID string
	switch a.Kind {
	case QuizPlacement:
		if c.Phase != PhasePlacement {
			return fail(fmt.Errorf("the placement quiz is already done (phase: %s)", c.Phase))
		}
		quizID = "placement"
		if a.Title == "" {
			a.Title = "Placement Quiz"
		}
		a.SectionID = ""
	case QuizCheckpoint:
		if c.Phase != PhaseLearning {
			return fail(fmt.Errorf("checkpoint quizzes happen during the learning phase (phase: %s)", c.Phase))
		}
		sec := c.CurrentSection()
		if a.SectionID == "" {
			a.SectionID = sec.ID
		}
		if a.SectionID != sec.ID {
			return fail(fmt.Errorf("the current section is %q (%s); checkpoints must be for the current section", sec.ID, sec.Title))
		}
		if sec.LessonsSinceQuiz == 0 {
			return fail(fmt.Errorf("write a lesson for section %q with WriteLesson before quizzing it", sec.ID))
		}
		if t.m.awaitingReader() {
			return tools.Result{
				Output:  "The user has not read the new lesson yet; the checkpoint will be given after they reply.",
				Display: fmt.Sprintf("📖 Your new lesson for %q is ready. Read it, then tell me when you want the checkpoint quiz.", sec.Title),
				EndTurn: true,
			}
		}
		quizID = fmt.Sprintf("%s-checkpoint-%d", sec.ID, sec.Checkpoints+1)
		if a.Title == "" {
			a.Title = fmt.Sprintf("%s — Checkpoint %d", sec.Title, sec.Checkpoints+1)
		}
	default:
		return fail(fmt.Errorf("kind must be \"placement\" or \"checkpoint\""))
	}
	q := quizParams{id: quizID, kind: a.Kind, sectionID: a.SectionID, title: a.Title, course: c.Title}
	if len(a.Questions) > 0 {
		return t.present(ctx, env, q, a.Questions)
	}
	// No questions in the call: take them as the next reply, hidden from the
	// user because the text includes the answers.
	if env.CaptureReply == nil {
		return fail(errors.New(`put the questions in "quiz"`))
	}
	t.m.setAwaiting("the quiz questions in the Q: / A) / ANSWER: format")
	env.CaptureReply(tools.Capture{Name: "GiveQuiz", Hidden: true, Handle: func(ctx context.Context, text string) tools.Result {
		r := t.present(ctx, env, q, ParseQuizText(text))
		if !r.IsError {
			t.m.setAwaiting("")
		}
		return r
	}})
	return tools.Result{
		Output:  "Ready. Your next reply must be only the quiz questions in the Q: / A) / ANSWER: format (they are hidden from the user and shown as a quiz).",
		Display: "Preparing the quiz…",
	}
}

type quizParams struct {
	id, kind, sectionID, title, course string
}

// present validates questions, shows the quiz, and records the result.
func (t *giveQuiz) present(ctx context.Context, env *tools.Env, p quizParams, questions []Question) tools.Result {
	if err := validateQuestions(questions); err != nil {
		return fail(fmt.Errorf("%w. Write the quiz as plain text: \"Q: ...\" then options \"A) ...\" and \"ANSWER: B\", a blank line between questions", err))
	}
	req := QuizRequest{ID: p.id, Kind: p.kind, Title: p.title, Course: p.course}
	for _, q := range questions {
		req.Questions = append(req.Questions, PublicQuestion{ID: q.ID, Type: q.Type, Prompt: q.Prompt, Options: q.Options})
	}
	if env.Interact == nil {
		return fail(errors.New("no user is available to take a quiz"))
	}
	reply, err := env.Interact(ctx, req)
	if err != nil {
		return fail(err)
	}
	answers := toStrings(reply)
	if answers == nil {
		return fail(errors.New("the quiz could not be shown to the user (no interactive front end); ask them to use the TUI or web app"))
	}

	result := grade(p.id, p.kind, p.sectionID, p.title, questions, answers)
	var msg string
	err = t.m.update(func(c *Course) error {
		if len(result.Ungraded()) > 0 {
			c.Pending = result
			msg = result.Report() + "\nShort answers need grading: call GradeQuiz with a grade for each of: " + ungradedIDs(result) + "."
			return nil
		}
		var err error
		msg, err = t.m.apply(c, result)
		return err
	})
	if err != nil {
		return fail(err)
	}
	return tools.Result{Output: msg, Display: quizDisplay(result)}
}

func ungradedIDs(q *QuizResult) string {
	var ids []string
	for _, r := range q.Ungraded() {
		ids = append(ids, r.ID)
	}
	return strings.Join(ids, ", ")
}

func quizDisplay(r *QuizResult) string {
	if len(r.Ungraded()) > 0 {
		return fmt.Sprintf("%d answers submitted; grading…", len(r.Results))
	}
	verdict := "not passed"
	if r.Kind == QuizPlacement {
		verdict = "placement"
	} else if r.Passed {
		verdict = "passed"
	}
	return fmt.Sprintf("Score %d/%d (%d%%) · %s", r.Score, r.Total, r.Percent, verdict)
}

// toStrings converts a front end's reply ([]string from Go front ends, []any
// from JSON) into answers.
func toStrings(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, len(x))
		for i, e := range x {
			out[i] = fmt.Sprint(e)
		}
		return out
	}
	return nil
}

// apply records a fully graded quiz and advances the course. It returns the
// instructions for the model.
func (m *Manager) apply(c *Course, r *QuizResult) (string, error) {
	r.finalize(m.cfg.PassPercent)
	c.Pending = nil

	var name string
	if r.Kind == QuizPlacement {
		name = "Quizzes/Placement Quiz.md"
	} else {
		idx, sec := c.section(r.SectionID)
		if sec == nil {
			return "", fmt.Errorf("section %q no longer exists in the plan", r.SectionID)
		}
		name = fmt.Sprintf("Quizzes/%02d %s - Checkpoint %d.md", idx+1, SafeName(sec.Title), sec.Checkpoints+1)
	}
	r.File = name
	if _, err := m.writeNote(c, name, renderQuizNote(c, r)); err != nil {
		return "", err
	}
	c.Quizzes = append(c.Quizzes, r)
	report := r.Report()

	if r.Kind == QuizPlacement {
		c.Placement = r
		c.Phase = PhasePlanning
		return report + "\nPlacement recorded. Briefly tell the user how they did, then design the course with SaveCoursePlan, focusing on what they got wrong and skipping what they clearly know.", nil
	}

	idx, sec := c.section(r.SectionID)
	sec.Checkpoints++
	sec.LessonsSinceQuiz = 0
	if !r.Passed {
		return report + fmt.Sprintf("\nNot passed (pass mark %d%%). Explain the mistakes kindly, then write a remedial lesson for section %q with WriteLesson that re-teaches these gaps in a different way, with more worked examples: %s",
			m.cfg.PassPercent, sec.ID, strings.Join(r.Gaps, "; ")), nil
	}
	sec.Passed = true
	next := -1
	for i := idx + 1; i < len(c.Sections); i++ {
		if !c.Sections[i].Passed {
			next = i
			break
		}
	}
	if next < 0 {
		c.Phase = PhaseComplete
		return report + "\nPassed — and that was the final section. The course is complete! Congratulate the user, summarize what they learned, and suggest where to go next.", nil
	}
	c.Current = next
	ns := c.Sections[next]
	return report + fmt.Sprintf("\nPassed! Section %q is complete. Next section: %q (%s). Write its lesson with WriteLesson now.", sec.Title, ns.ID, ns.Title), nil
}

// ---- GradeQuiz ----

type gradeQuiz struct{ m *Manager }

func (*gradeQuiz) Name() string     { return "GradeQuiz" }
func (*gradeQuiz) Kind() tools.Kind { return tools.KindInternal }
func (*gradeQuiz) Description() string {
	return "Grades the short-answer questions of the pending quiz. Judge each answer against the reference: correct if it shows the key understanding, even if worded differently. Give brief, specific feedback. Also list the user's strengths and knowledge gaps on this quiz."
}
func (*gradeQuiz) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"quiz_id":{"type":"string"},
"grades":{"type":"string","description":"One line per short-answer question: \"q3: correct | feedback\" or \"q5: incorrect | feedback\""},
"strengths":{"type":"string","description":"What the user understands, separated by semicolons"},
"gaps":{"type":"string","description":"What the user still needs to learn, separated by semicolons"}},
"required":["grades"]}`)
}

type gradeEntry struct {
	QuestionID string `json:"question_id"`
	Correct    bool   `json:"correct"`
	Feedback   string `json:"feedback"`
}

// Grades accepts a JSON array of grade objects or text lines like
// "q3: correct | feedback".
type Grades []gradeEntry

var reGrade = regexp.MustCompile(`(?i)^\s*[-*]?\s*\[?(q?\d+|\w+)\]?\s*[:\-–=]\s*(correct|incorrect|right|wrong|yes|no|true|false|pass|fail)\b\s*[|:\-–,.]?\s*(.*)$`)

func (g *Grades) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*g = nil
		for _, line := range strings.Split(s, "\n") {
			m := reGrade.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			id := strings.ToLower(m[1])
			if !strings.HasPrefix(id, "q") {
				id = "q" + id
			}
			v := strings.ToLower(m[2])
			ok := v == "correct" || v == "right" || v == "yes" || v == "true" || v == "pass"
			*g = append(*g, gradeEntry{QuestionID: id, Correct: ok, Feedback: strings.TrimSpace(m[3])})
		}
		return nil
	}
	var arr []gradeEntry
	if err := json.Unmarshal(b, &arr); err != nil {
		return err
	}
	*g = arr
	return nil
}
func (t *gradeQuiz) Run(ctx context.Context, env *tools.Env, in json.RawMessage) tools.Result {
	a, err := decode[struct {
		QuizID    string   `json:"quiz_id"`
		Grades    Grades   `json:"grades"`
		Strengths TextList `json:"strengths"`
		Gaps      TextList `json:"gaps"`
	}](in)
	if err != nil {
		return fail(err)
	}
	var msg string
	var result *QuizResult
	err = t.m.update(func(c *Course) error {
		p := c.Pending
		if p == nil {
			msg = "Nothing to grade: the last quiz had no short answers and was graded automatically. Continue with the next step."
			return nil
		}
		if a.QuizID != "" && a.QuizID != p.ID {
			return fmt.Errorf("the pending quiz is %q, not %q", p.ID, a.QuizID)
		}
		for _, g := range a.Grades {
			for i := range p.Results {
				if p.Results[i].ID == g.QuestionID {
					ok := g.Correct
					p.Results[i].Correct = &ok
					if g.Feedback != "" {
						p.Results[i].Feedback = g.Feedback
					}
				}
			}
		}
		if rest := p.Ungraded(); len(rest) > 0 {
			return fmt.Errorf("still ungraded: %s. Send one line per question, e.g. \"%s: correct | feedback\"", ungradedIDs(p), rest[0].ID)
		}
		p.Strengths, p.Gaps = a.Strengths, a.Gaps
		result = p
		var err error
		msg, err = t.m.apply(c, p)
		return err
	})
	if err != nil {
		return fail(err)
	}
	if result == nil {
		return tools.Result{Output: msg, Display: "Already graded"}
	}
	return tools.Result{Output: msg, Display: quizDisplay(result)}
}

// ---- SaveCoursePlan ----

type saveCoursePlan struct{ m *Manager }

func (*saveCoursePlan) Name() string     { return "SaveCoursePlan" }
func (*saveCoursePlan) Kind() tools.Kind { return tools.KindInternal }
func (*saveCoursePlan) Description() string {
	return `Saves the course plan after the placement quiz. Order 3-8 sections from foundations to advanced; skip or compress what the placement quiz showed the user already knows, and give extra room to their gaps.
Write "sections" as plain text, one section per line: title | objective; objective; objective
For example:
What a BST is | define the BST property; recognise valid and invalid BSTs
Searching and inserting | trace a search; insert keys and draw the result

Sections get the ids s1, s2, s3... in order. "mermaid" is optional: only needed when the path branches. List arrows between section ids, one per line, e.g. "s1 --> s2", "s1 --> s3". The diagram is drawn for you from the sections; without arrows the path is linear.
Can be called again later to revise the plan; sections already passed keep their progress.`
}
func (*saveCoursePlan) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"sections":{"type":"string","description":"One section per line: title | objective; objective"},
"mermaid":{"type":"string","description":"Optional arrows between section ids for a branching path, e.g. s1 --> s2"},
"title":{"type":"string","description":"Optional new course title"}},
"required":["sections"]}`)
}

type planSection struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Objectives TextList `json:"objectives"`
}

// PlanSections accepts a JSON array of sections or text lines
// "title | objective; objective". Missing ids become s1, s2, ...
type PlanSections []planSection

var reListPrefix = regexp.MustCompile(`^\s*(?:[-*•]|\d+[.)]|s\d+\s*[:.)-])\s*`)

var reInlineSection = regexp.MustCompile(`(?:^|[;\n])\s*(?:[-*•]\s*)?(s\d+)\b\s*[:.)\-]?\s*`)

func (p *PlanSections) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*p = nil
		// Sections written on one line with ids ("s1 A | x; s2 B | y")
		// are split at each id.
		if locs := reInlineSection.FindAllStringSubmatchIndex(s, -1); len(locs) > 1 {
			for i, l := range locs {
				end := len(s)
				if i+1 < len(locs) {
					end = locs[i+1][0]
				}
				p.add(s[l[2]:l[3]], s[l[1]:end])
			}
			return nil
		}
		for _, line := range strings.Split(s, "\n") {
			line = strings.TrimSpace(reListPrefix.ReplaceAllString(line, ""))
			if line == "" {
				continue
			}
			p.add("", line)
		}
		return nil
	}
	var arr []planSection
	if err := json.Unmarshal(b, &arr); err != nil {
		return err
	}
	*p = arr
	return nil
}

// add parses "title | objective; objective".
func (p *PlanSections) add(id, line string) {
	title, objs, _ := strings.Cut(strings.TrimSpace(line), "|")
	title = strings.Trim(strings.TrimSpace(title), "*")
	if title == "" {
		return
	}
	var list TextList
	_ = list.UnmarshalJSON(mustMarshal(objs))
	*p = append(*p, planSection{ID: id, Title: title, Objectives: list})
}

func mustMarshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

var sectionIDRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

func (t *saveCoursePlan) Run(ctx context.Context, env *tools.Env, in json.RawMessage) tools.Result {
	a, err := decode[struct {
		Title    string       `json:"title"`
		Mermaid  string       `json:"mermaid"`
		Sections PlanSections `json:"sections"`
	}](in)
	if err != nil {
		return fail(err)
	}
	if len(a.Sections) < 1 || len(a.Sections) > 12 {
		return fail(fmt.Errorf("a plan needs 1-12 sections, got %d. Write one section per line: title | objective; objective", len(a.Sections)))
	}
	var newSecs []*Section
	seen := map[string]bool{}
	for i, s := range a.Sections {
		id := s.ID
		if id == "" {
			id = fmt.Sprintf("s%d", i+1)
		}
		if !sectionIDRe.MatchString(id) {
			return fail(fmt.Errorf("section id %q must be letters, digits and underscores, starting with a letter", id))
		}
		if seen[id] {
			return fail(fmt.Errorf("duplicate section id %q", id))
		}
		seen[id] = true
		newSecs = append(newSecs, &Section{ID: id, Title: s.Title, Objectives: s.Objectives})
	}
	mermaid := drawMermaid(a.Mermaid, newSecs)

	var msg string
	err = t.m.update(func(c *Course) error {
		switch c.Phase {
		case PhasePlacement:
			return errors.New("give the placement quiz before planning")
		case PhaseComplete:
			return errors.New("this course is complete; start a new course instead")
		}
		old := map[string]*Section{}
		for _, s := range c.Sections {
			old[s.ID] = s
		}
		for _, ns := range newSecs {
			if o, ok := old[ns.ID]; ok && o.Title == ns.Title {
				ns.Passed, ns.Lessons, ns.LessonsSinceQuiz, ns.Checkpoints = o.Passed, o.Lessons, o.LessonsSinceQuiz, o.Checkpoints
			}
		}
		c.Sections = newSecs
		c.Mermaid = mermaid
		if a.Title != "" {
			c.Title = a.Title
		}
		c.Current = len(newSecs)
		for i, s := range newSecs {
			if !s.Passed {
				c.Current = i
				break
			}
		}
		if c.Current == len(newSecs) {
			c.Phase = PhaseComplete
			msg = "Every section in this plan is already passed; the course is complete."
			return nil
		}
		c.Phase = PhaseLearning
		var list []string
		for _, s := range newSecs {
			list = append(list, fmt.Sprintf("%s: %s", s.ID, s.Title))
		}
		cur := newSecs[c.Current]
		msg = fmt.Sprintf("Plan saved to %q with sections %s.", PlanFile, strings.Join(list, ", "))
		msg += fmt.Sprintf(" Show the user the path briefly, then write the first lesson for section %q (%s) with WriteLesson.", cur.ID, cur.Title)
		return nil
	})
	if err != nil {
		return fail(err)
	}
	return tools.Result{Output: msg, Display: fmt.Sprintf("Saved plan with %d sections", len(newSecs))}
}

// ---- WriteLesson ----

type writeLesson struct{ m *Manager }

func (*writeLesson) Name() string     { return "WriteLesson" }
func (*writeLesson) Kind() tools.Kind { return tools.KindInternal }
func (t *writeLesson) Description() string {
	how := `Markdown with LaTeX math: $...$ inline, $$...$$ on their own lines for display math. You may include mermaid diagrams in fenced code blocks and Obsidian callouts like "> [!tip]". Structure: intuition/motivation, key ideas and definitions, worked examples, common mistakes, practice exercises with solutions hidden in collapsed callouts ("> [!example]- Solution"), and a short summary. Do not include the title heading; it is added for you.`
	if t.m.Format() == "latex" {
		how = `a complete, compilable LaTeX document (\documentclass{article} ... \end{document}): intuition first, then definitions, worked examples, practice exercises with solutions, and a summary.`
	}
	return `Starts a lesson for the current section. Call it with just section_id and title; then write the whole lesson as your next reply, and it is saved to the course automatically (the reply is not shown as a chat message). Aim the lesson at the gaps the user's quiz answers revealed. Write the lesson as ` + how
}
func (*writeLesson) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"section_id":{"type":"string","description":"The current section's id, e.g. s1"},
"title":{"type":"string","description":"Lesson title"}},
"required":["section_id","title"]}`)
}
func (t *writeLesson) Subject(in json.RawMessage) string {
	a, _ := decode[struct{ Title string }](in)
	return a.Title
}

func (t *writeLesson) Run(ctx context.Context, env *tools.Env, in json.RawMessage) tools.Result {
	a, err := decode[struct {
		SectionID string `json:"section_id"`
		Title     string `json:"title"`
		Content   string `json:"content"` // accepted from models that send the lesson inline
	}](in)
	if err != nil {
		return fail(err)
	}
	if err := t.check(a.SectionID); err != nil {
		return fail(err)
	}
	if strings.TrimSpace(a.Content) != "" {
		return t.save(a.SectionID, a.Title, a.Content)
	}
	if env.CaptureReply == nil {
		return fail(errors.New("pass the lesson in \"content\""))
	}
	t.m.setAwaiting(fmt.Sprintf("the complete lesson text for section %q", a.SectionID))
	env.CaptureReply(tools.Capture{Name: "WriteLesson", Handle: func(ctx context.Context, text string) tools.Result {
		r := t.save(a.SectionID, a.Title, text)
		if !r.IsError {
			t.m.setAwaiting("")
		}
		return r
	}})
	return tools.Result{
		Output:  "Ready. Your next reply must be the complete lesson text itself (no preamble like \"Here is the lesson\", no tool calls). It will be saved as the lesson file.",
		Display: "Writing the lesson…",
	}
}

// check verifies a lesson can be written for sectionID right now.
func (t *writeLesson) check(sectionID string) error {
	c := t.m.Active()
	switch {
	case c == nil:
		return t.m.noActive()
	case c.Phase != PhaseLearning:
		return fmt.Errorf("lessons are written during the learning phase (phase: %s)", c.Phase)
	case c.Pending != nil:
		return errors.New("grade the pending quiz with GradeQuiz first")
	}
	if sec := c.CurrentSection(); sec.ID != sectionID {
		return fmt.Errorf("the current section is %q (%s); write lessons for it before moving on", sec.ID, sec.Title)
	}
	return nil
}

func (t *writeLesson) save(sectionID, title, content string) tools.Result {
	content = strings.TrimSpace(content)
	if len(content) < 200 {
		return fail(errors.New("that was too short to be a lesson; reply with the complete lesson text"))
	}
	var path, msg string
	err := t.m.update(func(c *Course) error {
		if c.Phase != PhaseLearning || c.Pending != nil {
			return fmt.Errorf("the course is not ready for a lesson (phase: %s)", c.Phase)
		}
		sec := c.CurrentSection()
		if sec.ID != sectionID {
			return fmt.Errorf("the current section is %q", sec.ID)
		}
		n := len(sec.Lessons) + 1
		label := ""
		if sec.Checkpoints > 0 {
			label = " (review)"
		}
		name := fmt.Sprintf("%02d %s - Lesson %d%s%s", c.Current+1, SafeName(sec.Title), n, label, t.m.lessonExt())
		body := content
		if t.m.Format() != "latex" {
			body = renderLesson(c, sec, n, title, cleanLesson(content))
		}
		var err error
		if path, err = t.m.writeNote(c, name, body); err != nil {
			return err
		}
		sec.Lessons = append(sec.Lessons, name)
		sec.LessonsSinceQuiz++
		t.m.reading = true
		msg = fmt.Sprintf("Lesson saved to %s. Now tell the user it is ready to read (in Obsidian or the web app) with a two-sentence preview, and offer to answer questions. When they say they are ready, give the checkpoint quiz for section %q.", path, sec.ID)
		return nil
	})
	if err != nil {
		return fail(err)
	}
	return tools.Result{Output: msg, Display: "Saved " + path}
}

func renderLesson(c *Course, sec *Section, n int, title, content string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "---\ntags: [nextpage, lesson]\ncourse: %q\nsection: %q\nlesson: %d\ncreated: %s\n---\n\n# %s\n\n",
		c.Title, sec.Title, n, time.Now().Format("2006-01-02"), title)
	if len(sec.Objectives) > 0 {
		b.WriteString("> [!abstract] Objectives\n")
		for _, o := range sec.Objectives {
			fmt.Fprintf(&b, "> - %s\n", o)
		}
		b.WriteString("\n")
	}
	b.WriteString(strings.TrimSpace(stripLeadingHeading(content)))
	fmt.Fprintf(&b, "\n\n---\nBack to [[%s]]\n", strings.TrimSuffix(PlanFile, ".md"))
	return b.String()
}

// stripLeadingHeading drops a "# Title" line the model added despite
// instructions, so the note doesn't show the title twice.
func stripLeadingHeading(content string) string {
	trimmed := strings.TrimSpace(content)
	if first, rest, ok := strings.Cut(trimmed, "\n"); ok && strings.HasPrefix(strings.TrimLeft(first, "#"), " ") && strings.HasPrefix(first, "#") {
		return strings.TrimSpace(rest)
	}
	return trimmed
}

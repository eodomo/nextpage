package learn

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	QuizPlacement  = "placement"
	QuizCheckpoint = "checkpoint"

	TypeMultipleChoice = "multiple_choice"
	TypeShortAnswer    = "short_answer"
)

// Question is what the model writes. Answer is the correct option (letter,
// number or text) for multiple choice, or a reference answer for short
// answer questions.
type Question struct {
	ID          string   `json:"id"`
	Type        string   `json:"type"`
	Prompt      string   `json:"prompt"`
	Options     []string `json:"options,omitempty"`
	Answer      string   `json:"answer"`
	Explanation string   `json:"explanation,omitempty"`
	// Level (1-5) and Topic are used by the adaptive placement quiz.
	Level int    `json:"level,omitempty"`
	Topic string `json:"topic,omitempty"`
}

// PublicQuestion is what the user sees: no answers.
type PublicQuestion struct {
	ID      string   `json:"id"`
	Type    string   `json:"type"`
	Prompt  string   `json:"prompt"`
	Options []string `json:"options,omitempty"`
}

// QuizRequest is the tools.Env.Interact payload for a quiz. Front ends reply
// with one answer string per question, in order (the chosen option's text
// for multiple choice). A nil reply means no user is available.
type QuizRequest struct {
	ID        string           `json:"id"`
	Kind      string           `json:"kind"`
	Title     string           `json:"title"`
	Course    string           `json:"course"`
	Questions []PublicQuestion `json:"questions"`
	// Adaptive quizzes send one question per request; Number counts them.
	// Front ends should offer an "I don't know" choice (reply "").
	Adaptive bool `json:"adaptive,omitempty"`
	Number   int  `json:"number,omitempty"`
}

// DirectoryRequest asks the user where to keep courses. Reply: a path string.
type DirectoryRequest struct {
	Default string `json:"default"`
}

type QuestionResult struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`
	Prompt     string   `json:"prompt"`
	Options    []string `json:"options,omitempty"`
	UserAnswer string   `json:"user_answer"`
	Expected   string   `json:"expected"`
	// Correct is nil until graded (short answers are graded by the model).
	Correct     *bool  `json:"correct"`
	Feedback    string `json:"feedback,omitempty"`
	Explanation string `json:"explanation,omitempty"`
}

type QuizResult struct {
	ID        string           `json:"id"`
	Kind      string           `json:"kind"`
	SectionID string           `json:"section_id,omitempty"`
	Title     string           `json:"title"`
	Time      time.Time        `json:"time"`
	Results   []QuestionResult `json:"results"`
	Score     int              `json:"score"`
	Total     int              `json:"total"`
	Percent   int              `json:"percent"`
	Passed    bool             `json:"passed"`
	Strengths []string         `json:"strengths,omitempty"`
	Gaps      []string         `json:"gaps,omitempty"`
	File      string           `json:"file,omitempty"`
	// Level is the adaptive placement estimate on a 1-5 scale (0 if the
	// quiz was not adaptive). Percent is not meaningful for adaptive quizzes,
	// which aim to keep the learner near 50% correct.
	Level float64 `json:"level,omitempty"`
	// Levels holds each question's difficulty in an adaptive quiz.
	Levels []int `json:"levels,omitempty"`
}

func (q *QuizResult) Ungraded() []QuestionResult {
	var out []QuestionResult
	for _, r := range q.Results {
		if r.Correct == nil {
			out = append(out, r)
		}
	}
	return out
}

func validateQuestions(qs []Question) error {
	if len(qs) < 3 || len(qs) > 12 {
		var got []string
		for _, q := range qs {
			got = append(got, fmt.Sprintf("%q", oneLine(q.Prompt)))
		}
		return fmt.Errorf("a quiz needs 3-12 questions, but only %d could be read (%s); start every question with \"Q:\"", len(qs), strings.Join(got, ", "))
	}
	seen := map[string]bool{}
	for i := range qs {
		q := &qs[i]
		if q.ID == "" {
			q.ID = fmt.Sprintf("q%d", i+1)
		}
		if seen[q.ID] {
			return fmt.Errorf("duplicate question id %q", q.ID)
		}
		seen[q.ID] = true
		if strings.TrimSpace(q.Prompt) == "" {
			return fmt.Errorf("question %s has no prompt", q.ID)
		}
		if q.Type == "" {
			if len(q.Options) > 0 {
				q.Type = TypeMultipleChoice
			} else {
				q.Type = TypeShortAnswer
			}
		}
		switch q.Type {
		case TypeMultipleChoice:
			if len(q.Options) < 2 {
				return fmt.Errorf("multiple choice question %s needs at least 2 options", q.ID)
			}
			if correctOption(q) < 0 {
				return fmt.Errorf("question %s: answer %q does not identify one of its options (use the option letter, e.g. \"B\", or the exact option text)", q.ID, q.Answer)
			}
		case TypeShortAnswer:
			if strings.TrimSpace(q.Answer) == "" {
				return fmt.Errorf("question %s (%q) was read as short answer with no ANSWER line; give each question an \"ANSWER:\" line, and put each option on its own line", q.ID, oneLine(q.Prompt))
			}
		default:
			return fmt.Errorf("question %s has unknown type %q (use multiple_choice or short_answer)", q.ID, q.Type)
		}
	}
	return nil
}

// correctOption resolves a multiple-choice answer given as a letter ("B",
// "b)", "(b)"), a 1-based number, or the option text. Returns -1 if unclear.
func correctOption(q *Question) int {
	a := strings.TrimSpace(q.Answer)
	for i, o := range q.Options {
		if strings.EqualFold(strings.TrimSpace(o), a) {
			return i
		}
	}
	if sa := stripLabel(a); len(sa) > 1 {
		for i, o := range q.Options {
			if strings.EqualFold(stripLabel(o), sa) {
				return i
			}
		}
	}
	// A letter, tolerating decoration small models add: "B;", "**B**",
	// "(b)", "Option B", "B - the text". A bare "A tree..." is not a letter.
	plain := strings.TrimSpace(strings.Trim(a, "*_`\"'"))
	if m := reAnswerLetter.FindStringSubmatch(plain); m != nil {
		if i := int(strings.ToUpper(m[1])[0] - 'A'); i < len(q.Options) {
			return i
		}
	}
	if n, err := strconv.Atoi(strings.Trim(plain, "().:;, ")); err == nil && n >= 1 && n <= len(q.Options) {
		return n - 1
	}
	return -1
}

var reAnswerLetter = regexp.MustCompile(`(?i)^(?:option\s+|choice\s+)?\(?([a-h])\)?\s*(?:$|[.:;,)\-–])`)

// stripLabel removes a leading "A) " / "B. " style label from an option.
func stripLabel(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 2 && (s[1] == ')' || s[1] == '.' || s[1] == ':') && strings.ContainsRune("ABCDEFGHabcdefgh", rune(s[0])) {
		return strings.TrimSpace(s[2:])
	}
	return s
}

// grade scores answers. Multiple choice is graded here; short answers stay
// ungraded for the model.
func grade(quizID, kind, sectionID, title string, qs []Question, answers []string) *QuizResult {
	r := &QuizResult{ID: quizID, Kind: kind, SectionID: sectionID, Title: title, Time: time.Now(), Total: len(qs)}
	for i, q := range qs {
		ans := ""
		if i < len(answers) {
			ans = strings.TrimSpace(answers[i])
		}
		qr := QuestionResult{ID: q.ID, Type: q.Type, Prompt: q.Prompt, Options: q.Options, UserAnswer: ans, Expected: q.Answer, Explanation: q.Explanation}
		if q.Type == TypeMultipleChoice {
			want := correctOption(&q)
			qr.Expected = q.Options[want]
			ok := ans != "" && (ans == q.Options[want] || stripLabel(ans) == stripLabel(q.Options[want]))
			qr.Correct = &ok
		} else if ans == "" {
			f := false
			qr.Correct = &f
			qr.Feedback = "No answer given."
		}
		r.Results = append(r.Results, qr)
	}
	return r
}

// finalize computes the score once every question is graded.
func (q *QuizResult) finalize(passPercent int) {
	q.Score = 0
	var wrong []string
	for _, r := range q.Results {
		if r.Correct != nil && *r.Correct {
			q.Score++
		} else {
			wrong = append(wrong, r.Prompt)
		}
	}
	if q.Total > 0 {
		q.Percent = q.Score * 100 / q.Total
	}
	q.Passed = q.Percent >= passPercent
	if len(q.Gaps) == 0 {
		q.Gaps = wrong
	}
}

// Report describes a graded quiz for the model.
func (q *QuizResult) Report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Quiz %q (%s): %d/%d correct (%d%%).\n", q.Title, q.Kind, q.Score, q.Total, q.Percent)
	for _, r := range q.Results {
		mark := "✗"
		if r.Correct != nil && *r.Correct {
			mark = "✓"
		}
		fmt.Fprintf(&b, "%s [%s] %s\n   user: %s\n   expected: %s\n", mark, r.ID, r.Prompt, orNone(r.UserAnswer), r.Expected)
		if r.Feedback != "" {
			fmt.Fprintf(&b, "   feedback: %s\n", r.Feedback)
		}
	}
	return b.String()
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}

func orNone(s string) string {
	if s == "" {
		return "(no answer)"
	}
	return s
}

// renderQuizNote writes a quiz with answers and feedback as an Obsidian note.
func renderQuizNote(c *Course, q *QuizResult) string {
	var b strings.Builder
	status := "not passed"
	if q.Passed {
		status = "passed"
	}
	if q.Kind == QuizPlacement {
		status = "placement"
	}
	fmt.Fprintf(&b, "---\ntags: [nextpage, quiz]\ncourse: %q\nscore: %d\n---\n\n# %s\n\n**Score:** %d/%d (%d%%) — %s · %s\n",
		c.Title, q.Percent, q.Title, q.Score, q.Total, q.Percent, status, q.Time.Format("2006-01-02 15:04"))
	if q.Level > 0 {
		fmt.Fprintf(&b, "\n**Estimated level:** %.1f / 5 (%s). The quiz adapted to your answers, getting harder after right answers and easier after wrong ones, so about half right is expected.\n", q.Level, LevelLabel(q.Level))
	}
	fmt.Fprintf(&b, "\nBack to [[%s]]\n", strings.TrimSuffix(PlanFile, ".md"))
	for i, r := range q.Results {
		icon := "❌"
		if r.Correct != nil && *r.Correct {
			icon = "✅"
		}
		level := ""
		if q.Level > 0 && i < len(q.Results) {
			level = fmt.Sprintf(" *(level %s)*", levelOf(q, i))
		}
		fmt.Fprintf(&b, "\n## %d. %s %s%s\n\n", i+1, icon, r.Prompt, level)
		for j, o := range r.Options {
			fmt.Fprintf(&b, "%c. %s\n", 'A'+j, o)
		}
		if len(r.Options) > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "**Your answer:** %s\n\n**Expected:** %s\n", orNone(r.UserAnswer), r.Expected)
		if r.Feedback != "" {
			fmt.Fprintf(&b, "\n> [!note] Feedback\n> %s\n", strings.ReplaceAll(r.Feedback, "\n", "\n> "))
		}
		if r.Explanation != "" {
			fmt.Fprintf(&b, "\n> [!info]- Explanation\n> %s\n", strings.ReplaceAll(r.Explanation, "\n", "\n> "))
		}
	}
	if len(q.Strengths) > 0 {
		b.WriteString("\n## Strengths\n")
		for _, s := range q.Strengths {
			fmt.Fprintf(&b, "- %s\n", s)
		}
	}
	if len(q.Gaps) > 0 {
		b.WriteString("\n## To review\n")
		for _, s := range q.Gaps {
			fmt.Fprintf(&b, "- %s\n", s)
		}
	}
	return b.String()
}

// ---- text formats ----
//
// Small local models often fail to emit large nested JSON tool arguments
// (Ollama drops the unparseable call), so the course tools take flat text
// that is parsed here. JSON arrays are still accepted for capable models.

var (
	reQuestion = regexp.MustCompile(`(?i)^\s*(?:\*\*)?(?:Q(?:uestion)?\s*\d*)\s*[:.)]\s*(?:\*\*)?\s*(.*)$`)
	reOption   = regexp.MustCompile(`^\s*(?:[-*]\s*)?\(?([A-Ha-h])[).:]\s+(.*)$`)
	reAnswer   = regexp.MustCompile(`(?i)^\s*(?:\*\*)?(?:correct\s+)?answer\s*(?:\*\*)?\s*:\s*(?:\*\*)?\s*(.*)$`)
	reExplain  = regexp.MustCompile(`(?i)^\s*(?:\*\*)?explanation\s*(?:\*\*)?\s*:\s*(?:\*\*)?\s*(.*)$`)
	reLevel    = regexp.MustCompile(`(?i)^\s*(?:\*\*)?(?:level|difficulty)\s*(?:\*\*)?\s*:\s*(?:\*\*)?\s*(\d)`)
	reTopic    = regexp.MustCompile(`(?i)^\s*(?:\*\*)?(?:topic|subtopic)\s*(?:\*\*)?\s*:\s*(?:\*\*)?\s*(.+)$`)
)

// ParseQuizText parses questions written as:
//
//	Q: What is 2+2?
//	A) 3
//	B) 4
//	ANSWER: B
//	EXPLANATION: optional
//
//	Q: Explain X.
//	ANSWER: model answer
//
// Questions with options are multiple choice; others are short answer.
func ParseQuizText(s string) []Question {
	s = splitInline(s)
	var qs []Question
	var cur *Question
	field := "" // which field continuation lines extend
	flush := func() {
		if cur != nil && strings.TrimSpace(cur.Prompt) != "" {
			cur.Prompt = strings.TrimSpace(cur.Prompt)
			cur.Answer = strings.TrimSpace(cur.Answer)
			cur.Explanation = strings.TrimSpace(cur.Explanation)
			cur.ID = fmt.Sprintf("q%d", len(qs)+1)
			if len(cur.Options) >= 2 {
				cur.Type = TypeMultipleChoice
			} else {
				cur.Type = TypeShortAnswer
				cur.Options = nil
			}
			qs = append(qs, *cur)
		}
		cur = nil
	}
	for _, line := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if m := reQuestion.FindStringSubmatch(line); m != nil {
			flush()
			cur = &Question{Prompt: m[1]}
			field = "prompt"
			continue
		}
		if cur == nil {
			continue
		}
		if m := reAnswer.FindStringSubmatch(line); m != nil {
			cur.Answer = m[1]
			field = "answer"
			continue
		}
		if m := reLevel.FindStringSubmatch(line); m != nil {
			cur.Level = int(m[1][0] - '0')
			field = "level"
			continue
		}
		if m := reTopic.FindStringSubmatch(line); m != nil {
			cur.Topic = strings.TrimSpace(strings.Trim(m[1], "*"))
			field = "topic"
			continue
		}
		if m := reExplain.FindStringSubmatch(line); m != nil {
			cur.Explanation = m[1]
			field = "explanation"
			continue
		}
		if m := reOption.FindStringSubmatch(line); m != nil && (field == "prompt" || field == "option") {
			cur.Options = append(cur.Options, strings.TrimSpace(m[2]))
			field = "option"
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		switch field {
		case "prompt":
			cur.Prompt += "\n" + line
		case "answer":
			cur.Answer += "\n" + line
		case "explanation":
			cur.Explanation += "\n" + line
		case "option":
			cur.Options[len(cur.Options)-1] += " " + strings.TrimSpace(line)
		}
	}
	flush()
	return qs
}

var (
	reInlineQ      = regexp.MustCompile(`([^\n])\s+(Q(?:uestion)?\s*\d*\s*:)`)
	reInlineOpt    = regexp.MustCompile(`([^\n])\s+\(?([A-H])\)\s+`)
	reInlineAnswer = regexp.MustCompile(`(?i)([^\n])\s+((?:correct\s+)?answer\s*:|explanation\s*:|level\s*:|topic\s*:)`)
)

// splitInline puts options and ANSWER/EXPLANATION markers that a model wrote
// on one line ("Q: ... A) x B) y ANSWER: B") onto their own lines.
func splitInline(s string) string {
	s = reInlineQ.ReplaceAllString(s, "$1\n$2")
	s = reInlineOpt.ReplaceAllString(s, "$1\n$2) ")
	s = reInlineAnswer.ReplaceAllString(s, "$1\n$2")
	return s
}

// Questions accepts a JSON array of question objects or a string in the
// ParseQuizText format.
type Questions []Question

func (q *Questions) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*q = ParseQuizText(s)
		return nil
	}
	var arr []Question
	if err := json.Unmarshal(b, &arr); err != nil {
		return err
	}
	*q = arr
	return nil
}

// TextList accepts a JSON array of strings or one string with items
// separated by newlines or semicolons.
type TextList []string

func (t *TextList) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*t = nil
		for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == ';' }) {
			if p := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(part), "-*•")); p != "" {
				*t = append(*t, p)
			}
		}
		return nil
	}
	var arr []string
	if err := json.Unmarshal(b, &arr); err != nil {
		return err
	}
	*t = arr
	return nil
}

// levelOf reports the difficulty of question i in an adaptive quiz.
func levelOf(q *QuizResult, i int) string {
	if i < len(q.Levels) && q.Levels[i] > 0 {
		return fmt.Sprint(q.Levels[i])
	}
	return "?"
}

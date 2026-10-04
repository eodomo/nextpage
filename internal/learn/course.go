// Package learn turns the agent harness into a tutoring platform. It is a
// plugin built only from the harness's extension points: tools (course
// actions), tools.Env.Interact (quizzes), and agent.PromptContext
// (tutor persona plus live course state).
//
// The harness, not the model, owns the workflow. Each course is a state
// machine: placement quiz → plan → (lesson → checkpoint quiz)* per section →
// complete. Tools refuse out-of-order calls, and every turn the system prompt
// states the one next step, which keeps small local models on track.
//
// Course files are written for Obsidian: Markdown notes with LaTeX math and
// mermaid diagrams, linked with [[wikilinks]].
package learn

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const stateFile = ".nextpage-course.json"

type Phase string

const (
	PhasePlacement Phase = "placement"
	PhasePlanning  Phase = "planning"
	PhaseLearning  Phase = "learning"
	PhaseComplete  Phase = "complete"
)

type Section struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Objectives []string `json:"objectives"`
	Passed     bool     `json:"passed"`
	Lessons    []string `json:"lessons"` // file names, relative to the course folder
	// LessonsSinceQuiz counts lessons written since the last checkpoint; a
	// checkpoint needs at least one.
	LessonsSinceQuiz int `json:"lessons_since_quiz"`
	Checkpoints      int `json:"checkpoints"`
}

type Course struct {
	Topic     string        `json:"topic"`
	Title     string        `json:"title"`
	Folder    string        `json:"folder"`
	Created   time.Time     `json:"created"`
	Updated   time.Time     `json:"updated"`
	Phase     Phase         `json:"phase"`
	Mermaid   string        `json:"mermaid,omitempty"`
	Sections  []*Section    `json:"sections,omitempty"`
	Current   int           `json:"current"`
	Placement *QuizResult   `json:"placement,omitempty"`
	Quizzes   []*QuizResult `json:"quizzes,omitempty"`
	// Pending is a quiz with short answers awaiting GradeQuiz.
	Pending *QuizResult `json:"pending,omitempty"`
}

func (c *Course) CurrentSection() *Section {
	if c.Current >= 0 && c.Current < len(c.Sections) {
		return c.Sections[c.Current]
	}
	return nil
}

func (c *Course) section(id string) (int, *Section) {
	for i, s := range c.Sections {
		if s.ID == id {
			return i, s
		}
	}
	return -1, nil
}

// lastResult returns the most recent graded quiz for a section ("" means
// the placement quiz).
func (c *Course) lastResult(sectionID string) *QuizResult {
	for i := len(c.Quizzes) - 1; i >= 0; i-- {
		if c.Quizzes[i].SectionID == sectionID {
			return c.Quizzes[i]
		}
	}
	return nil
}

type Config struct {
	// Dir is the root folder for courses (e.g. inside an Obsidian vault).
	// When empty, the user is asked the first time a course is started.
	Dir string
	// Format is "markdown" (default; Markdown + LaTeX math, renders in
	// Obsidian) or "latex" (standalone .tex lessons).
	Format string
	// PassPercent is the checkpoint pass mark (default 80).
	PassPercent int
	// SaveDir persists a directory chosen interactively.
	SaveDir func(dir string) error
	// OnChange is called after course files change (the web UI refreshes).
	OnChange func()
}

type Manager struct {
	mu     sync.Mutex
	cfg    Config
	active *Course
	// awaitingReply describes content the model must send as its next reply
	// (a lesson or quiz, see tools.Capture); not persisted.
	awaitingReply string
	// reading is set when a lesson was just saved and cleared when the user
	// next sends a message: checkpoints wait until they have had a chance to
	// read it.
	reading bool
	// lastLesson is the most recently written lesson, relative to Dir, so
	// front ends can open it.
	lastLesson string
}

// LastLesson returns the path (relative to Dir) of the newest lesson.
func (m *Manager) LastLesson() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastLesson
}

// UserSpoke is wired to agent.Config.OnPrompt.
func (m *Manager) UserSpoke(string) {
	m.mu.Lock()
	m.reading = false
	m.mu.Unlock()
}

func (m *Manager) awaitingReader() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reading
}

func (m *Manager) setAwaiting(what string) {
	m.mu.Lock()
	m.awaitingReply = what
	m.mu.Unlock()
}

// Awaiting returns what the model's next reply must contain, if anything.
func (m *Manager) Awaiting() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.awaitingReply
}

func NewManager(cfg Config) *Manager {
	if cfg.PassPercent <= 0 {
		cfg.PassPercent = 80
	}
	if cfg.Format == "" {
		cfg.Format = "markdown"
	}
	return &Manager{cfg: cfg}
}

func (m *Manager) Dir() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg.Dir
}

func (m *Manager) SetDir(dir string) error {
	dir = expandHome(strings.TrimSpace(dir))
	if dir == "" {
		return errors.New("no directory given")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return err
	}
	m.mu.Lock()
	m.cfg.Dir = abs
	save := m.cfg.SaveDir
	m.mu.Unlock()
	m.resumeLatest()
	if save != nil {
		return save(abs)
	}
	return nil
}

// resumeLatest activates the most recently used unfinished course, so a
// returning learner can carry on without naming it.
func (m *Manager) resumeLatest() {
	if m.Active() != nil {
		return
	}
	for _, s := range m.List() {
		if s.Phase == PhaseComplete {
			continue
		}
		if c, err := readCourse(filepath.Join(m.Dir(), s.Folder, stateFile)); err == nil {
			m.mu.Lock()
			m.active = c
			m.mu.Unlock()
		}
		return
	}
}

// find returns the summary of an existing course matching name by folder,
// title or topic, ignoring case.
func (m *Manager) find(name string) (Summary, bool) {
	name = strings.TrimSpace(name)
	for _, s := range m.List() {
		if strings.EqualFold(s.Folder, name) || strings.EqualFold(s.Title, name) || strings.EqualFold(s.Topic, name) {
			return s, true
		}
	}
	return Summary{}, false
}

// noActive explains how to get a course going, naming existing ones.
func (m *Manager) noActive() error {
	var names []string
	for _, s := range m.List() {
		names = append(names, fmt.Sprintf("%q", s.Folder))
	}
	if len(names) == 0 {
		return errors.New("no active course: call StartCourse first")
	}
	return fmt.Errorf("no active course. Existing courses: %s. Call OpenCourse to continue one, or StartCourse for a new topic", strings.Join(names, ", "))
}

func (m *Manager) Format() string { return m.cfg.Format }

func (m *Manager) lessonExt() string {
	if m.cfg.Format == "latex" {
		return ".tex"
	}
	return ".md"
}

// Active returns a snapshot of the active course, or nil.
func (m *Manager) Active() *Course {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil {
		return nil
	}
	// Deep copy so callers on other goroutines never share state with the
	// agent's tools.
	data, _ := json.Marshal(m.active)
	var cp Course
	_ = json.Unmarshal(data, &cp)
	return &cp
}

// SetOnChange registers a callback for course file changes.
func (m *Manager) SetOnChange(fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg.OnChange = fn
}

// changed is called with m.mu held or not; it only reads the callback.
func (m *Manager) changed() {
	if fn := m.cfg.OnChange; fn != nil {
		go fn()
	}
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") || p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p[1:], "/"))
		}
	}
	return p
}

var unsafeChars = regexp.MustCompile(`[\\/:*?"<>|#^\[\]]+`)

// SafeName makes s usable as a file name and as an Obsidian link target.
func SafeName(s string) string {
	s = unsafeChars.ReplaceAllString(s, " ")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 80 {
		s = strings.TrimSpace(s[:80])
	}
	if s == "" {
		s = "Untitled"
	}
	return s
}

func (m *Manager) folder(c *Course) string {
	return filepath.Join(m.cfg.Dir, c.Folder)
}

// save writes the course state and regenerates the plan note.
func (m *Manager) save(c *Course) error {
	c.Updated = time.Now()
	dir := m.folder(c)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, stateFile), data, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, PlanFile), []byte(renderPlan(c, m.cfg.PassPercent)), 0o644); err != nil {
		return err
	}
	m.changed()
	return nil
}

const PlanFile = "00 Course Plan.md"

// Create starts a new course and makes it active.
func (m *Manager) Create(topic, title string) (*Course, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cfg.Dir == "" {
		return nil, errors.New("no courses directory configured")
	}
	if title == "" {
		title = topic
	}
	folder := SafeName(title)
	for i := 2; ; i++ {
		if _, err := os.Stat(filepath.Join(m.cfg.Dir, folder)); os.IsNotExist(err) {
			break
		}
		folder = fmt.Sprintf("%s %d", SafeName(title), i)
	}
	c := &Course{Topic: topic, Title: title, Folder: folder, Created: time.Now(), Phase: PhasePlacement}
	if err := m.save(c); err != nil {
		return nil, err
	}
	m.active = c
	m.awaitingReply = ""
	return c, nil
}

type Summary struct {
	Folder   string    `json:"folder"`
	Title    string    `json:"title"`
	Topic    string    `json:"topic"`
	Phase    Phase     `json:"phase"`
	Passed   int       `json:"passed"`
	Sections int       `json:"sections"`
	Updated  time.Time `json:"updated"`
	Active   bool      `json:"active"`
}

// List returns all courses in the courses directory, most recent first.
func (m *Manager) List() []Summary {
	m.mu.Lock()
	dir := m.cfg.Dir
	var activeFolder string
	if m.active != nil {
		activeFolder = m.active.Folder
	}
	m.mu.Unlock()
	if dir == "" {
		return nil
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "*", stateFile))
	var out []Summary
	for _, p := range paths {
		c, err := readCourse(p)
		if err != nil {
			continue
		}
		s := Summary{Folder: c.Folder, Title: c.Title, Topic: c.Topic, Phase: c.Phase, Sections: len(c.Sections), Updated: c.Updated, Active: c.Folder == activeFolder}
		for _, sec := range c.Sections {
			if sec.Passed {
				s.Passed++
			}
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out
}

func readCourse(path string) (*Course, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Course
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	c.Folder = filepath.Base(filepath.Dir(path))
	return &c, nil
}

// Open makes the course in folder (matched by folder or title,
// case-insensitively) the active course.
func (m *Manager) Open(name string) (*Course, error) {
	m.setAwaiting("")
	s, ok := m.find(name)
	if !ok {
		return nil, fmt.Errorf("no course named %q (%v)", name, m.noActive())
	}
	c, err := readCourse(filepath.Join(m.Dir(), s.Folder, stateFile))
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.active = c
	m.mu.Unlock()
	m.changed()
	return c, nil
}

// Delete removes a course folder and everything in it. Only folders that
// hold a course state file are removed, so a bad name can never delete
// anything else in the courses directory.
func (m *Manager) Delete(name string) (string, error) {
	s, ok := m.find(name)
	if !ok {
		return "", fmt.Errorf("no course named %q", name)
	}
	dir := m.Dir()
	path := filepath.Join(dir, s.Folder)
	if rel, err := filepath.Rel(dir, path); err != nil || rel != s.Folder || strings.Contains(rel, string(filepath.Separator)) {
		return "", fmt.Errorf("invalid course folder %q", s.Folder)
	}
	if _, err := os.Stat(filepath.Join(path, stateFile)); err != nil {
		return "", fmt.Errorf("%q is not a course folder", s.Folder)
	}
	if err := os.RemoveAll(path); err != nil {
		return "", err
	}
	m.mu.Lock()
	if m.active != nil && m.active.Folder == s.Folder {
		m.active = nil
		m.awaitingReply = ""
		m.reading = false
	}
	m.mu.Unlock()
	m.changed()
	return s.Folder, nil
}

// Files lists the notes in a course folder (plan first, then lessons in
// order, then quizzes) as paths relative to the courses directory.
func (m *Manager) Files(folder string) []string {
	dir := m.Dir()
	base := filepath.Join(dir, filepath.Base(folder))
	var out []string
	_ = filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		if ext := filepath.Ext(p); ext != ".md" && ext != ".tex" {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	sort.SliceStable(out, func(i, j int) bool {
		qi, qj := strings.Contains(out[i], "/Quizzes/"), strings.Contains(out[j], "/Quizzes/")
		if qi != qj {
			return qj
		}
		return out[i] < out[j]
	})
	return out
}

// ReadFile returns a course file by its path relative to the courses
// directory, refusing anything outside it.
func (m *Manager) ReadFile(rel string) ([]byte, error) {
	dir := m.Dir()
	if dir == "" {
		return nil, errors.New("no courses directory")
	}
	p := filepath.Join(dir, filepath.FromSlash(rel))
	r, err := filepath.Rel(dir, p)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) || filepath.Base(p) == stateFile {
		return nil, errors.New("invalid path")
	}
	return os.ReadFile(p)
}

// update applies fn to the active course and saves it.
func (m *Manager) update(fn func(c *Course) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil {
		m.mu.Unlock()
		err := m.noActive()
		m.mu.Lock()
		return err
	}
	if err := fn(m.active); err != nil {
		return err
	}
	return m.save(m.active)
}

func (m *Manager) writeNote(c *Course, name, content string) (string, error) {
	p := filepath.Join(m.folder(c), name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		return "", err
	}
	return p, nil
}

// renderPlan produces the course overview note, including the mermaid path
// with completed and current sections highlighted.
func renderPlan(c *Course, pass int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "---\ntags: [nextpage, course]\ntopic: %q\nphase: %s\n---\n\n# %s\n\n", c.Topic, c.Phase, c.Title)
	fmt.Fprintf(&b, "**Topic:** %s  \n**Status:** %s", c.Topic, phaseLabel(c))
	if p := c.Placement; p != nil && p.Level > 0 {
		fmt.Fprintf(&b, "  \n**Starting level:** %.1f / 5, %s ([[%s]])", p.Level, LevelLabel(p.Level), strings.TrimSuffix(p.File, ".md"))
	} else if p != nil {
		fmt.Fprintf(&b, "  \n**Placement quiz:** %d%% ([[%s]])", p.Percent, strings.TrimSuffix(p.File, ".md"))
	}
	b.WriteString("\n")

	if c.Mermaid != "" {
		b.WriteString("\n## Learning path\n\n```mermaid\n")
		b.WriteString(drawMermaid(c.Mermaid, c.Sections))
		var done []string
		for _, s := range c.Sections {
			if s.Passed {
				done = append(done, s.ID)
			}
		}
		b.WriteString("\n    classDef done fill:#1b5e20,stroke:#66bb6a,color:#fff\n    classDef current fill:#b71c1c,stroke:#ff5252,color:#fff\n")
		if len(done) > 0 {
			fmt.Fprintf(&b, "    class %s done\n", strings.Join(done, ","))
		}
		if s := c.CurrentSection(); s != nil && c.Phase == PhaseLearning {
			fmt.Fprintf(&b, "    class %s current\n", s.ID)
		}
		b.WriteString("```\n")
	}

	if len(c.Sections) > 0 {
		b.WriteString("\n## Sections\n")
		for i, s := range c.Sections {
			mark := " "
			if s.Passed {
				mark = "x"
			}
			fmt.Fprintf(&b, "\n- [%s] **%d. %s**", mark, i+1, s.Title)
			if i == c.Current && c.Phase == PhaseLearning {
				b.WriteString(" ← current")
			}
			for _, o := range s.Objectives {
				fmt.Fprintf(&b, "\n    - %s", o)
			}
			for _, l := range s.Lessons {
				fmt.Fprintf(&b, "\n    - 📖 [[%s]]", strings.TrimSuffix(strings.TrimSuffix(l, ".md"), ".tex"))
			}
			for _, q := range c.Quizzes {
				if q.SectionID == s.ID {
					fmt.Fprintf(&b, "\n    - 📝 [[%s]] — %d%%", strings.TrimSuffix(q.File, ".md"), q.Percent)
				}
			}
		}
		fmt.Fprintf(&b, "\n\nCheckpoint pass mark: %d%%\n", pass)
	}
	return b.String()
}

func phaseLabel(c *Course) string {
	switch c.Phase {
	case PhasePlacement:
		return "placement quiz"
	case PhasePlanning:
		return "planning"
	case PhaseLearning:
		passed := 0
		for _, s := range c.Sections {
			if s.Passed {
				passed++
			}
		}
		return fmt.Sprintf("learning (%d/%d sections passed)", passed, len(c.Sections))
	case PhaseComplete:
		return "complete 🎉"
	}
	return string(c.Phase)
}

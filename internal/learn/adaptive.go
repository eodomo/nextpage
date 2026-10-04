package learn

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/eodomo/nextpage/internal/tools"
)

// The placement quiz is a computerized adaptive test. The learner's ability
// is a number on the 1-5 difficulty scale. Each answer nudges it towards the
// question's level (up when right, down when wrong), by a step that shrinks
// as evidence accumulates, so the questions home in on the learner's level
// like a binary search. The quiz ends when the estimate plateaus.
//
// The model only writes questions: a few at a time for a requested level,
// via tools.Env.Complete. They are generated in the background while the
// learner answers, so the next question is usually ready immediately.
const (
	adaptiveStart   = 2.5 // initial ability estimate
	adaptiveMin     = 6   // never stop before this many answers
	adaptiveMax     = 15  // always stop after this many
	plateauWindow   = 4   // estimates compared for the plateau test
	plateauRange    = 0.5 // max spread of those estimates to call it settled
	perGeneration   = 2   // questions requested per model call
	generateRetries = 2   // failed generations per level before giving up on it
)

// exactLevelWait is how long to wait for questions at the target level
// before using a neighbouring level instead.
var exactLevelWait = 20 * time.Second

// estimator tracks ability with a logistic (Rasch-style) model:
// P(correct) = 1 / (1 + e^(-1.5(θ - level))), updated with a step of
// 1.5/√n so early answers move the estimate a lot and later ones little.
type estimator struct {
	theta   float64
	n       int
	history []float64
}

func (e *estimator) update(level int, correct bool) {
	e.n++
	p := 1 / (1 + math.Exp(-1.5*(e.theta-float64(level))))
	outcome := 0.0
	if correct {
		outcome = 1
	}
	e.theta += 1.5 / math.Sqrt(float64(e.n)) * (outcome - p)
	e.theta = math.Max(0.5, math.Min(5.5, e.theta))
	e.history = append(e.history, e.theta)
}

// target is the difficulty to ask next.
func (e *estimator) target() int {
	return int(math.Max(1, math.Min(5, math.Round(e.theta))))
}

// settled reports whether the estimate has plateaued.
func (e *estimator) settled() bool {
	if e.n < adaptiveMin {
		return false
	}
	if e.n >= adaptiveMax {
		return true
	}
	recent := e.history[len(e.history)-plateauWindow:]
	lo, hi := recent[0], recent[0]
	for _, v := range recent {
		lo, hi = math.Min(lo, v), math.Max(hi, v)
	}
	return hi-lo <= plateauRange
}

// LevelLabel names a point on the 1-5 scale.
func LevelLabel(l float64) string {
	switch {
	case l < 1.5:
		return "beginner"
	case l < 2.5:
		return "novice"
	case l < 3.5:
		return "intermediate"
	case l < 4.5:
		return "advanced"
	}
	return "expert"
}

// questionBank holds generated questions and fills itself in the background.
type questionBank struct {
	topic    string
	complete func(ctx context.Context, system, prompt string) (string, error)

	mu       sync.Mutex
	items    []*Question
	seen     map[string]bool // normalized prompts, to avoid repeats
	topics   []string        // subtopics covered so far
	calls    int             // generation requests, to rotate the angle
	queued   map[int]bool    // levels waiting for or being generated
	failures map[int]int
	changed  chan struct{} // closed and replaced whenever the bank changes
	work     chan int
}

func newBank(ctx context.Context, topic string, complete func(context.Context, string, string) (string, error)) *questionBank {
	b := &questionBank{
		topic:    topic,
		complete: complete,
		seen:     map[string]bool{},
		queued:   map[int]bool{},
		failures: map[int]int{},
		changed:  make(chan struct{}),
		work:     make(chan int, 16),
	}
	go b.worker(ctx)
	return b
}

var questionAngles = []string{
	"key definitions and what things are",
	"how a process works, step by step",
	"comparing two related ideas",
	"applying the idea to a concrete example or scenario",
	"common misconceptions",
	"cause and effect: what happens if something changes",
	"why it works the way it does",
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

func normalize(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// add stores usable multiple-choice questions, assigning level if missing.
func (b *questionBank) add(qs []Question, level int) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	added := 0
	for i := range qs {
		q := qs[i]
		if len(q.Options) < 2 || correctOption(&q) < 0 || b.seen[normalize(q.Prompt)] {
			continue
		}
		if level > 0 {
			q.Level = level
		} else if q.Level < 1 || q.Level > 5 {
			q.Level = 3
		}
		q.Type = TypeMultipleChoice
		b.seen[normalize(q.Prompt)] = true
		if q.Topic != "" && !containsFold(b.topics, q.Topic) {
			b.topics = append(b.topics, q.Topic)
		}
		b.items = append(b.items, &q)
		added++
	}
	b.notify()
	return added
}

func (b *questionBank) notify() {
	close(b.changed)
	b.changed = make(chan struct{})
}

// want queues generation for level unless questions for it exist or are
// already on the way.
func (b *questionBank) want(level int) {
	if level < 1 || level > 5 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.queued[level] || b.failures[level] >= generateRetries {
		return
	}
	for _, q := range b.items {
		if q.Level == level {
			return
		}
	}
	b.queued[level] = true
	select {
	case b.work <- level:
	default:
		b.queued[level] = false
	}
}

// anyQueued reports whether generation is pending; callers hold b.mu.
func (b *questionBank) anyQueued() bool {
	for _, q := range b.queued {
		if q {
			return true
		}
	}
	return false
}

func (b *questionBank) canGenerate(level int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.failures[level] < generateRetries
}

func (b *questionBank) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case level := <-b.work:
			n, err := b.generate(ctx, level)
			b.mu.Lock()
			b.queued[level] = false
			if err != nil || n == 0 {
				b.failures[level]++
				log.Printf("learn: generating level %d questions: added %d, err %v", level, n, err)
			}
			b.notify()
			b.mu.Unlock()
		}
	}
}

func (b *questionBank) generate(ctx context.Context, level int) (int, error) {
	// Small models copy example text, so past questions are never shown;
	// only the subtopics already used, plus a rotating angle for variety.
	b.mu.Lock()
	covered := strings.Join(b.topics, ", ")
	angle := questionAngles[b.calls%len(questionAngles)]
	b.calls++
	b.mu.Unlock()
	prompt := fmt.Sprintf(`Write %d multiple-choice questions about %q at difficulty level %d of 5.
Levels: 1 = complete beginner (basic terms), 2 = novice, 3 = intermediate (applies the core ideas), 4 = advanced, 5 = expert (subtle edge cases, deep theory).
Each question has 4 options and exactly one correct answer. Test understanding rather than trivia. Angle for these questions: %s.`, perGeneration, b.topic, level, angle)
	if covered != "" {
		prompt += "\nThese subtopics are already covered, so choose different ones: " + covered + "."
	}
	prompt += `

Reply with the questions only, in exactly this format, with a blank line between questions:
Q: <question>
A) <option>
B) <option>
C) <option>
D) <option>
ANSWER: <letter>
TOPIC: <2-4 word subtopic>`
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	text, err := b.complete(ctx, "You write placement-test questions. Reply with the questions only, in the exact format requested.", prompt)
	if err != nil {
		return 0, err
	}
	parsed := ParseQuizText(text)
	n := b.add(parsed, level)
	if n == 0 {
		log.Printf("learn: level %d generation unusable (%d parsed); raw output: %.1500q", level, len(parsed), text)
	}
	return n, nil
}

// next removes and returns the unused question closest to level, waiting
// for generation when nothing suitable is ready. Questions within one level
// of the target are used rather than waiting.
func (b *questionBank) next(ctx context.Context, level int, topicCount map[string]int) (*Question, error) {
	b.want(level)
	// Prefer the exact level: wait a little for questions being generated
	// for it before settling for a neighbouring level.
	patience := time.After(exactLevelWait)
	impatient := false
	for {
		b.mu.Lock()
		best, bestDist := -1, 99
		for i, q := range b.items {
			d := abs(q.Level - level)
			if d < bestDist || best >= 0 && d == bestDist && topicCount[q.Topic] < topicCount[b.items[best].Topic] {
				best, bestDist = i, d
			}
		}
		exhausted := b.failures[level] >= generateRetries && !b.queued[level]
		acceptable := bestDist == 0 || bestDist == 1 && (impatient || !b.queued[level]) || exhausted || impatient && !b.anyQueued()
		if best >= 0 && acceptable {
			q := b.items[best]
			b.items = append(b.items[:best], b.items[best+1:]...)
			b.mu.Unlock()
			return q, nil
		}
		// Nothing usable yet. If this level can't be generated, try the
		// nearest levels that still can; if none can, give up.
		pending := b.anyQueued()
		changed := b.changed
		b.mu.Unlock()
		if !pending {
			asked := false
			for _, l := range []int{level, level + 1, level - 1, level + 2, level - 2, level + 3, level - 3, level + 4, level - 4} {
				if l >= 1 && l <= 5 && b.canGenerate(l) {
					b.want(l)
					asked = true
					break
				}
			}
			if !asked && best < 0 {
				return nil, errors.New("the model could not write usable quiz questions")
			}
			if !asked {
				impatient = true // take the closest question we have
				continue
			}
		}
		select {
		case <-changed:
		case <-patience:
			impatient = true
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// runAdaptive gives the placement quiz one question at a time.
func (t *giveQuiz) runAdaptive(ctx context.Context, env *tools.Env, c *Course, title string, seed []Question) tools.Result {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // stops background generation
	bank := newBank(ctx, c.Topic, env.Complete)
	bank.add(seed, 0)
	for _, l := range []int{3, 2, 4} {
		bank.want(l)
	}

	est := &estimator{theta: adaptiveStart}
	var asked []Question
	var answers []string
	topicCount := map[string]int{}
	for !est.settled() {
		q, err := bank.next(ctx, est.target(), topicCount)
		if err != nil {
			if len(asked) >= 3 && !errors.Is(err, context.Canceled) {
				break // finish with what we have
			}
			return fail(err)
		}
		reply, err := env.Interact(ctx, QuizRequest{
			ID: "placement", Kind: QuizPlacement, Title: title, Course: c.Title,
			Adaptive: true, Number: len(asked) + 1,
			Questions: []PublicQuestion{{ID: fmt.Sprintf("q%d", len(asked)+1), Type: q.Type, Prompt: q.Prompt, Options: q.Options}},
		})
		if err != nil {
			return fail(err)
		}
		got := toStrings(reply)
		if got == nil {
			return fail(errors.New("the quiz could not be shown to the user (no interactive front end); ask them to use the TUI or web app"))
		}
		ans := ""
		if len(got) > 0 {
			ans = strings.TrimSpace(got[0])
		}
		want := q.Options[correctOption(q)]
		correct := ans != "" && (ans == want || stripLabel(ans) == stripLabel(want))
		est.update(q.Level, correct)
		q.ID = fmt.Sprintf("q%d", len(asked)+1)
		asked = append(asked, *q)
		answers = append(answers, ans)
		topicCount[q.Topic]++
		// Get questions ready for wherever the next answer might lead.
		for _, l := range []int{est.target(), est.target() + 1, est.target() - 1} {
			bank.want(l)
		}
	}

	result := grade("placement", QuizPlacement, "", title, asked, answers)
	result.Level = math.Round(math.Max(1, math.Min(5, est.theta))*10) / 10
	for _, q := range asked {
		result.Levels = append(result.Levels, q.Level)
	}
	result.Strengths, result.Gaps = topicSummary(asked, result.Results)
	var msg string
	err := t.m.update(func(c *Course) error {
		var err error
		msg, err = t.m.apply(c, result)
		return err
	})
	if err != nil {
		return fail(err)
	}
	return tools.Result{Output: msg, Display: quizDisplay(result)}
}

// topicSummary groups answers by subtopic: topics answered entirely right
// are strengths, any miss is a gap (with the level it was asked at).
func topicSummary(qs []Question, rs []QuestionResult) (strengths, gaps []string) {
	type stat struct{ right, total, missLevel int }
	stats := map[string]*stat{}
	var order []string
	for i, q := range qs {
		topic := q.Topic
		if topic == "" {
			topic = oneLine(q.Prompt)
		}
		s, ok := stats[topic]
		if !ok {
			s = &stat{}
			stats[topic] = s
			order = append(order, topic)
		}
		s.total++
		if rs[i].Correct != nil && *rs[i].Correct {
			s.right++
		} else if s.missLevel == 0 || q.Level < s.missLevel {
			s.missLevel = q.Level
		}
	}
	for _, topic := range order {
		s := stats[topic]
		if s.right == s.total {
			strengths = append(strengths, topic)
		} else {
			gaps = append(gaps, fmt.Sprintf("%s (missed at level %d)", topic, s.missLevel))
		}
	}
	return strengths, gaps
}

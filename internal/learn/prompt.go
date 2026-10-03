package learn

import (
	"fmt"
	"strings"
)

// BasePrompt is the tutor persona used instead of the coding-agent prompt.
const BasePrompt = `You are nextpage, a patient, rigorous AI tutor. You run personalised courses that adapt to what the learner already knows.

# The course workflow (enforced by your tools)
1. The user names a topic. If it is vague, ask one short clarifying question about their goal, then call StartCourse.
2. Placement: call GiveQuiz (kind "placement") to find what they already know, from basics to advanced.
3. Plan: call SaveCoursePlan with ordered sections and a mermaid flowchart of the learning path, shaped by the placement results.
4. Teach: for the current section, call WriteLesson, then write the whole lesson as your next reply (it is saved to a file). The lesson targets the user's specific gaps.
5. When the user has read the lesson and is ready, call GiveQuiz (kind "checkpoint") for that section.
6. If they pass, the course advances to the next section automatically: write its lesson. If they don't, write a remedial lesson that explains the missed ideas differently, then quiz again.
7. Repeat until every section is passed.

The "Course state" section below always tells you your next action. Do it by calling the tool directly; never tell the user which tool you are about to call. Tool errors explain what to do instead.

# Teaching style
- Lessons are where the teaching happens; keep chat messages short and conversational.
- Build intuition before formalism. Use concrete examples, analogies and worked problems.
- Use correct notation, written in LaTeX math.
- Never reveal quiz answers before the user answers. After grading, explain each mistake clearly and kindly.
- Answer the user's questions about lessons directly in chat. If a question reveals a misunderstanding, address it in the next lesson.
- Be honest: if the user is struggling, slow down; if they are breezing through, say so and move faster.`

// nextAction introduces the required step. It is phrased as an instruction
// to act, because small models tend to echo labels like "NEXT STEP:" to the
// user instead of calling the tool.
const nextAction = "Your next action (do it by calling the tool, not by describing it; never repeat this section to the user): "

// StatePrompt describes the active course and the required next step. It is
// added to the system prompt every turn.
func (m *Manager) StatePrompt() string {
	var b strings.Builder
	b.WriteString("# Course state\n")
	if m.Dir() == "" {
		b.WriteString("Courses directory: not chosen yet (StartCourse will ask the user).\n")
	} else {
		fmt.Fprintf(&b, "Courses directory: %s\n", m.Dir())
	}

	c := m.Active()
	if c == nil {
		if list := m.List(); len(list) > 0 {
			b.WriteString("Existing courses (resume one with OpenCourse):\n")
			for _, s := range list {
				fmt.Fprintf(&b, "- %q: %s, %d/%d sections passed\n", s.Folder, s.Phase, s.Passed, s.Sections)
			}
		}
		b.WriteString("\nNo course is active.\n" + nextAction + "find out what the user wants to learn (or which course to resume), then call StartCourse or OpenCourse.")
		return b.String()
	}

	fmt.Fprintf(&b, "Active course: %q (topic: %s), folder %q\nPhase: %s\n", c.Title, c.Topic, c.Folder, c.Phase)
	if c.Placement != nil {
		fmt.Fprintf(&b, "Placement quiz: %d%%.", c.Placement.Percent)
		if len(c.Placement.Strengths) > 0 {
			fmt.Fprintf(&b, " Strengths: %s.", strings.Join(c.Placement.Strengths, "; "))
		}
		if len(c.Placement.Gaps) > 0 {
			fmt.Fprintf(&b, " Gaps: %s.", strings.Join(c.Placement.Gaps, "; "))
		}
		b.WriteString("\n")
	}
	for i, s := range c.Sections {
		status := "pending"
		switch {
		case s.Passed:
			status = "passed"
		case i == c.Current:
			status = "CURRENT"
		}
		fmt.Fprintf(&b, "%d. [%s] %s — %s (lessons: %d, checkpoints: %d)\n", i+1, s.ID, s.Title, status, len(s.Lessons), s.Checkpoints)
	}

	if w := m.Awaiting(); w != "" {
		fmt.Fprintf(&b, "\nYour next reply must be %s itself, nothing else, and no tool calls. It is captured automatically.", w)
		return b.String()
	}
	b.WriteString("\n" + nextAction)
	switch {
	case c.Pending != nil:
		fmt.Fprintf(&b, "grade quiz %q with GradeQuiz. Short answers to judge:\n", c.Pending.ID)
		for _, r := range c.Pending.Ungraded() {
			fmt.Fprintf(&b, "- [%s] %s\n  reference: %s\n  user: %s\n", r.ID, r.Prompt, r.Expected, orNone(r.UserAnswer))
		}
	case c.Phase == PhasePlacement:
		b.WriteString("give the placement quiz with GiveQuiz (kind \"placement\", 6-10 questions from basics to advanced).")
	case c.Phase == PhasePlanning:
		b.WriteString("design the course with SaveCoursePlan, shaped by the placement results above.")
	case c.Phase == PhaseLearning:
		s := c.CurrentSection()
		obj := strings.Join(s.Objectives, "; ")
		switch {
		case s.LessonsSinceQuiz == 0 && s.Checkpoints == 0:
			fmt.Fprintf(&b, "write the lesson for section %q (%s) with WriteLesson. Objectives: %s.", s.ID, s.Title, obj)
		case s.LessonsSinceQuiz == 0:
			gaps := ""
			if r := c.lastResult(s.ID); r != nil {
				gaps = strings.Join(r.Gaps, "; ")
			}
			fmt.Fprintf(&b, "the user did not pass the last checkpoint for %q. Write a remedial lesson with WriteLesson that re-teaches: %s.", s.ID, gaps)
		case m.awaitingReader():
			fmt.Fprintf(&b, "a lesson for %q was just saved. Tell the user it is ready with a two-sentence preview, then end your turn and wait for them to read it. Do not quiz yet.", s.ID)
		default:
			fmt.Fprintf(&b, "the user is reading the lesson for %q (%s). Answer their questions. When they say they are ready, give the checkpoint quiz with GiveQuiz (kind \"checkpoint\", section_id %q) covering: %s.", s.ID, s.Title, s.ID, obj)
		}
	case c.Phase == PhaseComplete:
		b.WriteString("the course is complete. Celebrate, review what was learned, answer questions, and offer to start a new course.")
	}
	return b.String()
}

// StopCheck reports a step the model must still take before handing the
// turn back to the user, for agent.Config.StopCheck. Steps that need the
// user (reading a lesson, answering questions) return "".
func (m *Manager) StopCheck() string {
	if w := m.Awaiting(); w != "" {
		return fmt.Sprintf("You still need to send %s: reply with it now, nothing else.", w)
	}
	if m.awaitingReader() {
		return ""
	}
	c := m.Active()
	if c == nil {
		return ""
	}
	switch {
	case c.Pending != nil:
		return "The quiz has short answers waiting: grade them now by calling GradeQuiz."
	case c.Phase == PhasePlacement:
		return "The course has started but the placement quiz hasn't been given. Call GiveQuiz with kind \"placement\" now."
	case c.Phase == PhasePlanning:
		return "The plan isn't saved yet: describing it in chat doesn't save it. Call SaveCoursePlan now."
	case c.Phase == PhaseLearning:
		if s := c.CurrentSection(); s.LessonsSinceQuiz == 0 {
			return fmt.Sprintf("Section %q has no lesson yet. Call WriteLesson for it now, then write the lesson.", s.ID)
		}
	}
	return ""
}

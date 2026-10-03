# nextpage

An AI-driven learning platform built on a Claude Code–style agent harness. Tell it what you want to learn and it runs a personal course:

1. **Placement quiz.** A graded quiz finds out what you already know.
2. **Course plan.** Sections ordered around your gaps, with a mermaid diagram of your learning path.
3. **Lessons.** Markdown notes with LaTeX math, written for Obsidian and aimed at what your quizzes showed you're missing.
4. **Checkpoints.** A quiz after each section. Pass to move on; otherwise you get a remedial lesson and another try.
5. Repeat until every section on the path is passed.

It runs in the terminal or as a password-protected web app, against any tool-capable model on an Ollama server. The same harness also works as a general coding agent (`--profile code`).

```sh
cp example.env .env        # set OLLAMASERVER, MODEL, NEXTPAGE_COURSES_DIR, ...
./run.sh                   # terminal tutor
./run.sh --web             # web app on :8080 (needs NEXTPAGE_PASSWORD)
docker compose up -d --build   # web app behind Traefik (see below)
```

## Your course in Obsidian

Point `NEXTPAGE_COURSES_DIR` (or `--courses-dir`, or `"coursesDir"` in settings) at a folder in your vault. If it isn't set, nextpage asks the first time you start a course and remembers the answer. Each course gets a folder:

```
Linear Algebra/
  00 Course Plan.md               ← overview, mermaid path (done/current highlighted), progress, links
  01 Vectors - Lesson 1.md
  01 Vectors - Lesson 2 (review).md   ← remedial lesson after a failed checkpoint
  02 Matrices - Lesson 1.md
  Quizzes/Placement Quiz.md       ← your answers, the correct ones, feedback
  Quizzes/01 Vectors - Checkpoint 1.md
  .nextpage-course.json           ← course state (hidden in Obsidian)
```

Notes use `$…$` / `$$…$$` math, ```` ```mermaid ```` diagrams, `[[wikilinks]]`, callouts and frontmatter tags, all of which Obsidian renders natively. Set `"lessonFormat": "latex"` to get standalone `.tex` lessons instead.

Courses resume automatically: the most recent unfinished course is active on startup, and `/courses` lists them all.

## Web app

`--web` serves a single-user app: a password login (`NEXTPAGE_PASSWORD`), a course and lesson browser, a lesson reader (LaTeX via KaTeX, mermaid, Obsidian callouts and wikilinks), a chat with the tutor, and quizzes in a focused overlay. **Settings** sets the Ollama server URL, the basic-auth username and password, and the model. Settings are saved to `~/.nextpage/connection.json` (mode 0600), override the env vars, and apply immediately. The password is never sent back to the browser.

### Docker + Traefik

`compose.yml` builds the image and routes it through Traefik with TLS from the `myresolver` certificate resolver. Set these in `.env`:

| Variable | Meaning |
| --- | --- |
| `NEXTPAGE_PASSWORD` | Web login password (required) |
| `WEB_URL` | Hostname Traefik routes to the app, e.g. `learn.example.com` (required) |
| `COURSES_PATH` | Host folder (e.g. in your vault) mounted as the courses directory (required) |
| `OLLAMASERVER`, `MODEL`, `OLLAMAUSER`, `OLLAMAPASS` | Optional; can be set in the app's Settings instead |
| `TRAEFIK_NETWORK` | External Traefik network (default `traefik`) |
| `TRAEFIK_ENTRYPOINT` | HTTPS entrypoint (default `websecure`) |
| `PUID` / `PGID` | User the container runs as, so notes are owned by you (default 1000) |

Settings, sessions and logs live in the `nextpage_data` volume (`/data`).

## Choosing a model

The workflow runs on tool calls, so the model must actually call tools.

- **qwen3:1.7b** works end to end and is fast on CPU. Lesson and quiz quality is basic.
- **gemma4:e4b** and **deepseek-r1:8b/14b** produce better content but are slower.
- **deepseek-r1:1.5b** advertises tool support but never calls tools: it writes quizzes as chat text, so courses can't progress. Avoid it.

The tutor turns model "thinking" off by default because it's slow on CPU. Enable it with `--think` or `"think": true`. `"maxTokens"` (default 4096) caps each reply so a model stuck in a loop can't run for hours.

## Configuration

Settings are merged from `~/.nextpage/settings.json`, then `.nextpage/settings.json`, then `.nextpage/settings.local.json`, with MCP servers also read from `.mcp.json`. Env vars override settings, and flags override both. Learning-specific keys: `profile` (`learn` | `code`), `coursesDir`, `lessonFormat` (`markdown` | `latex`), `passPercent` (default 80). General keys: `server`, `model`, `numCtx`, `maxTokens`, `think`, `permissions`, `hooks`, `mcpServers`, `env`.

## The harness

Everything above is a plugin (`internal/learn`) on a general agent harness modeled on Claude Code. The plugin uses only the harness's extension points:

- **Tools** (`tools.Tool`): StartCourse, OpenCourse, CourseStatus, GiveQuiz, GradeQuiz, SaveCoursePlan, WriteLesson.
- **Interactions** (`tools.Env.Interact`): plugin-defined requests such as a quiz, which each front end renders its own way.
- **Reply capture** (`tools.Env.CaptureReply`): lets a tool take long content (a lesson or quiz) as the model's next plain-text reply. Small models write that far more reliably than a big JSON argument. Captures can be hidden, which keeps quiz answers off screen.
- **Prompt sections** (`agent.PromptContext.Base/Sections`): the tutor persona, plus live course state with the one next step.
- **StopCheck / OnPrompt**: keep the model moving through steps that don't need the user, and know when the user is back. For example, a checkpoint waits until you've had a chance to read the lesson.

The harness itself also hardens weak models: it retries empty replies, nudges a model that describes a tool call instead of making it, stops repeated identical failures, and caps output length.

In the code profile (`--profile code`) the same harness is a coding agent. It provides Read/Write/Edit/Bash/Glob/Grep/WebFetch/TodoWrite/Task/Skill tools, permission modes and rules, hooks, MCP, memory files (NEXTPAGE.md/AGENTS.md/CLAUDE.md), resumable sessions, compaction, custom slash commands, sub-agents, skills, and print mode (`-p`).

## Shortcuts (terminal)

| Key | Action |
| --- | --- |
| esc | Interrupt |
| shift+tab | Cycle permission mode |
| ctrl+o | Verbose (full tool output, thinking) |
| ctrl+t | Toggle the todo list |
| shift+enter or ctrl+j | New line |
| mouse wheel, pgup/pgdown | Scroll |
| ctrl+c ×2 | Exit |

`/help` lists all commands, including `/courses`, `/model`, `/resume`, `/compact` and `/think`.

## Developing

`go test ./...` runs the unit tests, including a full simulated course in `internal/learn`. CLAUDE.md describes the architecture.

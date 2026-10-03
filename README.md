# nextpage

A terminal coding agent in the style of Claude Code, driving any tool-capable model on an Ollama server.

```sh
cp example.env .env   # set OLLAMASERVER, MODEL (and OLLAMAUSER/OLLAMAPASS if behind basic auth)
./run.sh              # interactive TUI
./run.sh -p "explain main.go"          # print mode: answer and exit
git diff | ./run.sh -p "review this"   # stdin is appended to the prompt
./run.sh -c           # continue the last conversation in this project
```

## Features

| Area | What's there |
| --- | --- |
| Agent loop | Streaming responses and thinking, multi-step tool calling, max-turns limit, interrupt with esc, messages typed while it runs are queued |
| Tools | Read, Write, Edit (diff previews, must read before writing), Bash (persistent cwd, timeouts, background shells via BashOutput/KillShell), Glob, Grep, WebFetch, TodoWrite, Task (sub-agents), Skill, AskUserQuestion, ExitPlanMode, MCP tools |
| Permissions | Modes default → acceptEdits → plan (shift+tab), `--dangerously-skip-permissions`. Allow/ask/deny rules in settings or `--allowedTools`/`--disallowedTools`. Per-call dialog with "don't ask again" for the session or the project. Read-only commands such as `git status` and `ls` run without asking |
| Memory | `NEXTPAGE.md`, `AGENTS.md`, `CLAUDE.md` from cwd up to `/`, `~/.nextpage/NEXTPAGE.md`, `NEXTPAGE.local.md`, `@path` imports. Typing `#note` appends to project memory |
| Sessions | JSONL transcripts, `/resume`, `-c`, `-r <id>`, `/compact` plus auto-compact near the context limit, `/export` |
| Extensions | Hooks (PreToolUse, PostToolUse, UserPromptSubmit, Stop, SubagentStop, SessionStart, SessionEnd, PreCompact, Notification), MCP stdio servers, custom slash commands, sub-agent definitions, skills |
| Input | `/` commands with completion, `!cmd` shell passthrough, `@file` attachments (tab completes), input history, multi-line input |

## Configuration

Settings are merged from `~/.nextpage/settings.json`, then `.nextpage/settings.json`, then `.nextpage/settings.local.json`, with MCP servers also read from `.mcp.json`. Env vars override settings, and flags override both.

```json
{
  "model": "qwen3:14b",
  "server": "http://127.0.0.1:11434",
  "numCtx": 32768,
  "think": false,
  "autoCompact": true,
  "permissions": {
    "allow": ["Bash(go test:*)", "Bash(go build:*)", "Edit(internal/**)"],
    "deny": ["Read(.env)"],
    "defaultMode": "default"
  },
  "hooks": {
    "PostToolUse": [{ "matcher": "Edit|Write", "hooks": [{ "type": "command", "command": "gofmt -l . >&2" }] }]
  },
  "mcpServers": {
    "github": { "command": "github-mcp-server", "args": ["stdio"], "env": { "GITHUB_TOKEN": "${GITHUB_TOKEN}" } }
  }
}
```

The context window (`numCtx`) defaults to 32768 tokens, which Ollama allocates per request. Lower it on memory-constrained or CPU-only servers.

### Extensions

Each of these is looked up in both `~/.nextpage/` and `<project>/.nextpage/`; project definitions win.

- `commands/<name>.md`: slash command `/name`. Frontmatter supports `description`, `argument-hint` and `model`. The body supports `$ARGUMENTS`, `$1`–`$9` and inline shell via ``!`cmd` ``.
- `agents/<name>.md`: sub-agent type for the Task tool. Frontmatter supports `name`, `description`, `tools: Read, Grep` and `model`; the body is the system prompt.
- `skills/<name>/SKILL.md`: a skill listed to the model and loaded on demand. Frontmatter supports `name` and `description`. Each skill can also be run as `/name`.

## Shortcuts

Type `/help` in the app for the full list.

| Key | Action |
| --- | --- |
| esc | Interrupt |
| shift+tab | Cycle permission mode |
| ctrl+o | Verbose (full tool output, thinking) |
| ctrl+t | Toggle todo list |
| shift+enter or ctrl+j | New line |
| pgup/pgdown | Scroll |
| ctrl+c ×2 | Exit |

## Developing

`go test ./...`. CLAUDE.md describes the architecture, and new model backends implement `llm.Provider`.

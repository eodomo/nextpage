package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"

	"github.com/eodomo/nextpage/internal/agent"
	"github.com/eodomo/nextpage/internal/config"
	"github.com/eodomo/nextpage/internal/ext"
	"github.com/eodomo/nextpage/internal/hooks"
	"github.com/eodomo/nextpage/internal/learn"
	"github.com/eodomo/nextpage/internal/llm"
	"github.com/eodomo/nextpage/internal/mcp"
	"github.com/eodomo/nextpage/internal/memory"
	"github.com/eodomo/nextpage/internal/permission"
	"github.com/eodomo/nextpage/internal/session"
	"github.com/eodomo/nextpage/internal/tools"
	"github.com/eodomo/nextpage/internal/tui"
	"github.com/eodomo/nextpage/internal/web"
)

type options struct {
	print          bool
	continueLast   bool
	resume         string
	model          string
	permissionMode string
	skipPerms      bool
	allowedTools   string
	disallowed     string
	outputFormat   string
	appendSystem   string
	maxTurns       int
	think          bool
	verbose        bool
	profile        string
	coursesDir     string
	web            bool
	addr           string
}

func parseFlags() (options, string) {
	var o options
	flag.BoolVar(&o.print, "p", false, "print mode: answer the prompt non-interactively and exit")
	flag.BoolVar(&o.print, "print", false, "same as -p")
	flag.BoolVar(&o.continueLast, "c", false, "continue the most recent conversation in this project")
	flag.BoolVar(&o.continueLast, "continue", false, "same as -c")
	flag.StringVar(&o.resume, "r", "", "resume the conversation with this session id")
	flag.StringVar(&o.resume, "resume", "", "same as -r")
	flag.StringVar(&o.model, "model", "", "model to use (overrides MODEL and settings)")
	flag.StringVar(&o.permissionMode, "permission-mode", "", "default | acceptEdits | plan | bypassPermissions")
	flag.BoolVar(&o.skipPerms, "dangerously-skip-permissions", false, "run every tool without asking")
	flag.StringVar(&o.allowedTools, "allowedTools", "", "comma-separated permission rules to allow, e.g. \"Bash(git:*),Edit\"")
	flag.StringVar(&o.disallowed, "disallowedTools", "", "comma-separated permission rules to deny")
	flag.StringVar(&o.outputFormat, "output-format", "text", "print mode output: text | json | stream-json")
	flag.StringVar(&o.appendSystem, "append-system-prompt", "", "text appended to the system prompt")
	flag.IntVar(&o.maxTurns, "max-turns", 0, "maximum agentic turns per prompt")
	flag.BoolVar(&o.think, "think", false, "enable extended thinking for models that support it")
	flag.BoolVar(&o.verbose, "verbose", false, "print mode: log tool calls to stderr")
	flag.StringVar(&o.profile, "profile", "", "learn (AI tutor, default) | code (coding agent)")
	flag.StringVar(&o.coursesDir, "courses-dir", "", "directory for course notes, e.g. inside an Obsidian vault")
	flag.BoolVar(&o.web, "web", false, "serve the web app instead of the terminal UI (needs NEXTPAGE_PASSWORD)")
	flag.StringVar(&o.addr, "addr", ":8080", "web app listen address")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: nextpage [flags] [prompt]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	return o, strings.Join(flag.Args(), " ")
}

func splitRules(s string) []string {
	var out []string
	depth, last := 0, 0
	for i, c := range s {
		switch c {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(s[last:i]))
				last = i + 1
			}
		}
	}
	if r := strings.TrimSpace(s[last:]); r != "" {
		out = append(out, r)
	}
	return out
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "nextpage:", err)
		os.Exit(1)
	}
}

func run() error {
	opts, prompt := parseFlags()

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	paths := config.ResolvePaths(cwd)
	settings, err := config.Load(paths)
	if err != nil {
		return err
	}
	for k, v := range settings.Env {
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}

	logPath := os.Getenv("LOGPATH")
	if logPath == "" {
		logPath = filepath.Join(paths.UserDir, "nextpage.log")
		_ = os.MkdirAll(paths.UserDir, 0o755)
	}
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	log.SetOutput(logFile)

	// Precedence: flags > env > settings.
	if v := os.Getenv("OLLAMASERVER"); v != "" {
		settings.Server = v
	}
	if v := os.Getenv("MODEL"); v != "" {
		settings.Model = v
	}
	username, password := os.Getenv("OLLAMAUSER"), os.Getenv("OLLAMAPASS")
	// A connection saved from the web app's settings wins over env vars.
	connPath := config.ConnectionPath(paths.UserDir)
	conn, err := config.LoadConnection(connPath)
	if err != nil {
		return fmt.Errorf("%s: %w", connPath, err)
	}
	if conn.Server != "" {
		settings.Server, username, password = conn.Server, conn.Username, conn.Password
	}
	if conn.Model != "" {
		settings.Model = conn.Model
	}
	if opts.model != "" {
		settings.Model = opts.model
	}
	// The web app can be configured from its settings page, so it may start
	// without a server or model.
	if settings.Model == "" && !opts.web {
		return fmt.Errorf("no model configured: set MODEL, pass --model, or set \"model\" in %s", paths.User)
	}
	if settings.NumCtx == 0 {
		settings.NumCtx = 32768
	}
	if settings.MaxTokens == 0 {
		settings.MaxTokens = 4096
	}
	if v := os.Getenv("NEXTPAGE_PROFILE"); v != "" {
		settings.Profile = v
	}
	if opts.profile != "" {
		settings.Profile = opts.profile
	}
	if settings.Profile == "" {
		settings.Profile = "learn"
	}
	if settings.Profile != "learn" && settings.Profile != "code" {
		return fmt.Errorf("unknown profile %q (use learn or code)", settings.Profile)
	}
	learning := settings.Profile == "learn"
	if v := os.Getenv("NEXTPAGE_COURSES_DIR"); v != "" {
		settings.CoursesDir = v
	}
	if opts.coursesDir != "" {
		settings.CoursesDir = opts.coursesDir
	}

	provider, err := llm.New(llm.ProviderConfig{
		Kind:     settings.Provider,
		BaseURL:  settings.Server,
		Username: username,
		Password: password,
	})
	if err != nil {
		if !opts.web {
			return err
		}
		log.Printf("no model server yet (%v); configure it in the web app", err)
		provider = nil
	}

	modeName := settings.Permissions.DefaultMode
	if opts.permissionMode != "" {
		modeName = opts.permissionMode
	}
	if opts.skipPerms {
		modeName = "bypassPermissions"
	}
	mode, err := permission.ParseMode(modeName)
	if err != nil {
		return err
	}
	perms := permission.NewChecker(paths.ProjectRoot, mode,
		append(settings.Permissions.Allow, splitRules(opts.allowedTools)...),
		append(settings.Permissions.Deny, splitRules(opts.disallowed)...),
		settings.Permissions.Ask)
	perms.AllowBypass = mode == permission.ModeBypass

	extDirs := ext.Dirs(paths.UserDir, paths.ProjectRoot)
	skills := ext.LoadSkills(extDirs)
	agentDefs := ext.LoadAgents(extDirs)
	commands := ext.LoadCommands(extDirs)

	env := tools.NewEnv(cwd, paths.ProjectRoot)
	env.Skills = skills
	env.Agents = agentDefs
	defer env.Shells.KillAll()

	registry := tools.DefaultRegistry()
	var courses *learn.Manager
	if learning {
		// The tutor only needs to read and research; course files are
		// written and quizzes given through the course tools. A short tool
		// list matters: small models pick the wrong tool from long ones.
		registry = registry.Filter(func(t tools.Tool) bool {
			switch t.Name() {
			case "Read", "WebFetch":
				return true
			}
			return false
		})
		courses = learn.NewManager(learn.Config{
			Format:      settings.LessonFormat,
			PassPercent: settings.PassPercent,
			SaveDir:     func(dir string) error { return config.SetValue(paths.User, "coursesDir", dir) },
		})
		if settings.CoursesDir != "" {
			if err := courses.SetDir(settings.CoursesDir); err != nil {
				return fmt.Errorf("courses dir: %w", err)
			}
		}
		for _, t := range learn.Tools(courses) {
			registry.Add(t)
		}
	}
	mcpManager := mcp.StartAll(context.Background(), settings.MCPServers, registry)
	defer mcpManager.Close()

	var memMu sync.Mutex
	loadMemory := func() []memory.File {
		files := memory.Load(paths.UserDir, cwd)
		if !learning {
			return files
		}
		// Project instruction files (CLAUDE.md etc.) describe code, not
		// the learner; only user-level memory applies to tutoring.
		var user []memory.File
		for _, f := range files {
			if strings.HasPrefix(f.Path, paths.UserDir) {
				user = append(user, f)
			}
		}
		return user
	}
	memFiles := loadMemory()
	getMemory := func() []memory.File {
		memMu.Lock()
		defer memMu.Unlock()
		return memFiles
	}
	reloadMemory := func() {
		files := loadMemory()
		memMu.Lock()
		memFiles = files
		memMu.Unlock()
	}

	sessionDir := session.ProjectDir(paths.UserDir, paths.ProjectRoot)
	if learning {
		// Tutoring sessions aren't tied to the directory nextpage runs in.
		sessionDir = filepath.Join(paths.UserDir, "learn-sessions")
	}
	var history []llm.Message
	var store *session.Store
	switch {
	case opts.resume != "":
		history, err = session.Load(filepath.Join(sessionDir, opts.resume+".jsonl"))
		if err != nil {
			return fmt.Errorf("resume %s: %w", opts.resume, err)
		}
		store, err = session.Open(sessionDir, opts.resume)
	case opts.continueLast:
		if list := session.List(sessionDir); len(list) > 0 {
			history, _ = session.Load(list[0].Path)
			store, err = session.Open(sessionDir, list[0].ID)
		} else {
			store, err = session.New(sessionDir)
		}
	default:
		store, err = session.New(sessionDir)
	}
	if err != nil {
		return err
	}
	defer store.Close()

	hookRunner := hooks.NewRunner(settings.Hooks)
	gitStatus := ""
	if !learning {
		gitStatus = agent.GitSummary(paths.ProjectRoot)
	}

	var ag *agent.Agent
	promptCtx := agent.PromptContext{
		Cwd:         env.Cwd,
		ProjectRoot: paths.ProjectRoot,
		Model:       func() string { return ag.CurrentModel() },
		Mode:        perms.Mode,
		Memory:      func() string { return memory.Format(getMemory()) },
		Skills:      skills,
		Agents:      agentDefs,
		GitStatus:   gitStatus,
		Extra:       opts.appendSystem,
	}
	var stopCheck func() string
	var onPrompt func(string)
	if learning {
		stopCheck = courses.StopCheck
		onPrompt = courses.UserSpoke
		promptCtx.Base = learn.BasePrompt
		promptCtx.Sections = []func() string{courses.StatePrompt}
	}
	think := opts.think || (settings.Think != nil && *settings.Think)
	// Thinking is slow on CPU-only servers, so the tutor turns it off unless
	// asked; the coding profile leaves the model's default.
	thinkExplicit := opts.think || settings.Think != nil || learning
	autoCompact := settings.AutoCompact == nil || *settings.AutoCompact
	maxTurns := settings.MaxTurns
	if opts.maxTurns > 0 {
		maxTurns = opts.maxTurns
	}
	ag = agent.New(agent.Config{
		Provider:      provider,
		Model:         settings.Model,
		Tools:         registry,
		Perms:         perms,
		Hooks:         hookRunner,
		Env:           env,
		Session:       store,
		SystemPrompt:  func() string { return agent.BuildSystemPrompt(promptCtx) },
		Think:         think,
		ThinkExplicit: thinkExplicit,
		NumCtx:        settings.NumCtx,
		Temperature:   settings.Temperature,
		MaxTurns:      maxTurns,
		MaxTokens:     settings.MaxTokens,
		StopCheck:     stopCheck,
		OnPrompt:      onPrompt,
		AutoCompact:   autoCompact,
		LocalSettings: paths.Local,
	})
	ag.SetHistory(history)

	source := "startup"
	if len(history) > 0 {
		source = "resume"
	}
	start := hookRunner.Run(context.Background(), hooks.Input{
		SessionID: store.ID, TranscriptPath: store.Path, Cwd: cwd,
		HookEventName: hooks.SessionStart, Source: source,
	})
	if start.Context != "" {
		ag.AddContext("<system-reminder>\nSessionStart hook context:\n" + start.Context + "\n</system-reminder>")
	}
	defer hookRunner.Run(context.Background(), hooks.Input{
		SessionID: store.ID, TranscriptPath: store.Path, Cwd: cwd, HookEventName: hooks.SessionEnd,
	})

	if opts.web {
		webPassword := os.Getenv("NEXTPAGE_PASSWORD")
		if webPassword == "" {
			return fmt.Errorf("web mode needs NEXTPAGE_PASSWORD to be set")
		}
		return web.Serve(context.Background(), web.Config{
			Addr:       opts.addr,
			Password:   webPassword,
			Agent:      ag,
			Perms:      perms,
			Courses:    courses,
			SessionDir: sessionDir,
			Connection: web.ConnectionConfig{
				Path:         connPath,
				ProviderKind: settings.Provider,
				Server:       settings.Server,
				Username:     username,
				Password:     password,
			},
		})
	}

	if opts.print {
		if stdin, ok := readPipedStdin(); ok {
			prompt = strings.TrimSpace(prompt + "\n\n" + stdin)
		}
		if prompt == "" {
			return fmt.Errorf("print mode needs a prompt (argument or stdin)")
		}
		return runPrint(ag, prompt, opts)
	}

	m := tui.New(tui.Deps{
		Agent:         ag,
		Perms:         perms,
		Env:           env,
		Hooks:         hookRunner,
		MCP:           mcpManager,
		Paths:         paths,
		Settings:      settings,
		Commands:      commands,
		Skills:        skills,
		Agents:        agentDefs,
		SessionDir:    sessionDir,
		Memory:        getMemory,
		ReloadMemory:  reloadMemory,
		InitialPrompt: prompt,
		Courses:       courses,
	})
	_, err = tea.NewProgram(m).Run()
	return err
}

func readPipedStdin() (string, bool) {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice != 0 {
		return "", false
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", false
	}
	return string(data), len(data) > 0
}

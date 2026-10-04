// Package web serves nextpage as a single-user web app: a password login,
// a JSON API, and a Server-Sent Events stream of agent events. It is a third
// front end on the same agent.Event stream the TUI and print mode consume.
package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/eodomo/nextpage/internal/agent"
	"github.com/eodomo/nextpage/internal/learn"
	"github.com/eodomo/nextpage/internal/llm"
	"github.com/eodomo/nextpage/internal/permission"
	"github.com/eodomo/nextpage/internal/session"
	"github.com/eodomo/nextpage/internal/tools"
)

//go:embed static
var staticFS embed.FS

type Config struct {
	Addr       string
	Password   string
	Agent      *agent.Agent
	Perms      *permission.Checker
	Courses    *learn.Manager // nil in the code profile
	SessionDir string
	Connection ConnectionConfig
}

// ConnectionConfig is the model server login currently in use. The settings
// page edits it; changes are saved to Path and applied immediately.
type ConnectionConfig struct {
	Path         string
	ProviderKind string
	Server       string
	Username     string
	Password     string
}

type Server struct {
	cfg Config

	mu      sync.Mutex
	tokens  map[string]time.Time // login sessions → expiry
	subs    map[chan []byte]struct{}
	running bool
	cancel  context.CancelFunc
	live    []json.RawMessage // events of the current run, for reloads
	pending map[string]*pending
	nextID  int
}

type pending struct {
	ID    string          `json:"id"`
	Event json.RawMessage `json:"event"`
	reply func(json.RawMessage) error
}

const cookieName = "nextpage_session"

func Serve(ctx context.Context, cfg Config) error {
	s := &Server{
		cfg:     cfg,
		tokens:  map[string]time.Time{},
		subs:    map[chan []byte]struct{}{},
		pending: map[string]*pending{},
	}
	if cfg.Courses != nil {
		cfg.Courses.SetOnChange(func() { s.broadcast(map[string]any{"type": "courses_changed"}) })
	}
	static, _ := fs.Sub(staticFS, "static")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.logout)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /{$}", s.page(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, static, "index.html")
	}))
	mux.HandleFunc("GET /api/state", s.api(s.state))
	mux.HandleFunc("GET /api/events", s.api(s.events))
	mux.HandleFunc("POST /api/chat", s.api(s.chat))
	mux.HandleFunc("POST /api/interrupt", s.api(s.interrupt))
	mux.HandleFunc("POST /api/reply", s.api(s.reply))
	mux.HandleFunc("POST /api/new", s.api(s.newConversation))
	mux.HandleFunc("GET /api/courses", s.api(s.courses))
	mux.HandleFunc("GET /api/file", s.api(s.file))
	mux.HandleFunc("POST /api/courses/delete", s.api(s.deleteCourse))
	mux.HandleFunc("GET /api/settings", s.api(s.getSettings))
	mux.HandleFunc("POST /api/settings", s.api(s.saveSettings))
	mux.HandleFunc("POST /api/settings/test", s.api(s.testSettings))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	srv := &http.Server{Addr: cfg.Addr, Handler: securityHeaders(mux), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()
	log.Printf("web: listening on %s", cfg.Addr)
	fmt.Printf("nextpage web app listening on %s\n", cfg.Addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		h.ServeHTTP(w, r)
	})
}

// ---- auth ----

func (s *Server) authed(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.tokens[c.Value]
	if !ok || time.Now().After(exp) {
		delete(s.tokens, c.Value)
		return false
	}
	return true
}

func (s *Server) page(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authed(r) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		h(w, r)
	}
}

// api requires a login and, for POSTs, a JSON body (which a cross-site form
// can't send), on top of the SameSite=Strict cookie.
func (s *Server) api(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authed(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not logged in"})
			return
		}
		if r.Method == http.MethodPost && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "expected application/json"})
			return
		}
		h(w, r)
	}
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if s.authed(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	data, _ := staticFS.ReadFile("static/login.html")
	page := string(data)
	msg := ""
	if r.URL.Query().Get("failed") != "" {
		msg = `<p class="login-error">Wrong password.</p>`
	}
	page = strings.Replace(page, "<!--error-->", msg, 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(page))
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	got := sha256.Sum256([]byte(r.FormValue("password")))
	want := sha256.Sum256([]byte(s.cfg.Password))
	if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		time.Sleep(time.Second) // slow down guessing
		http.Redirect(w, r, "/login?failed=1", http.StatusSeeOther)
		return
	}
	b := make([]byte, 32)
	rand.Read(b)
	token := hex.EncodeToString(b)
	const ttl = 30 * 24 * time.Hour
	s.mu.Lock()
	s.tokens[token] = time.Now().Add(ttl)
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.mu.Lock()
		delete(s.tokens, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---- events ----

func (s *Server) broadcast(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		log.Printf("web: marshal event: %v", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		s.live = append(s.live, data)
	}
	for ch := range s.subs {
		select {
		case ch <- data:
		default: // slow client; it will resync from /api/state on reconnect
		}
	}
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch := make(chan []byte, 512)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case data := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// emit converts agent events to JSON for the browser. Events that need an
// answer are registered as pending until /api/reply resolves them.
func (s *Server) emit(ev agent.Event) {
	switch e := ev.(type) {
	case agent.TextDelta:
		if e.Agent == "" {
			s.broadcast(map[string]any{"type": "text", "text": e.Text})
		}
	case agent.ThinkingDelta:
		if e.Agent == "" {
			s.broadcast(map[string]any{"type": "thinking", "text": e.Text})
		}
	case agent.AssistantMessage:
		if e.Agent == "" {
			s.broadcast(map[string]any{"type": "assistant_done"})
		}
	case agent.ToolStart:
		s.broadcast(map[string]any{"type": "tool_start", "id": e.Agent + "/" + e.ID, "name": e.Name, "subject": e.Subject, "agent": e.Agent})
	case agent.ToolEnd:
		// Failed calls are the model's internal retries: they are logged by
		// the agent and never shown. Successful ones show their short status.
		out := ""
		if !e.Result.IsError {
			out = e.Result.Display
			if out == "" && s.cfg.Courses == nil {
				out = firstLine(e.Result.Output)
			}
		}
		s.broadcast(map[string]any{"type": "tool_end", "id": e.Agent + "/" + e.ID, "name": e.Name, "output": out, "is_error": e.Result.IsError, "open": s.fileToOpen(e)})
	case agent.Notice:
		if e.Level == agent.NoticeDebug {
			return
		}
		s.broadcast(map[string]any{"type": "notice", "text": e.Text, "level": int(e.Level)})
	case agent.UsageUpdate:
		s.broadcast(map[string]any{"type": "usage", "context": e.ContextTokens, "output": e.Total.CompletionTokens})
	case agent.PermissionRequest:
		s.ask(map[string]any{"type": "permission", "tool": e.ToolName, "subject": e.Subject, "preview": e.Preview, "suggestion": e.Suggestion, "agent": e.Agent},
			func(raw json.RawMessage) error {
				var v struct {
					Allow    bool   `json:"allow"`
					Remember bool   `json:"remember"`
					Feedback string `json:"feedback"`
				}
				if err := json.Unmarshal(raw, &v); err != nil {
					return err
				}
				reply := agent.PermissionReply{Allow: v.Allow, Feedback: v.Feedback}
				if v.Remember {
					reply.Remember = agent.RememberSession
				}
				e.Reply <- reply
				return nil
			})
	case agent.QuestionRequest:
		s.ask(map[string]any{"type": "question", "question": e.Question.Question, "options": e.Question.Options},
			func(raw json.RawMessage) error {
				var v string
				if err := json.Unmarshal(raw, &v); err != nil {
					return err
				}
				e.Reply <- v
				return nil
			})
	case agent.PlanRequest:
		s.ask(map[string]any{"type": "plan", "plan": e.Plan}, func(raw json.RawMessage) error {
			var v bool
			if err := json.Unmarshal(raw, &v); err != nil {
				return err
			}
			e.Reply <- v
			return nil
		})
	case agent.InteractionRequest:
		switch p := e.Payload.(type) {
		case learn.QuizRequest:
			s.ask(map[string]any{"type": "quiz", "quiz": p}, func(raw json.RawMessage) error {
				var answers []string
				if err := json.Unmarshal(raw, &answers); err != nil {
					return err
				}
				e.Reply <- answers
				return nil
			})
		case learn.DirectoryRequest:
			s.ask(map[string]any{"type": "directory", "default": p.Default}, func(raw json.RawMessage) error {
				var v string
				if err := json.Unmarshal(raw, &v); err != nil {
					return err
				}
				e.Reply <- v
				return nil
			})
		default:
			e.Reply <- nil
		}
	}
}

// fileToOpen tells the browser which note a course tool just wrote, so the
// reader can show it.
func (s *Server) fileToOpen(e agent.ToolEnd) string {
	c := s.cfg.Courses
	if c == nil || e.Result.IsError {
		return ""
	}
	switch e.Name {
	case "WriteLesson":
		if e.Result.Display == "Lesson saved" {
			return c.LastLesson()
		}
	case "SaveCoursePlan", "OpenCourse":
		if a := c.Active(); a != nil {
			return a.Folder + "/" + learn.PlanFile
		}
	}
	return ""
}

func (s *Server) ask(event map[string]any, reply func(json.RawMessage) error) {
	s.mu.Lock()
	s.nextID++
	id := fmt.Sprintf("i%d", s.nextID)
	event["id"] = id
	data, _ := json.Marshal(event)
	s.pending[id] = &pending{ID: id, Event: data, reply: reply}
	s.mu.Unlock()
	s.broadcast(map[string]any{"type": "interaction", "id": id, "event": json.RawMessage(data)})
}

// ---- handlers ----

type historyItem struct {
	Role    string `json:"role"` // user | assistant | tool
	Text    string `json:"text,omitempty"`
	Name    string `json:"name,omitempty"`
	Subject string `json:"subject,omitempty"`
	Output  string `json:"output,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
}

func (s *Server) history() []historyItem {
	ag := s.cfg.Agent
	h := ag.History()
	results := map[string]llm.Message{}
	for _, m := range h {
		if m.Role == llm.RoleTool {
			results[m.ToolCallID] = m
		}
	}
	var out []historyItem
	for _, m := range h {
		switch m.Role {
		case llm.RoleUser:
			if m.Harness {
				continue
			}
			text := m.Content
			if i := strings.Index(text, "\n\n<system-reminder>"); i >= 0 {
				text = text[:i]
			}
			if strings.HasPrefix(text, "<") || strings.HasPrefix(text, "[Request interrupted") {
				continue
			}
			out = append(out, historyItem{Role: "user", Text: text})
		case llm.RoleAssistant:
			if strings.TrimSpace(m.Content) != "" && !m.Harness {
				out = append(out, historyItem{Role: "assistant", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				if r, ok := results[tc.ID]; ok && strings.HasPrefix(r.Content, "Error") {
					continue // internal retries are not shown
				}
				it := historyItem{Role: "tool", Name: tc.Name}
				if t, ok := ag.Tools.Get(tc.Name); ok {
					it.Subject = tools.Subject(t, tc.Args)
				}
				if r, ok := results[tc.ID]; ok && s.cfg.Courses == nil {
					it.Output = firstLine(r.Content)
				}
				out = append(out, it)
			}
		}
	}
	return out
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	live := append([]json.RawMessage(nil), s.live...)
	var pend []*pending
	for _, p := range s.pending {
		pend = append(pend, p)
	}
	running := s.running
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"running":    running,
		"live":       live,
		"pending":    pend,
		"history":    s.history(),
		"model":      s.cfg.Agent.CurrentModel(),
		"learning":   s.cfg.Courses != nil,
		"configured": s.cfg.Agent.CurrentProvider() != nil && s.cfg.Agent.CurrentModel() != "",
	})
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	var req struct{ Text string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Text) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty message"})
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		cancel()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "nextpage is still working; wait or interrupt it"})
		return
	}
	s.running = true
	s.cancel = cancel
	s.live = nil
	s.mu.Unlock()

	s.broadcast(map[string]any{"type": "user", "text": req.Text})
	go func() {
		err := s.cfg.Agent.Run(ctx, req.Text, s.emit)
		msg := ""
		switch {
		case errors.Is(err, context.Canceled):
			msg = "Interrupted."
		case err != nil:
			msg = err.Error()
		}
		s.mu.Lock()
		s.running = false
		s.cancel = nil
		s.pending = map[string]*pending{}
		s.mu.Unlock()
		s.broadcast(map[string]any{"type": "done", "error": msg})
		s.mu.Lock()
		s.live = nil
		s.mu.Unlock()
	}()
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

func (s *Server) interrupt(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) reply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID    string          `json:"id"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.mu.Lock()
	p := s.pending[req.ID]
	delete(s.pending, req.ID)
	s.mu.Unlock()
	if p == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "that request is no longer waiting for an answer"})
		return
	}
	if err := p.reply(req.Value); err != nil {
		s.mu.Lock()
		s.pending[req.ID] = p
		s.mu.Unlock()
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.broadcast(map[string]any{"type": "resolved", "id": req.ID})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) newConversation(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	if running {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "interrupt the current response first"})
		return
	}
	ag := s.cfg.Agent
	ag.SetHistory(nil)
	if ag.Session != nil {
		ag.Session.Close()
	}
	if st, err := session.New(s.cfg.SessionDir); err == nil {
		ag.Session = st
	}
	s.broadcast(map[string]any{"type": "cleared"})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) courses(w http.ResponseWriter, r *http.Request) {
	c := s.cfg.Courses
	if c == nil {
		writeJSON(w, http.StatusOK, map[string]any{"dir": "", "courses": []any{}})
		return
	}
	type course struct {
		learn.Summary
		Files []string `json:"files"`
	}
	var list []course
	for _, sum := range c.List() {
		list = append(list, course{Summary: sum, Files: c.Files(sum.Folder)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"dir": c.Dir(), "courses": list})
}

func (s *Server) deleteCourse(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Courses == nil {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Folder string `json:"folder"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Folder == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "which course?"})
		return
	}
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	if running {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "wait for the tutor to finish (or stop it) before deleting a course"})
		return
	}
	folder, err := s.cfg.Courses.Delete(req.Folder)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	log.Printf("web: deleted course %q", folder)
	writeJSON(w, http.StatusOK, map[string]string{"deleted": folder})
}

func (s *Server) file(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Courses == nil {
		http.NotFound(w, r)
		return
	}
	data, err := s.cfg.Courses.ReadFile(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(data)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

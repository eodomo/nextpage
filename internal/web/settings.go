package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/eodomo/nextpage/internal/config"
	"github.com/eodomo/nextpage/internal/llm"
)

type settingsRequest struct {
	Server   string `json:"server"`
	Username string `json:"username"`
	// Password replaces the saved one when non-empty; KeepPassword keeps the
	// saved one when the field is left blank, so it never round-trips.
	Password     string `json:"password"`
	KeepPassword bool   `json:"keep_password"`
	Model        string `json:"model"`
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	c := s.cfg.Connection
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"server":       c.Server,
		"username":     c.Username,
		"password_set": c.Password != "",
		"model":        s.cfg.Agent.CurrentModel(),
	})
}

// provider builds a provider from the request, filling in the saved password
// when asked to keep it.
func (s *Server) provider(req settingsRequest) (llm.Provider, config.Connection, error) {
	server := strings.TrimRight(strings.TrimSpace(req.Server), "/")
	u, err := url.Parse(server)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, config.Connection{}, errors.New("server must be a full URL, e.g. https://ollama.example.com")
	}
	s.mu.Lock()
	cur := s.cfg.Connection
	s.mu.Unlock()
	conn := config.Connection{Server: server, Username: strings.TrimSpace(req.Username), Password: req.Password, Model: strings.TrimSpace(req.Model)}
	if conn.Password == "" && req.KeepPassword {
		conn.Password = cur.Password
	}
	p, err := llm.New(llm.ProviderConfig{Kind: cur.ProviderKind, BaseURL: conn.Server, Username: conn.Username, Password: conn.Password})
	return p, conn, err
}

func listModels(p llm.Provider) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	models, err := p.ListModels(ctx)
	if err != nil {
		return nil, errors.New("could not reach the server: " + err.Error())
	}
	return models, nil
}

// testSettings checks a connection and returns the server's models.
func (s *Server) testSettings(w http.ResponseWriter, r *http.Request) {
	var req settingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	p, _, err := s.provider(req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	models, err := listModels(p)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	var req settingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	if running {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "wait for the tutor to finish (or stop it) before changing the connection"})
		return
	}
	p, conn, err := s.provider(req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if conn.Model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "choose a model"})
		return
	}
	if _, err := listModels(p); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if err := config.SaveConnection(s.cfg.Connection.Path, conn); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "saving settings: " + err.Error()})
		return
	}
	s.mu.Lock()
	s.cfg.Connection.Server, s.cfg.Connection.Username, s.cfg.Connection.Password = conn.Server, conn.Username, conn.Password
	s.mu.Unlock()
	s.cfg.Agent.SetProvider(p)
	s.cfg.Agent.SetModel(conn.Model)
	s.broadcast(map[string]any{"type": "settings", "model": conn.Model})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

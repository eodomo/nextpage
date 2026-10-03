// Package session persists conversations as JSONL transcripts under
// ~/.nextpage/projects/<project-slug>/<session-id>.jsonl so they can be
// resumed with --continue, --resume or /resume.
package session

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/eodomo/nextpage/internal/llm"
)

type entry struct {
	Type    string       `json:"type"` // "message" | "compact"
	Time    time.Time    `json:"time"`
	Message *llm.Message `json:"message,omitempty"`
}

type Store struct {
	mu   sync.Mutex
	ID   string
	Path string
	f    *os.File
}

func ProjectDir(userDir, projectRoot string) string {
	slug := strings.NewReplacer("/", "-", "\\", "-", ":", "-", ".", "-").Replace(projectRoot)
	return filepath.Join(userDir, "projects", slug)
}

// New starts a fresh transcript in dir.
func New(dir string) (*Store, error) {
	return Open(dir, uuid.NewString())
}

// Open appends to the transcript with the given id, creating it if needed.
func Open(dir, id string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, id+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Store{ID: id, Path: path, f: f}, nil
}

func (s *Store) Append(m llm.Message) error {
	return s.write(entry{Type: "message", Time: time.Now(), Message: &m})
}

// MarkCompact records that history was replaced; Load discards everything
// before the latest marker.
func (s *Store) MarkCompact() error {
	return s.write(entry{Type: "compact", Time: time.Now()})
}

func (s *Store) write(e entry) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = s.f.Write(append(b, '\n'))
	return err
}

func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	return s.f.Close()
}

// Load reads the messages of a transcript.
func Load(path string) ([]llm.Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var msgs []llm.Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		var e entry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		switch e.Type {
		case "compact":
			msgs = nil
		case "message":
			if e.Message != nil {
				msgs = append(msgs, *e.Message)
			}
		}
	}
	return msgs, sc.Err()
}

type Info struct {
	ID       string
	Path     string
	Modified time.Time
	Summary  string // first user prompt
	Messages int
}

// List returns the sessions in dir, most recent first.
func List(dir string) []Info {
	paths, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	var out []Info
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil || fi.Size() == 0 {
			continue
		}
		msgs, _ := Load(p)
		info := Info{ID: strings.TrimSuffix(filepath.Base(p), ".jsonl"), Path: p, Modified: fi.ModTime(), Messages: len(msgs)}
		for _, m := range msgs {
			if m.Role == llm.RoleUser && !strings.HasPrefix(m.Content, "<") {
				info.Summary = strings.Join(strings.Fields(m.Content), " ")
				break
			}
		}
		if len(info.Summary) > 70 {
			info.Summary = info.Summary[:67] + "..."
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out
}

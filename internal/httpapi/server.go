package httpapi

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wbot-dev/wbot/internal/agent"
	"github.com/wbot-dev/wbot/internal/config"
	"github.com/wbot-dev/wbot/internal/memory"
	"github.com/wbot-dev/wbot/internal/storage"
)

type Server struct {
	s        config.Settings
	store    *storage.Store
	agent    *agent.Service
	memories *memory.Manager
	mux      *http.ServeMux
}

func New(s config.Settings, st *storage.Store, a *agent.Service, m *memory.Manager) *Server {
	x := &Server{s: s, store: st, agent: a, memories: m, mux: http.NewServeMux()}
	x.routes()
	return x
}
func (s *Server) Handler() http.Handler { return s.withMiddleware(s.mux) }
func (s *Server) routes() {
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { write(w, 200, map[string]any{"status": "ok"}) })
	s.mux.HandleFunc("/api/v1/auth/session", s.authSession)
	s.mux.HandleFunc("/api/v1/workspaces/open", s.openWorkspace)
	s.mux.HandleFunc("/api/v1/workspaces", s.workspaces)
	s.mux.HandleFunc("/api/v1/sessions", s.createSession)
	s.mux.HandleFunc("/api/v1/sessions/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages") {
			s.message(w, r)
		} else if strings.HasSuffix(r.URL.Path, "/events") {
			s.events(w, r)
		} else {
			s.session(w, r)
		}
	})
	s.mux.HandleFunc("/api/v1/tasks/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			s.cancel(w, r)
		} else {
			s.task(w, r)
		}
	})
	s.mux.HandleFunc("/api/v1/approvals", s.approvals)
	s.mux.HandleFunc("/api/v1/approvals/", s.decide)
	s.mux.HandleFunc("/api/v1/artifacts/", s.artifact)
	s.mux.HandleFunc("/api/v1/memories", s.listMemories)
	s.mux.HandleFunc("/api/v1/metrics", func(w http.ResponseWriter, r *http.Request) { x, e := s.store.Metrics(r.Context()); respond(w, x, e) })
	s.mux.HandleFunc("/api/v1/memories/", s.deleteMemory)
	s.mux.HandleFunc("/", s.static)
}
func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		if strings.HasPrefix(r.URL.Path, "/api/") && s.s.AuthToken != "" {
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if token == "" {
				if c, e := r.Cookie("wbot_session"); e == nil {
					token = c.Value
				}
			}
			if subtle.ConstantTimeCompare([]byte(token), []byte(s.s.AuthToken)) != 1 {
				write(w, 401, map[string]string{"error": "unauthorized"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
func (s *Server) authSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "wbot_session", Value: s.s.AuthToken, Path: "/api/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: 3600})
	write(w, 200, map[string]bool{"authenticated": true})
}
func (s *Server) openWorkspace(w http.ResponseWriter, r *http.Request) {
	var in struct{ Name, Path, Kind string }
	if !decode(w, r, &in) {
		return
	}
	p := in.Path
	if p == "" {
		p = s.s.WorkspaceRoot
	}
	a, e := filepath.Abs(p)
	if e != nil {
		bad(w, e)
		return
	}
	if real, x := filepath.EvalSymlinks(a); x == nil {
		a = real
	}
	root := s.s.WorkspaceRoot
	if real, x := filepath.EvalSymlinks(root); x == nil {
		root = real
	}
	rel, e := filepath.Rel(root, a)
	if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		bad(w, fmt.Errorf("workspace must be within WBOT_WORKSPACE_ROOT"))
		return
	}
	if in.Name == "" {
		in.Name = filepath.Base(a)
	}
	if in.Kind == "" {
		in.Kind = "local"
	}
	x, e := s.store.OpenWorkspace(r.Context(), in.Name, a, in.Kind)
	respond(w, x, e)
}
func (s *Server) workspaces(w http.ResponseWriter, r *http.Request) {
	x, e := s.store.Workspaces(r.Context())
	respond(w, x, e)
}
func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var in struct{ WorkspaceID, Title string }
	if !decode(w, r, &in) {
		return
	}
	if in.Title == "" {
		in.Title = "新会话"
	}
	x, e := s.store.CreateSession(r.Context(), in.WorkspaceID, in.Title)
	respond(w, x, e)
}
func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	id := segment(r.URL.Path, "sessions")
	x, e := s.store.Session(r.Context(), id)
	if e != nil {
		respond(w, nil, e)
		return
	}
	msgs, _ := s.store.Messages(r.Context(), id, 100)
	tasks, _ := s.store.TasksBySession(r.Context(), id)
	write(w, 200, map[string]any{"session": x, "messages": msgs, "tasks": tasks})
}
func (s *Server) message(w http.ResponseWriter, r *http.Request) {
	var in struct{ Content string }
	if !decode(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Content) == "" {
		bad(w, fmt.Errorf("content is required"))
		return
	}
	x, e := s.agent.Start(r.Context(), segment(r.URL.Path, "sessions"), in.Content)
	if e != nil {
		respond(w, nil, e)
		return
	}
	write(w, 202, x)
}
func (s *Server) task(w http.ResponseWriter, r *http.Request) {
	x, e := s.store.Task(r.Context(), segment(r.URL.Path, "tasks"))
	if e != nil {
		respond(w, nil, e)
		return
	}
	nodes, _ := s.store.Nodes(r.Context(), x.ID)
	criteria, _ := s.store.Criteria(r.Context(), x.ID)
	write(w, 200, map[string]any{"task": x, "nodes": nodes, "acceptance_criteria": criteria})
}
func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	t, e := s.store.Task(r.Context(), segment(r.URL.Path, "tasks"))
	if e == nil {
		s.agent.Cancel(t.ID)
		e = s.store.UpdateTask(r.Context(), t.ID, "cancelled", "", "")
		s.store.Emit(r.Context(), t.SessionID, t.ID, "task.cancelled", map[string]any{})
	}
	respond(w, map[string]bool{"cancelled": e == nil}, e)
}
func (s *Server) approvals(w http.ResponseWriter, r *http.Request) {
	x, e := s.store.Approvals(r.Context(), r.URL.Query().Get("status"))
	respond(w, x, e)
}
func (s *Server) decide(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	d := ""
	if len(parts) > 0 {
		d = parts[len(parts)-1]
	}
	if d == "approve" {
		d = "approved"
	} else if d == "reject" {
		d = "rejected"
	}
	a, e := s.store.DecideApproval(r.Context(), segment(r.URL.Path, "approvals"), d)
	if e != nil {
		respond(w, nil, e)
		return
	}
	t, _ := s.store.Task(r.Context(), a.TaskID)
	s.store.Emit(r.Context(), t.SessionID, t.ID, "approval.decided", a)
	if d == "approved" {
		s.agent.Resume(context.Background(), a.TaskID)
	} else {
		s.store.UpdateTask(r.Context(), a.TaskID, "failed", "", "approval rejected")
	}
	write(w, 200, a)
}
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	f, ok := w.(http.Flusher)
	if !ok {
		bad(w, fmt.Errorf("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	after, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	old, _ := s.store.Events(r.Context(), segment(r.URL.Path, "sessions"), after)
	send := func(ev any, id int64) {
		b, _ := json.Marshal(ev)
		fmt.Fprintf(w, "id: %d\ndata: %s\n\n", id, b)
		f.Flush()
	}
	for _, ev := range old {
		send(ev, ev.ID)
	}
	ch, closeFn := s.store.Subscribe(segment(r.URL.Path, "sessions"))
	defer closeFn()
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case ev := <-ch:
			send(ev, ev.ID)
		case <-ticker.C:
			fmt.Fprint(w, ": keepalive\n\n")
			f.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
func (s *Server) artifact(w http.ResponseWriter, r *http.Request) {
	p, m, e := s.store.Artifact(r.Context(), segment(r.URL.Path, "artifacts"))
	if e != nil {
		respond(w, nil, e)
		return
	}
	w.Header().Set("Content-Type", m)
	http.ServeFile(w, r, p)
}
func (s *Server) listMemories(w http.ResponseWriter, r *http.Request) {
	x, e := s.memories.List(r.Context())
	respond(w, x, e)
}
func (s *Server) deleteMemory(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut {
		var in memory.Entry
		if !decode(w, r, &in) {
			return
		}
		in.ID = segment(r.URL.Path, "memories")
		e := s.memories.Upsert(r.Context(), in)
		respond(w, map[string]bool{"updated": e == nil}, e)
		return
	}
	e := s.memories.Delete(r.Context(), segment(r.URL.Path, "memories"))
	respond(w, map[string]bool{"deleted": e == nil}, e)
}
func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	p := filepath.Join("web", "dist")
	if _, e := os.Stat(filepath.Join(p, "index.html")); e != nil {
		write(w, 200, map[string]string{"message": "wbot API is running; build web UI with npm run build"})
		return
	}
	target := filepath.Join(p, filepath.Clean(r.URL.Path))
	if r.URL.Path == "/" {
		target = filepath.Join(p, "index.html")
	}
	if _, e := os.Stat(target); e != nil {
		target = filepath.Join(p, "index.html")
	}
	http.ServeFile(w, r, target)
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	defer r.Body.Close()
	if e := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); e != nil {
		bad(w, e)
		return false
	}
	return true
}
func respond(w http.ResponseWriter, v any, e error) {
	if e == nil {
		write(w, 200, v)
		return
	}
	if e == sql.ErrNoRows {
		write(w, 404, map[string]string{"error": "not found"})
		return
	}
	bad(w, e)
}
func bad(w http.ResponseWriter, e error) { write(w, 400, map[string]string{"error": e.Error()}) }
func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func segment(path, key string) string {
	p := strings.Split(strings.Trim(path, "/"), "/")
	for i := range p {
		if p[i] == key && i+1 < len(p) {
			return p[i+1]
		}
	}
	return ""
}

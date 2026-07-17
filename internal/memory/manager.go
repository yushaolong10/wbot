package memory

import (
	"context"
	"errors"
	"fmt"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Entry struct {
	ID         string     `json:"id"`
	Type       string     `json:"type"`
	Content    string     `json:"content"`
	Summary    string     `json:"summary"`
	Tags       []string   `json:"tags"`
	Confidence float64    `json:"confidence"`
	Importance float64    `json:"importance"`
	Status     string     `json:"status"`
	UpdatedAt  time.Time  `json:"updated_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	Version    int        `json:"version"`
}
type Manager struct {
	root string
	path string
	mu   sync.Mutex
}

func New(root string) *Manager { return &Manager{root: root, path: filepath.Join(root, "index.yaml")} }
func (m *Manager) load() ([]Entry, error) {
	b, e := os.ReadFile(m.path)
	if errors.Is(e, os.ErrNotExist) {
		b, e = os.ReadFile(filepath.Join(m.root, "index.json"))
		if errors.Is(e, os.ErrNotExist) {
			return []Entry{}, nil
		}
	}
	if e != nil {
		return nil, e
	}
	var x []Entry
	e = yaml.Unmarshal(b, &x)
	return x, e
}
func (m *Manager) save(x []Entry) error {
	b, _ := yaml.Marshal(x)
	tmp := m.path + ".tmp"
	f, e := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	if x := f.Close(); e == nil {
		e = x
	}
	if e != nil {
		return e
	}
	if e = os.Rename(tmp, m.path); e != nil {
		return e
	}
	return m.writeClassified(x)
}
func (m *Manager) writeClassified(x []Entry) error {
	groups := map[string][]Entry{}
	for _, e := range x {
		if e.Status == "active" {
			groups[e.Type] = append(groups[e.Type], e)
		}
	}
	for typ, entries := range groups {
		dir := filepath.Join(m.root, typ)
		if e := os.MkdirAll(dir, 0700); e != nil {
			return e
		}
		var b strings.Builder
		b.WriteString("# " + typ + " memories\n\n")
		for _, e := range entries {
			fmt.Fprintf(&b, "## %s\n\n%s\n\n- ID: `%s`\n- Version: %d\n- Updated: %s\n\n", e.Summary, e.Content, e.ID, e.Version, e.UpdatedAt.Format(time.RFC3339))
		}
		if e := os.WriteFile(filepath.Join(dir, "entries.md"), []byte(b.String()), 0600); e != nil {
			return e
		}
	}
	return nil
}
func (m *Manager) List(ctx context.Context) ([]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.load()
}
func (m *Manager) Upsert(ctx context.Context, e Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if containsSecret(e.Content + " " + e.Summary) {
		return errors.New("memory contains a possible secret")
	}
	valid := map[string]bool{"user": true, "project": true, "episodic": true, "procedural": true}
	if !valid[e.Type] {
		return errors.New("invalid memory type")
	}
	if e.ID == "" {
		e.ID = "mem_" + time.Now().UTC().Format("20060102150405.000000000")
	}
	if e.Status == "" {
		e.Status = "active"
	}
	e.UpdatedAt = time.Now().UTC()
	x, err := m.load()
	if err != nil {
		return err
	}
	for i := range x {
		if x[i].ID == e.ID {
			e.Version = x[i].Version + 1
			x[i] = e
			return m.save(x)
		}
	}
	if e.Version == 0 {
		e.Version = 1
	}
	return m.save(append(x, e))
}
func (m *Manager) Delete(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	x, e := m.load()
	if e != nil {
		return e
	}
	for i := range x {
		if x[i].ID == id {
			x[i].Status = "deleted"
			x[i].UpdatedAt = time.Now().UTC()
			return m.save(x)
		}
	}
	return os.ErrNotExist
}
func (m *Manager) Retrieve(ctx context.Context, q string, limit int) ([]Entry, error) {
	x, e := m.List(ctx)
	if e != nil {
		return nil, e
	}
	words := strings.Fields(strings.ToLower(q))
	type scored struct {
		e Entry
		s float64
	}
	var a []scored
	for _, v := range x {
		if v.Status != "active" || (v.ExpiresAt != nil && v.ExpiresAt.Before(time.Now())) {
			continue
		}
		hay := strings.ToLower(v.Summary + " " + v.Content + " " + strings.Join(v.Tags, " "))
		s := v.Importance
		for _, w := range words {
			if strings.Contains(hay, w) {
				s += 1
			}
		}
		if s > 0 {
			a = append(a, scored{v, s})
		}
	}
	sort.Slice(a, func(i, j int) bool { return a[i].s > a[j].s })
	if len(a) > limit {
		a = a[:limit]
	}
	out := make([]Entry, len(a))
	for i := range a {
		out[i] = a[i].e
	}
	return out, nil
}
func containsSecret(s string) bool {
	l := strings.ToLower(s)
	for _, x := range []string{"private key", "password=", "bearer ", "sk-", "api_key", "token="} {
		if strings.Contains(l, x) {
			return true
		}
	}
	return false
}

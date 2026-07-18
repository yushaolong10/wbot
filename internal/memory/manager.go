package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wbot-dev/wbot/internal/config"
	"github.com/wbot-dev/wbot/internal/domain"
	"github.com/wbot-dev/wbot/internal/inference"
	"github.com/wbot-dev/wbot/internal/tokenizer"
	_ "modernc.org/sqlite"
)

type Entry struct {
	ID             string     `json:"id"`
	WorkspaceID    string     `json:"workspace_id,omitempty"`
	UserScope      string     `json:"user_scope,omitempty"`
	ProjectScope   string     `json:"project_scope,omitempty"`
	Type           string     `json:"type"`
	Content        string     `json:"content"`
	Summary        string     `json:"summary"`
	Tags           []string   `json:"tags"`
	Source         Source     `json:"source"`
	Confidence     float64    `json:"confidence"`
	Importance     float64    `json:"importance"`
	Score          float64    `json:"score,omitempty"`
	ConflictIDs    []string   `json:"conflict_ids,omitempty"`
	Status         string     `json:"status"`
	ValidFrom      *time.Time `json:"valid_from,omitempty"`
	ValidUntil     *time.Time `json:"valid_until,omitempty"`
	LastAccessedAt *time.Time `json:"last_accessed_at,omitempty"`
	AccessCount    int        `json:"access_count"`
	Version        int        `json:"version"`
	ContentHash    string     `json:"content_hash"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type Source struct {
	TaskID      string   `json:"task_id"`
	MessageIDs  []string `json:"message_ids,omitempty"`
	ToolCallIDs []string `json:"tool_call_ids,omitempty"`
	ArtifactIDs []string `json:"artifact_ids,omitempty"`
	Extraction  string   `json:"extraction"`
}
type Query struct {
	Text, Objective, LatestUserMessage, CurrentNode, WorkspaceID, UserScope, ProjectScope string
	OpenIssues, DesiredTypes                                                              []string
	MaxEntries, MaxTokens, MaxEntryTokens                                                 int
	MinScore                                                                              float64
}
type Config struct {
	Enabled, AutoExtract, UseFTS, UseLLMRerank, AutoConsolidate, RequireEvidence bool
	MaxEntries, MaxTokens, MaxEntryTokens                                        int
	MinScore, MinConfidence                                                      float64
	EpisodicTTLDays, DeletedRetentionDays, VersionRetentionCount, StaleAfterDays int
	EnablePhysicalGC                                                             bool
}
type Option func(*Manager)

func WithGenerator(g inference.TextGenerator) Option { return func(m *Manager) { m.aux = g } }
func WithConfig(c Config) Option                     { return func(m *Manager) { m.cfg = normalizeConfig(c) } }
func ConfigFrom(c config.MemorySettings) Config {
	return Config{Enabled: c.Enabled, AutoExtract: c.AutoExtract, UseFTS: c.Retrieval.UseFTS, UseLLMRerank: c.Retrieval.UseLLMRerank, AutoConsolidate: c.Write.AutoConsolidate, RequireEvidence: c.Write.RequireEvidence, MaxEntries: c.Retrieval.MaxEntries, MaxTokens: c.Retrieval.MaxTokens, MaxEntryTokens: c.Retrieval.MaxEntryTokens, MinScore: c.Retrieval.MinScore, MinConfidence: c.Write.MinConfidence, EpisodicTTLDays: c.Retention.EpisodicTTLDays, DeletedRetentionDays: c.Retention.DeletedRetentionDays, VersionRetentionCount: c.Retention.VersionRetentionCount, StaleAfterDays: c.Retention.StaleAfterDays, EnablePhysicalGC: c.Retention.EnablePhysicalGC}
}

type Manager struct {
	db  *sql.DB
	aux inference.TextGenerator
	cfg Config
	tok tokenizer.Counter
	mu  sync.Mutex
}

func normalizeConfig(c Config) Config {
	if c.MaxEntries < 1 {
		c.MaxEntries = 8
	}
	if c.MaxTokens < 200 {
		c.MaxTokens = 6000
	}
	if c.MaxEntryTokens < 100 {
		c.MaxEntryTokens = 800
	}
	if c.MinScore <= 0 {
		c.MinScore = .35
	}
	if c.MinConfidence <= 0 {
		c.MinConfidence = .8
	}
	if c.EpisodicTTLDays < 1 {
		c.EpisodicTTLDays = 90
	}
	if c.DeletedRetentionDays < 1 {
		c.DeletedRetentionDays = 30
	}
	if c.VersionRetentionCount < 1 {
		c.VersionRetentionCount = 10
	}
	if c.StaleAfterDays < 1 {
		c.StaleAfterDays = 180
	}
	return c
}

func New(root string, opts ...Option) *Manager {
	_ = os.MkdirAll(root, 0700)
	db, e := sql.Open("sqlite", filepath.Join(root, "memory.db"))
	if e != nil {
		panic(e)
	}
	db.SetMaxOpenConns(1)
	m := &Manager{db: db, cfg: normalizeConfig(Config{Enabled: true, AutoExtract: true, UseFTS: true, UseLLMRerank: true, AutoConsolidate: true, RequireEvidence: true, EnablePhysicalGC: true})}
	for _, o := range opts {
		o(m)
	}
	if e = m.migrate(); e != nil {
		panic(e)
	}
	return m
}

func (m *Manager) Close() error { return m.db.Close() }
func (m *Manager) migrate() error {
	_, e := m.db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS memories(id TEXT PRIMARY KEY,workspace_id TEXT NOT NULL DEFAULT '',user_scope TEXT NOT NULL DEFAULT '',project_scope TEXT NOT NULL DEFAULT '',type TEXT NOT NULL,summary TEXT NOT NULL,content TEXT NOT NULL,tags_json TEXT NOT NULL DEFAULT '[]',source_json TEXT NOT NULL DEFAULT '{}',confidence REAL NOT NULL,importance REAL NOT NULL,status TEXT NOT NULL,valid_from TEXT,valid_until TEXT,last_accessed_at TEXT,access_count INTEGER NOT NULL DEFAULT 0,version INTEGER NOT NULL,content_hash TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS memory_versions(id INTEGER PRIMARY KEY AUTOINCREMENT,memory_id TEXT NOT NULL,version INTEGER NOT NULL,snapshot_json TEXT NOT NULL,change_reason TEXT NOT NULL,source_task_id TEXT,created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS memory_links(memory_id TEXT NOT NULL,relation TEXT NOT NULL,target_memory_id TEXT NOT NULL,PRIMARY KEY(memory_id,relation,target_memory_id));
CREATE INDEX IF NOT EXISTS idx_memories_scope ON memories(workspace_id,user_scope,project_scope,status);
CREATE INDEX IF NOT EXISTS idx_memories_hash ON memories(content_hash,status);
CREATE VIRTUAL TABLE IF NOT EXISTS memory_fts USING fts5(memory_id UNINDEXED,summary,content,tags,tokenize='trigram');`)
	return e
}

func (m *Manager) List(context.Context) ([]Entry, error) {
	rows, e := m.db.Query(`SELECT ` + entryColumns + ` FROM memories ORDER BY updated_at DESC`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		x, e := scanEntry(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

const entryColumns = `id,workspace_id,user_scope,project_scope,type,summary,content,tags_json,source_json,confidence,importance,status,valid_from,valid_until,last_accessed_at,access_count,version,content_hash,created_at,updated_at`

func scanEntry(row interface{ Scan(...any) error }) (Entry, error) {
	var x Entry
	var tags, source string
	var vf, vu, la sql.NullString
	var c, u string
	e := row.Scan(&x.ID, &x.WorkspaceID, &x.UserScope, &x.ProjectScope, &x.Type, &x.Summary, &x.Content, &tags, &source, &x.Confidence, &x.Importance, &x.Status, &vf, &vu, &la, &x.AccessCount, &x.Version, &x.ContentHash, &c, &u)
	if e != nil {
		return x, e
	}
	_ = json.Unmarshal([]byte(tags), &x.Tags)
	_ = json.Unmarshal([]byte(source), &x.Source)
	x.ValidFrom = parseTime(vf)
	x.ValidUntil = parseTime(vu)
	x.LastAccessedAt = parseTime(la)
	x.CreatedAt, _ = time.Parse(time.RFC3339Nano, c)
	x.UpdatedAt, _ = time.Parse(time.RFC3339Nano, u)
	return x, nil
}
func parseTime(s sql.NullString) *time.Time {
	if !s.Valid {
		return nil
	}
	t, e := time.Parse(time.RFC3339Nano, s.String)
	if e != nil {
		return nil
	}
	return &t
}
func timeValue(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func (m *Manager) Upsert(ctx context.Context, e Entry) error {
	if !m.cfg.Enabled {
		return errors.New("memory is disabled")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := validate(e, m.cfg.MinConfidence, m.cfg.RequireEvidence); err != nil {
		return err
	}
	conflictTarget := ""
	supersedesTarget := ""
	if e.ID == "" && m.cfg.AutoConsolidate {
		if target, action := m.findConsolidation(ctx, e); target.ID != "" && action == "merge" {
			e.ID = target.ID
			e = mergeEntry(target, e)
		} else if target.ID != "" && action == "replace" {
			supersedesTarget = target.ID
		} else if target.ID != "" && action == "mark_conflict" {
			conflictTarget = target.ID
		} else if action == "reject" {
			return nil
		}
	}
	now := time.Now().UTC()
	if e.ID == "" {
		e.ID = "mem_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		e.Version = 1
		e.CreatedAt = now
	} else {
		var old Entry
		old, x := m.get(e.ID)
		if x == nil {
			e.Version = old.Version + 1
			e.CreatedAt = old.CreatedAt
		} else if !errors.Is(x, sql.ErrNoRows) {
			return x
		} else {
			e.Version = 1
			e.CreatedAt = now
		}
	}
	if e.Status == "" {
		e.Status = "active"
	}
	if e.Confidence == 0 {
		e.Confidence = .8
	}
	if e.Importance == 0 {
		e.Importance = .7
	}
	if e.Type == "episodic" && e.ValidUntil == nil {
		t := now.AddDate(0, 0, m.cfg.EpisodicTTLDays)
		e.ValidUntil = &t
	}
	e.UpdatedAt = now
	h := sha256.Sum256([]byte(strings.TrimSpace(e.Summary) + "\n" + strings.TrimSpace(e.Content)))
	e.ContentHash = hex.EncodeToString(h[:])
	tags, _ := json.Marshal(e.Tags)
	source, _ := json.Marshal(e.Source)
	snapshot, _ := json.Marshal(e)
	tx, x := m.db.BeginTx(ctx, nil)
	if x != nil {
		return x
	}
	defer tx.Rollback()
	_, x = tx.ExecContext(ctx, `INSERT INTO memory_versions(memory_id,version,snapshot_json,change_reason,source_task_id,created_at) VALUES(?,?,?,?,?,?)`, e.ID, e.Version, string(snapshot), "upsert", e.Source.TaskID, now.Format(time.RFC3339Nano))
	if x != nil {
		return x
	}
	_, x = tx.ExecContext(ctx, `INSERT INTO memories(id,workspace_id,user_scope,project_scope,type,summary,content,tags_json,source_json,confidence,importance,status,valid_from,valid_until,last_accessed_at,access_count,version,content_hash,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET workspace_id=excluded.workspace_id,user_scope=excluded.user_scope,project_scope=excluded.project_scope,type=excluded.type,summary=excluded.summary,content=excluded.content,tags_json=excluded.tags_json,source_json=excluded.source_json,confidence=excluded.confidence,importance=excluded.importance,status=excluded.status,valid_from=excluded.valid_from,valid_until=excluded.valid_until,version=excluded.version,content_hash=excluded.content_hash,updated_at=excluded.updated_at`, e.ID, e.WorkspaceID, e.UserScope, e.ProjectScope, e.Type, e.Summary, e.Content, string(tags), string(source), e.Confidence, e.Importance, e.Status, timeValue(e.ValidFrom), timeValue(e.ValidUntil), timeValue(e.LastAccessedAt), e.AccessCount, e.Version, e.ContentHash, e.CreatedAt.Format(time.RFC3339Nano), e.UpdatedAt.Format(time.RFC3339Nano))
	if x != nil {
		return x
	}
	if _, x = tx.ExecContext(ctx, "DELETE FROM memory_fts WHERE memory_id=?", e.ID); x != nil {
		return x
	}
	if e.Status == "active" {
		_, x = tx.ExecContext(ctx, "INSERT INTO memory_fts(memory_id,summary,content,tags) VALUES(?,?,?,?)", e.ID, e.Summary, e.Content, strings.Join(e.Tags, " "))
		if x != nil {
			return x
		}
	}
	if conflictTarget != "" {
		if _, x = tx.ExecContext(ctx, "INSERT OR IGNORE INTO memory_links(memory_id,relation,target_memory_id) VALUES(?,?,?)", e.ID, "conflicts_with", conflictTarget); x != nil {
			return x
		}
		if _, x = tx.ExecContext(ctx, "INSERT OR IGNORE INTO memory_links(memory_id,relation,target_memory_id) VALUES(?,?,?)", conflictTarget, "conflicts_with", e.ID); x != nil {
			return x
		}
	}
	if supersedesTarget != "" {
		var old Entry
		old, x = scanEntry(tx.QueryRowContext(ctx, `SELECT `+entryColumns+` FROM memories WHERE id=? AND status='active'`, supersedesTarget))
		if x != nil {
			return x
		}
		old.Status = "archived"
		old.ValidUntil = &now
		old.Version++
		old.UpdatedAt = now
		oldSnapshot, _ := json.Marshal(old)
		if _, x = tx.ExecContext(ctx, `INSERT INTO memory_versions(memory_id,version,snapshot_json,change_reason,source_task_id,created_at) VALUES(?,?,?,?,?,?)`, old.ID, old.Version, string(oldSnapshot), "superseded", e.Source.TaskID, now.Format(time.RFC3339Nano)); x != nil {
			return x
		}
		if _, x = tx.ExecContext(ctx, `UPDATE memories SET status='archived',valid_until=?,version=?,updated_at=? WHERE id=?`, now.Format(time.RFC3339Nano), old.Version, now.Format(time.RFC3339Nano), old.ID); x != nil {
			return x
		}
		if _, x = tx.ExecContext(ctx, `DELETE FROM memory_fts WHERE memory_id=?`, old.ID); x != nil {
			return x
		}
		if _, x = tx.ExecContext(ctx, `INSERT OR IGNORE INTO memory_links(memory_id,relation,target_memory_id) VALUES(?,?,?)`, e.ID, "supersedes", old.ID); x != nil {
			return x
		}
	}
	return tx.Commit()
}

func (m *Manager) get(id string) (Entry, error) {
	return scanEntry(m.db.QueryRow(`SELECT `+entryColumns+` FROM memories WHERE id=?`, id))
}
func (m *Manager) Delete(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	tx, e := m.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	old, e := scanEntry(tx.QueryRowContext(ctx, `SELECT `+entryColumns+` FROM memories WHERE id=?`, id))
	if errors.Is(e, sql.ErrNoRows) {
		return os.ErrNotExist
	}
	if e != nil {
		return e
	}
	now := time.Now().UTC()
	old.Status, old.Version, old.UpdatedAt = "deleted", old.Version+1, now
	snapshot, _ := json.Marshal(old)
	if _, e = tx.ExecContext(ctx, `INSERT INTO memory_versions(memory_id,version,snapshot_json,change_reason,source_task_id,created_at) VALUES(?,?,?,?,?,?)`, id, old.Version, string(snapshot), "deleted", old.Source.TaskID, now.Format(time.RFC3339Nano)); e != nil {
		return e
	}
	r, e := tx.ExecContext(ctx, "UPDATE memories SET status='deleted',version=?,updated_at=? WHERE id=?", old.Version, now.Format(time.RFC3339Nano), id)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return os.ErrNotExist
	}
	if _, e = tx.ExecContext(ctx, "DELETE FROM memory_fts WHERE memory_id=?", id); e != nil {
		return e
	}
	return tx.Commit()
}

func (m *Manager) Retrieve(ctx context.Context, q string, limit int) ([]Entry, error) {
	return m.RetrieveQuery(ctx, Query{Text: q, MaxEntries: limit})
}
func (m *Manager) RetrieveQuery(ctx context.Context, q Query) ([]Entry, error) {
	if !m.cfg.Enabled {
		return nil, nil
	}
	if q.MaxEntries < 1 {
		q.MaxEntries = m.cfg.MaxEntries
	}
	if q.MaxTokens < 1 {
		q.MaxTokens = m.cfg.MaxTokens
	}
	if q.MaxEntryTokens < 1 {
		q.MaxEntryTokens = m.cfg.MaxEntryTokens
	}
	if q.MinScore <= 0 {
		q.MinScore = m.cfg.MinScore
	}
	text := strings.TrimSpace(strings.Join([]string{q.Text, q.Objective, q.LatestUserMessage, q.CurrentNode, strings.Join(q.OpenIssues, " ")}, " "))
	if text == "" {
		return nil, nil
	}
	var candidates []Entry
	var e error
	if m.cfg.UseFTS {
		candidates, e = m.searchFTS(ctx, text, q, 40)
	}
	if !m.cfg.UseFTS || e != nil || len(candidates) == 0 {
		candidates, e = m.searchLike(ctx, text, q, 40)
	}
	if e != nil {
		return nil, e
	}
	candidates = filterCandidates(candidates, q)
	m.attachConflicts(candidates)
	scoreEntries(candidates, text)
	if m.cfg.UseLLMRerank && m.aux != nil && len(candidates) > 8 {
		candidates = m.rerank(ctx, text, candidates)
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Score > candidates[j].Score })
	var out []Entry
	tokens := 0
	for _, x := range candidates {
		if x.Score < q.MinScore {
			continue
		}
		n := m.tok.CountString(x.Summary) + m.tok.CountString(x.Content)
		if n > q.MaxEntryTokens {
			x.Content = excerpt(x.Content, q.MaxEntryTokens*3)
			n = m.tok.CountString(x.Summary) + m.tok.CountString(x.Content)
		}
		if tokens+n > q.MaxTokens {
			continue
		}
		tokens += n
		out = append(out, x)
		if len(out) >= q.MaxEntries {
			break
		}
	}
	if len(out) > 0 {
		ids := make([]string, len(out))
		for i := range out {
			ids[i] = out[i].ID
		}
		m.recordAccess(ids)
	}
	return out, nil
}

func (m *Manager) searchFTS(ctx context.Context, text string, q Query, limit int) ([]Entry, error) {
	matchTerms := terms(text)
	parts := make([]string, 0, len(matchTerms))
	for _, term := range matchTerms {
		term = strings.ReplaceAll(term, `"`, `""`)
		if term != "" {
			parts = append(parts, `"`+term+`"`)
		}
	}
	match := strings.Join(parts, " OR ")
	if match == "" {
		return nil, nil
	}
	rows, e := m.db.QueryContext(ctx, `SELECT `+prefixColumns("m")+` FROM memory_fts f JOIN memories m ON m.id=f.memory_id WHERE memory_fts MATCH ? AND m.status='active' AND (m.valid_until IS NULL OR m.valid_until>?) AND (?='' OR m.workspace_id='' OR m.workspace_id=?) AND (?='' OR m.user_scope='' OR m.user_scope=?) AND (?='' OR m.project_scope='' OR m.project_scope=?) ORDER BY bm25(memory_fts) LIMIT ?`, match, time.Now().UTC().Format(time.RFC3339Nano), q.WorkspaceID, q.WorkspaceID, q.UserScope, q.UserScope, q.ProjectScope, q.ProjectScope, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	return collectEntries(rows)
}
func prefixColumns(p string) string {
	parts := strings.Split(entryColumns, ",")
	for i := range parts {
		parts[i] = p + "." + parts[i]
	}
	return strings.Join(parts, ",")
}
func (m *Manager) searchLike(ctx context.Context, text string, q Query, limit int) ([]Entry, error) {
	queryTerms := terms(text)
	if len(queryTerms) > 12 {
		queryTerms = queryTerms[:12]
	}
	if len(queryTerms) == 0 {
		return nil, nil
	}
	clauses := make([]string, 0, len(queryTerms))
	args := []any{time.Now().UTC().Format(time.RFC3339Nano), q.WorkspaceID, q.WorkspaceID, q.UserScope, q.UserScope, q.ProjectScope, q.ProjectScope}
	for _, term := range queryTerms {
		clauses = append(clauses, "(summary LIKE ? OR content LIKE ? OR tags_json LIKE ?)")
		pattern := "%" + strings.NewReplacer("%", "", "_", "").Replace(term) + "%"
		args = append(args, pattern, pattern, pattern)
	}
	args = append(args, limit)
	query := `SELECT ` + entryColumns + ` FROM memories WHERE status='active' AND (valid_until IS NULL OR valid_until>?) AND (?='' OR workspace_id='' OR workspace_id=?) AND (?='' OR user_scope='' OR user_scope=?) AND (?='' OR project_scope='' OR project_scope=?) AND (` + strings.Join(clauses, " OR ") + `) LIMIT ?`
	rows, e := m.db.QueryContext(ctx, query, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	return collectEntries(rows)
}
func collectEntries(rows *sql.Rows) ([]Entry, error) {
	var out []Entry
	for rows.Next() {
		x, e := scanEntry(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
func scoreEntries(xs []Entry, q string) {
	terms := terms(q)
	for i := range xs {
		s := .15*xs[i].Importance + .1*xs[i].Confidence
		hay := strings.ToLower(xs[i].Summary + " " + xs[i].Content + " " + strings.Join(xs[i].Tags, " "))
		hits := 0
		for _, t := range terms {
			if strings.Contains(hay, t) {
				hits++
			}
		}
		if len(terms) > 0 {
			s += .65 * float64(hits) / float64(len(terms))
		}
		if hits == 0 {
			s -= .3
		}
		if xs[i].ValidUntil != nil {
			s -= .05
		}
		if len(xs[i].ConflictIDs) > 0 {
			s -= .2
		}
		xs[i].Score = math.Max(0, s)
	}
}

func filterCandidates(xs []Entry, q Query) []Entry {
	types := map[string]bool{}
	for _, typ := range q.DesiredTypes {
		types[typ] = true
	}
	out := xs[:0]
	for _, x := range xs {
		if q.ProjectScope != "" && x.ProjectScope != "" && x.ProjectScope != q.ProjectScope {
			continue
		}
		if len(types) > 0 && !types[x.Type] {
			continue
		}
		out = append(out, x)
	}
	return out
}

func (m *Manager) attachConflicts(xs []Entry) {
	for i := range xs {
		rows, err := m.db.Query(`SELECT target_memory_id FROM memory_links WHERE memory_id=? AND relation='conflicts_with'`, xs[i].ID)
		if err != nil {
			continue
		}
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				xs[i].ConflictIDs = append(xs[i].ConflictIDs, id)
			}
		}
		rows.Close()
	}
}
func terms(q string) []string {
	q = strings.ToLower(strings.TrimSpace(q))
	var out []string
	for _, word := range strings.Fields(q) {
		r := []rune(word)
		if len(r) <= 3 {
			out = append(out, word)
			continue
		}
		for i := 0; i+3 <= len(r); i += 2 {
			out = append(out, string(r[i:i+3]))
		}
	}
	if len(out) == 0 && q != "" {
		out = append(out, q)
	}
	return out
}

func (m *Manager) rerank(ctx context.Context, q string, xs []Entry) []Entry {
	view := make([]map[string]any, len(xs))
	for i, x := range xs {
		view[i] = map[string]any{"id": x.ID, "summary": x.Summary, "type": x.Type, "score": x.Score}
	}
	b, _ := json.Marshal(map[string]any{"query": q, "candidates": view})
	auxCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	out, e := m.aux.Complete(auxCtx, `按相关性重排记忆。只输出 {"selected":[{"id":"...","relevance":0.0}]}，只能选择输入 ID。`, string(b))
	cancel()
	if e != nil {
		return xs
	}
	var v struct {
		Selected []struct {
			ID        string  `json:"id"`
			Relevance float64 `json:"relevance"`
		} `json:"selected"`
	}
	if json.Unmarshal(extractJSON(out), &v) != nil {
		return xs
	}
	scores := map[string]float64{}
	for _, s := range v.Selected {
		scores[s.ID] = s.Relevance
	}
	for i := range xs {
		if s, ok := scores[xs[i].ID]; ok {
			xs[i].Score = .5*xs[i].Score + .5*s
		}
	}
	return xs
}
func (m *Manager) recordAccess(ids []string) {
	if len(ids) == 0 {
		return
	}
	tx, e := m.db.Begin()
	if e != nil {
		return
	}
	defer tx.Rollback()
	for _, id := range ids {
		_, _ = tx.Exec("UPDATE memories SET access_count=access_count+1,last_accessed_at=? WHERE id=?", time.Now().UTC().Format(time.RFC3339Nano), id)
	}
	_ = tx.Commit()
}

func (m *Manager) findConsolidation(ctx context.Context, e Entry) (Entry, string) {
	h := sha256.Sum256([]byte(strings.TrimSpace(e.Summary) + "\n" + strings.TrimSpace(e.Content)))
	var x Entry
	if got, err := scanEntry(m.db.QueryRow(`SELECT `+entryColumns+` FROM memories WHERE content_hash=? AND status='active' LIMIT 1`, hex.EncodeToString(h[:]))); err == nil {
		return got, "merge"
	}
	cands, err := m.RetrieveQuery(ctx, Query{Text: e.Summary + " " + strings.Join(e.Tags, " "), WorkspaceID: e.WorkspaceID, UserScope: e.UserScope, MaxEntries: 3, MaxTokens: 2000, MinScore: .2})
	if err != nil || len(cands) == 0 {
		return x, "create"
	}
	if m.aux == nil {
		return cands[0], "create"
	}
	b, _ := json.Marshal(map[string]any{"candidate": e, "existing": cands})
	out, err := m.aux.Complete(ctx, `判断候选记忆与已有记忆关系。只输出 {"action":"create|merge|replace|keep_both|mark_conflict|reject","target_id":"..."}。同义事实 merge，新事实取代旧事实 replace，冲突 mark_conflict。`, string(b))
	if err != nil {
		return x, "create"
	}
	var d struct {
		Action   string `json:"action"`
		TargetID string `json:"target_id"`
	}
	if json.Unmarshal(extractJSON(out), &d) != nil {
		return x, "create"
	}
	for _, c := range cands {
		if c.ID == d.TargetID {
			return c, d.Action
		}
	}
	return x, "create"
}
func mergeEntry(old, n Entry) Entry {
	n.ID = old.ID
	n.Version = old.Version
	n.CreatedAt = old.CreatedAt
	n.Tags = uniq(append(old.Tags, n.Tags...))
	n.Source.MessageIDs = uniq(append(old.Source.MessageIDs, n.Source.MessageIDs...))
	n.Source.ToolCallIDs = uniq(append(old.Source.ToolCallIDs, n.Source.ToolCallIDs...))
	if len(n.Content) < len(old.Content) {
		n.Content = old.Content
	}
	if n.Confidence < old.Confidence {
		n.Confidence = old.Confidence
	}
	if n.Importance < old.Importance {
		n.Importance = old.Importance
	}
	return n
}

func (m *Manager) Extract(ctx context.Context, task domain.Task, msgs []domain.Message, scope ...string) error {
	if !m.cfg.Enabled || !m.cfg.AutoExtract || m.aux == nil {
		return nil
	}
	view := map[string]any{"task": task, "messages": msgs}
	b, _ := json.Marshal(view)
	out, e := m.aux.Complete(ctx, `从已完成任务提取未来可复用的长期记忆。只输出 {"candidates":[{"type":"user|project|episodic|procedural","summary":"","content":"","tags":[],"confidence":0.0,"importance":0.0,"evidence_message_ids":[]}]}。不提取寒暄、临时日志、密钥、未验证推测和可随时读取的长原文。`, string(b))
	if e != nil {
		return e
	}
	var v struct {
		Candidates []struct {
			Type               string   `json:"type"`
			Summary            string   `json:"summary"`
			Content            string   `json:"content"`
			Tags               []string `json:"tags"`
			EvidenceMessageIDs []string `json:"evidence_message_ids"`
			Confidence         float64  `json:"confidence"`
			Importance         float64  `json:"importance"`
		} `json:"candidates"`
	}
	if e = json.Unmarshal(extractJSON(out), &v); e != nil {
		return e
	}
	validIDs := map[string]bool{}
	messageText := map[string]string{}
	for _, x := range msgs {
		validIDs[x.ID] = true
		messageText[x.ID] = x.Content + " " + string(x.ContentJSON)
	}
	var writeErrors []error
	for _, c := range v.Candidates {
		var evidence []string
		for _, id := range c.EvidenceMessageIDs {
			if validIDs[id] {
				evidence = append(evidence, id)
			}
		}
		if len(evidence) == 0 {
			continue
		}
		var evidenceBody strings.Builder
		for _, id := range evidence {
			evidenceBody.WriteString(messageText[id])
			evidenceBody.WriteByte('\n')
		}
		if !evidenceSupports(c.Summary+" "+c.Content, evidenceBody.String()) {
			continue
		}
		workspace := ""
		if len(scope) > 0 {
			workspace = scope[0]
		}
		x := Entry{WorkspaceID: workspace, Type: c.Type, Summary: c.Summary, Content: c.Content, Tags: c.Tags, Confidence: c.Confidence, Importance: c.Importance, Source: Source{TaskID: task.ID, MessageIDs: evidence, Extraction: "automatic"}}
		if err := m.Upsert(ctx, x); err != nil {
			writeErrors = append(writeErrors, err)
		}
	}
	return errors.Join(writeErrors...)
}

func (m *Manager) Maintain(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	stamp := now.Format(time.RFC3339Nano)
	stale := now.AddDate(0, 0, -m.cfg.StaleAfterDays).Format(time.RFC3339Nano)
	rows, e := m.db.QueryContext(ctx, `SELECT `+entryColumns+` FROM memories WHERE status='active' AND ((type='episodic' AND valid_until IS NOT NULL AND valid_until<?) OR (type IN ('episodic','procedural') AND importance<0.35 AND COALESCE(last_accessed_at,updated_at)<?))`, stamp, stale)
	if e != nil {
		return e
	}
	var archive []Entry
	for rows.Next() {
		x, err := scanEntry(rows)
		if err != nil {
			rows.Close()
			return err
		}
		archive = append(archive, x)
	}
	if e = rows.Close(); e != nil {
		return e
	}
	rows, e = m.db.QueryContext(ctx, `SELECT `+entryColumns+` FROM memories WHERE status='active' ORDER BY content_hash,updated_at DESC`)
	if e != nil {
		return e
	}
	keepers := map[string]string{}
	duplicates := map[string]string{}
	for rows.Next() {
		x, err := scanEntry(rows)
		if err != nil {
			rows.Close()
			return err
		}
		if keep := keepers[x.ContentHash]; keep != "" {
			duplicates[x.ID] = keep
			archive = append(archive, x)
		} else {
			keepers[x.ContentHash] = x.ID
		}
	}
	if e = rows.Close(); e != nil {
		return e
	}
	tx, e := m.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	seenArchive := map[string]bool{}
	for _, old := range archive {
		if seenArchive[old.ID] {
			continue
		}
		seenArchive[old.ID] = true
		old.Status, old.Version, old.UpdatedAt = "archived", old.Version+1, now
		reason := "retention_archive"
		if duplicates[old.ID] != "" {
			reason = "duplicate_archive"
		}
		snapshot, _ := json.Marshal(old)
		if _, e = tx.ExecContext(ctx, `INSERT INTO memory_versions(memory_id,version,snapshot_json,change_reason,source_task_id,created_at) VALUES(?,?,?,?,?,?)`, old.ID, old.Version, string(snapshot), reason, old.Source.TaskID, stamp); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE memories SET status='archived',version=?,updated_at=? WHERE id=? AND status='active'`, old.Version, stamp, old.ID); e != nil {
			return e
		}
		if target := duplicates[old.ID]; target != "" {
			if _, e = tx.ExecContext(ctx, `INSERT OR IGNORE INTO memory_links(memory_id,relation,target_memory_id) VALUES(?,?,?)`, old.ID, "duplicates", target); e != nil {
				return e
			}
		}
	}
	if _, e = tx.ExecContext(ctx, "DELETE FROM memory_fts"); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO memory_fts(memory_id,summary,content,tags) SELECT id,summary,content,replace(replace(tags_json,'[',''),']','') FROM memories WHERE status='active' AND (valid_until IS NULL OR valid_until>?)", stamp); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, `DELETE FROM memory_versions WHERE id IN (SELECT id FROM (SELECT id,ROW_NUMBER() OVER (PARTITION BY memory_id ORDER BY version DESC,id DESC) AS rn FROM memory_versions) WHERE rn>?)`, m.cfg.VersionRetentionCount); e != nil {
		return e
	}
	if m.cfg.EnablePhysicalGC {
		cutoff := now.AddDate(0, 0, -m.cfg.DeletedRetentionDays).Format(time.RFC3339Nano)
		if _, e = tx.ExecContext(ctx, `DELETE FROM memory_links WHERE memory_id IN (SELECT id FROM memories WHERE status='deleted' AND updated_at<?) OR target_memory_id IN (SELECT id FROM memories WHERE status='deleted' AND updated_at<?)`, cutoff, cutoff); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `DELETE FROM memory_versions WHERE memory_id IN (SELECT id FROM memories WHERE status='deleted' AND updated_at<?)`, cutoff); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `DELETE FROM memories WHERE status='deleted' AND updated_at<?`, cutoff); e != nil {
			return e
		}
	}
	return tx.Commit()
}

var secretPatterns = []*regexp.Regexp{regexp.MustCompile(`(?i)-----BEGIN [A-Z ]*PRIVATE KEY-----`), regexp.MustCompile(`(?i)bearer\s+[a-z0-9._~+/-]+`), regexp.MustCompile(`(?i)(password|api[_-]?key|token)\s*[:=]\s*\S+`), regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}`)}
var highEntropyCandidate = regexp.MustCompile(`[A-Za-z0-9_+/=-]{40,}`)

func validate(e Entry, minConfidence float64, requireEvidence bool) error {
	valid := map[string]bool{"user": true, "project": true, "episodic": true, "procedural": true}
	if !valid[e.Type] {
		return errors.New("invalid memory type")
	}
	if strings.TrimSpace(e.Summary) == "" || strings.TrimSpace(e.Content) == "" {
		return errors.New("summary and content are required")
	}
	for _, r := range secretPatterns {
		if r.MatchString(e.Summary + " " + e.Content) {
			return errors.New("memory contains a possible secret")
		}
	}
	for _, candidate := range highEntropyCandidate.FindAllString(e.Summary+" "+e.Content, -1) {
		if shannonEntropy(candidate) >= 4.2 {
			return errors.New("memory contains a high-entropy secret candidate")
		}
	}
	if regexp.MustCompile(`(?i)(ignore|disregard).{0,24}(system|previous|instruction)|忽略.{0,12}(系统|之前|指令)`).MatchString(e.Summary + " " + e.Content) {
		return errors.New("memory contains prompt-injection style instructions")
	}
	if e.Source.Extraction == "automatic" && e.Confidence < minConfidence {
		return fmt.Errorf("memory confidence %.2f is below %.2f", e.Confidence, minConfidence)
	}
	if requireEvidence && e.Source.Extraction == "automatic" && len(e.Source.MessageIDs)+len(e.Source.ToolCallIDs)+len(e.Source.ArtifactIDs) == 0 {
		return errors.New("automatic memory requires evidence")
	}
	return nil
}

func shannonEntropy(s string) float64 {
	if s == "" {
		return 0
	}
	counts := map[rune]int{}
	for _, r := range s {
		counts[r]++
	}
	n := float64(len([]rune(s)))
	entropy := 0.0
	for _, count := range counts {
		p := float64(count) / n
		entropy -= p * math.Log2(p)
	}
	return entropy
}

func evidenceSupports(claim, evidence string) bool {
	claimTerms := terms(claim)
	if len(claimTerms) == 0 {
		return false
	}
	evidence = strings.ToLower(evidence)
	hits := 0
	for _, term := range claimTerms {
		if strings.Contains(evidence, term) {
			hits++
		}
	}
	return hits >= 1 && float64(hits)/float64(len(claimTerms)) >= .15
}
func excerpt(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n/2]) + "…" + string(r[len(r)-n/2:])
}
func uniq(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if x != "" && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
func extractJSON(s string) []byte {
	i := strings.Index(s, "{")
	j := strings.LastIndex(s, "}")
	if i >= 0 && j > i {
		return []byte(s[i : j+1])
	}
	return []byte(s)
}

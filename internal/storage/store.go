package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wbot-dev/wbot/internal/domain"
	_ "modernc.org/sqlite"
)

type Store struct {
	db           *sql.DB
	artifactRoot string
	subscribers  map[string]map[chan domain.Event]struct{}
	mu           sync.Mutex
}

func Open(path, dataRoot string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, artifactRoot: filepath.Join(dataRoot, "artifacts"), subscribers: map[string]map[chan domain.Event]struct{}{}}
	if err = s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) migrate() error {
	_, err := s.db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;
	CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY,applied_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS workspaces(id TEXT PRIMARY KEY,name TEXT NOT NULL,type TEXT NOT NULL DEFAULT 'local',path TEXT NOT NULL DEFAULT '',root TEXT NOT NULL,kind TEXT NOT NULL,created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sessions(id TEXT PRIMARY KEY,workspace_id TEXT NOT NULL,title TEXT NOT NULL,status TEXT NOT NULL DEFAULT 'active',created_at TEXT NOT NULL,updated_at TEXT NOT NULL,FOREIGN KEY(workspace_id) REFERENCES workspaces(id));
CREATE TABLE IF NOT EXISTS messages(id TEXT PRIMARY KEY,session_id TEXT NOT NULL,task_id TEXT,role TEXT NOT NULL,content TEXT NOT NULL,metadata TEXT,created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS tasks(id TEXT PRIMARY KEY,session_id TEXT NOT NULL,objective TEXT NOT NULL,status TEXT NOT NULL,result TEXT NOT NULL DEFAULT '',error TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL,updated_at TEXT NOT NULL,completed_at TEXT);
CREATE TABLE IF NOT EXISTS task_nodes(id TEXT PRIMARY KEY,task_id TEXT NOT NULL,title TEXT NOT NULL,description TEXT NOT NULL,status TEXT NOT NULL,depends_on TEXT NOT NULL,risk_level TEXT NOT NULL,attempt INTEGER NOT NULL,max_attempts INTEGER NOT NULL,result TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS task_checkpoints(id INTEGER PRIMARY KEY AUTOINCREMENT,task_id TEXT NOT NULL,state TEXT NOT NULL,created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS task_criteria(task_id TEXT NOT NULL,criterion TEXT NOT NULL,passed INTEGER NOT NULL DEFAULT 0,reason TEXT NOT NULL DEFAULT '',PRIMARY KEY(task_id,criterion));
CREATE TABLE IF NOT EXISTS message_summaries(id INTEGER PRIMARY KEY AUTOINCREMENT,session_id TEXT NOT NULL,first_message_id TEXT,last_message_id TEXT,summary TEXT NOT NULL,created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS approvals(id TEXT PRIMARY KEY,task_id TEXT NOT NULL,session_id TEXT NOT NULL DEFAULT '',node_id TEXT,tool_name TEXT NOT NULL,tool_call_id TEXT NOT NULL DEFAULT '',arguments TEXT NOT NULL,arguments_digest TEXT NOT NULL,risk_level TEXT NOT NULL,reason TEXT NOT NULL,status TEXT NOT NULL,created_at TEXT NOT NULL,decided_at TEXT);
CREATE TABLE IF NOT EXISTS tool_calls(id TEXT PRIMARY KEY,task_id TEXT NOT NULL,node_id TEXT,name TEXT NOT NULL,arguments_digest TEXT NOT NULL,status TEXT NOT NULL,result TEXT,created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS artifacts(id TEXT PRIMARY KEY,task_id TEXT,kind TEXT NOT NULL DEFAULT '',mime_type TEXT NOT NULL,path TEXT NOT NULL,size INTEGER NOT NULL,sha256 TEXT NOT NULL,created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS events(id INTEGER PRIMARY KEY AUTOINCREMENT,trace_id TEXT NOT NULL DEFAULT '',session_id TEXT NOT NULL,task_id TEXT,node_id TEXT NOT NULL DEFAULT '',type TEXT NOT NULL,payload TEXT NOT NULL,created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS model_usage(id INTEGER PRIMARY KEY AUTOINCREMENT,task_id TEXT NOT NULL,model TEXT NOT NULL,role TEXT NOT NULL,input_tokens INTEGER NOT NULL DEFAULT 0,output_tokens INTEGER NOT NULL DEFAULT 0,total_tokens INTEGER NOT NULL DEFAULT 0,prompt_tokens INTEGER NOT NULL DEFAULT 0,completion_tokens INTEGER NOT NULL DEFAULT 0,duration_ms INTEGER NOT NULL DEFAULT 0,created_at TEXT NOT NULL);
	CREATE INDEX IF NOT EXISTS idx_events_session ON events(session_id,id); CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status);`)
	if err != nil {
		return err
	}
	return s.migrateLegacy()
}

type columnMigration struct{ table, column, definition string }

func (s *Store) migrateLegacy() error {
	migrations := []columnMigration{
		{"workspaces", "name", "TEXT NOT NULL DEFAULT ''"}, {"workspaces", "type", "TEXT NOT NULL DEFAULT 'local'"}, {"workspaces", "path", "TEXT NOT NULL DEFAULT ''"}, {"workspaces", "root", "TEXT NOT NULL DEFAULT ''"}, {"workspaces", "kind", "TEXT NOT NULL DEFAULT 'local'"}, {"workspaces", "created_at", "TEXT NOT NULL DEFAULT ''"},
		{"sessions", "workspace_id", "TEXT NOT NULL DEFAULT ''"}, {"sessions", "title", "TEXT NOT NULL DEFAULT ''"}, {"sessions", "created_at", "TEXT NOT NULL DEFAULT ''"},
		{"sessions", "status", "TEXT NOT NULL DEFAULT 'active'"}, {"sessions", "updated_at", "TEXT NOT NULL DEFAULT ''"},
		{"messages", "task_id", "TEXT"}, {"messages", "metadata", "TEXT"},
		{"tasks", "result", "TEXT NOT NULL DEFAULT ''"}, {"tasks", "error", "TEXT NOT NULL DEFAULT ''"}, {"tasks", "updated_at", "TEXT NOT NULL DEFAULT ''"}, {"tasks", "completed_at", "TEXT"},
		{"task_nodes", "description", "TEXT NOT NULL DEFAULT ''"}, {"task_nodes", "depends_on", "TEXT NOT NULL DEFAULT '[]'"}, {"task_nodes", "risk_level", "TEXT NOT NULL DEFAULT 'low'"}, {"task_nodes", "attempt", "INTEGER NOT NULL DEFAULT 0"}, {"task_nodes", "max_attempts", "INTEGER NOT NULL DEFAULT 2"}, {"task_nodes", "result", "TEXT NOT NULL DEFAULT ''"}, {"task_nodes", "created_at", "TEXT NOT NULL DEFAULT ''"}, {"task_nodes", "updated_at", "TEXT NOT NULL DEFAULT ''"},
		{"approvals", "session_id", "TEXT NOT NULL DEFAULT ''"}, {"approvals", "node_id", "TEXT"}, {"approvals", "tool_call_id", "TEXT NOT NULL DEFAULT ''"}, {"approvals", "arguments_digest", "TEXT NOT NULL DEFAULT ''"}, {"approvals", "risk_level", "TEXT NOT NULL DEFAULT 'L2'"}, {"approvals", "reason", "TEXT NOT NULL DEFAULT ''"}, {"approvals", "decided_at", "TEXT"},
		{"events", "trace_id", "TEXT NOT NULL DEFAULT ''"}, {"events", "task_id", "TEXT"}, {"events", "node_id", "TEXT NOT NULL DEFAULT ''"},
		{"artifacts", "task_id", "TEXT"}, {"artifacts", "kind", "TEXT NOT NULL DEFAULT ''"}, {"artifacts", "mime_type", "TEXT NOT NULL DEFAULT 'application/octet-stream'"},
		{"model_usage", "role", "TEXT NOT NULL DEFAULT ''"}, {"model_usage", "input_tokens", "INTEGER NOT NULL DEFAULT 0"}, {"model_usage", "output_tokens", "INTEGER NOT NULL DEFAULT 0"}, {"model_usage", "total_tokens", "INTEGER NOT NULL DEFAULT 0"}, {"model_usage", "prompt_tokens", "INTEGER NOT NULL DEFAULT 0"}, {"model_usage", "completion_tokens", "INTEGER NOT NULL DEFAULT 0"}, {"model_usage", "duration_ms", "INTEGER NOT NULL DEFAULT 0"},
	}
	for _, m := range migrations {
		ok, e := s.hasColumn(m.table, m.column)
		if e != nil {
			return e
		}
		if !ok {
			if _, e = s.db.Exec("ALTER TABLE " + m.table + " ADD COLUMN " + m.column + " " + m.definition); e != nil {
				return fmt.Errorf("migrate %s.%s: %w", m.table, m.column, e)
			}
		}
	}
	hasPath, e := s.hasColumn("workspaces", "path")
	if e != nil {
		return e
	}
	if hasPath {
		if _, e = s.db.Exec("UPDATE workspaces SET root=path WHERE root='' OR root IS NULL"); e != nil {
			return e
		}
	}
	_, e = s.db.Exec("UPDATE workspaces SET name=CASE WHEN name='' THEN root ELSE name END, kind=CASE WHEN kind='' THEN 'local' ELSE kind END, created_at=CASE WHEN created_at='' THEN ? ELSE created_at END", now())
	if e != nil {
		return e
	}
	_, e = s.db.Exec("UPDATE workspaces SET path=root WHERE path='' OR path IS NULL; UPDATE workspaces SET type=kind WHERE type='' OR type IS NULL; UPDATE task_nodes SET description=title WHERE description='' OR description IS NULL")
	if e != nil {
		return e
	}
	_, e = s.db.Exec("INSERT OR IGNORE INTO schema_migrations(version,applied_at) VALUES(2,?)", now())
	return e
}
func (s *Store) hasColumn(table, column string) (bool, error) {
	rows, e := s.db.Query("PRAGMA table_info(" + table + ")")
	if e != nil {
		return false, e
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var def any
		if e = rows.Scan(&cid, &name, &typ, &notnull, &def, &pk); e != nil {
			return false, e
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}
func now() string                { return time.Now().UTC().Format(time.RFC3339Nano) }
func id(prefix string) string    { return prefix + "_" + strings.ReplaceAll(uuid.NewString(), "-", "") }
func NewID(prefix string) string { return id(prefix) }
func (s *Store) OpenWorkspace(ctx context.Context, name, root, kind string) (domain.Workspace, error) {
	w := domain.Workspace{ID: id("ws"), Name: name, Root: root, Kind: kind, CreatedAt: time.Now().UTC()}
	_, e := s.db.ExecContext(ctx, "INSERT INTO workspaces(id,name,type,path,root,kind,created_at) VALUES(?,?,?,?,?,?,?)", w.ID, w.Name, w.Kind, w.Root, w.Root, w.Kind, w.CreatedAt.Format(time.RFC3339Nano))
	return w, e
}
func (s *Store) Workspaces(ctx context.Context) ([]domain.Workspace, error) {
	rows, e := s.db.QueryContext(ctx, "SELECT id,name,root,kind,created_at FROM workspaces ORDER BY created_at")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := make([]domain.Workspace, 0)
	for rows.Next() {
		var x domain.Workspace
		var t string
		rows.Scan(&x.ID, &x.Name, &x.Root, &x.Kind, &t)
		x.CreatedAt, _ = time.Parse(time.RFC3339Nano, t)
		out = append(out, x)
	}
	return out, rows.Err()
}
func (s *Store) CreateSession(ctx context.Context, wid, title string) (domain.Session, error) {
	x := domain.Session{ID: id("session"), WorkspaceID: wid, Title: title, CreatedAt: time.Now().UTC()}
	ts := x.CreatedAt.Format(time.RFC3339Nano)
	_, e := s.db.ExecContext(ctx, "INSERT INTO sessions(id,workspace_id,title,status,created_at,updated_at) VALUES(?,?,?,?,?,?)", x.ID, x.WorkspaceID, x.Title, "active", ts, ts)
	return x, e
}
func (s *Store) Session(ctx context.Context, sid string) (domain.Session, error) {
	var x domain.Session
	var t string
	e := s.db.QueryRowContext(ctx, "SELECT id,workspace_id,title,created_at FROM sessions WHERE id=?", sid).Scan(&x.ID, &x.WorkspaceID, &x.Title, &t)
	x.CreatedAt, _ = time.Parse(time.RFC3339Nano, t)
	return x, e
}
func (s *Store) AddMessage(ctx context.Context, sid, tid, role, content string) (domain.Message, error) {
	m := domain.Message{ID: id("msg"), SessionID: sid, TaskID: tid, Role: role, Content: content, CreatedAt: time.Now().UTC()}
	_, e := s.db.ExecContext(ctx, "INSERT INTO messages(id,session_id,task_id,role,content,created_at) VALUES(?,?,?,?,?,?)", m.ID, sid, tid, role, content, m.CreatedAt.Format(time.RFC3339Nano))
	return m, e
}
func (s *Store) Messages(ctx context.Context, sid string, limit int) ([]domain.Message, error) {
	rows, e := s.db.QueryContext(ctx, "SELECT id,session_id,COALESCE(task_id,''),role,content,created_at FROM (SELECT * FROM messages WHERE session_id=? ORDER BY created_at DESC LIMIT ?) ORDER BY created_at", sid, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := make([]domain.Message, 0)
	for rows.Next() {
		var m domain.Message
		var t string
		rows.Scan(&m.ID, &m.SessionID, &m.TaskID, &m.Role, &m.Content, &t)
		m.CreatedAt, _ = time.Parse(time.RFC3339Nano, t)
		out = append(out, m)
	}
	return out, rows.Err()
}
func (s *Store) TaskMessages(ctx context.Context, tid string, limit int) ([]domain.Message, error) {
	rows, e := s.db.QueryContext(ctx, "SELECT id,session_id,COALESCE(task_id,''),role,content,created_at FROM (SELECT * FROM messages WHERE task_id=? ORDER BY created_at DESC LIMIT ?) ORDER BY created_at", tid, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := make([]domain.Message, 0)
	for rows.Next() {
		var m domain.Message
		var ts string
		if e = rows.Scan(&m.ID, &m.SessionID, &m.TaskID, &m.Role, &m.Content, &ts); e != nil {
			return nil, e
		}
		m.CreatedAt, _ = time.Parse(time.RFC3339Nano, ts)
		out = append(out, m)
	}
	return out, rows.Err()
}
func (s *Store) LatestSummary(ctx context.Context, sid string) (string, string, error) {
	var last, summary string
	e := s.db.QueryRowContext(ctx, "SELECT COALESCE(last_message_id,''),summary FROM message_summaries WHERE session_id=? ORDER BY id DESC LIMIT 1", sid).Scan(&last, &summary)
	if errors.Is(e, sql.ErrNoRows) {
		return "", "", nil
	}
	return last, summary, e
}
func (s *Store) SaveSummary(ctx context.Context, sid, first, last, summary string) error {
	_, e := s.db.ExecContext(ctx, "INSERT INTO message_summaries(session_id,first_message_id,last_message_id,summary,created_at) VALUES(?,?,?,?,?)", sid, first, last, summary, now())
	return e
}
func (s *Store) CreateTask(ctx context.Context, sid, objective string) (domain.Task, error) {
	t := domain.Task{ID: id("task"), SessionID: sid, Objective: objective, Status: "running", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return t, e
	}
	defer tx.Rollback()
	_, e = tx.ExecContext(ctx, "INSERT INTO tasks(id,session_id,objective,status,created_at,updated_at) VALUES(?,?,?,?,?,?)", t.ID, sid, objective, t.Status, t.CreatedAt.Format(time.RFC3339Nano), t.UpdatedAt.Format(time.RFC3339Nano))
	if e == nil {
		_, e = tx.ExecContext(ctx, "INSERT INTO task_criteria(task_id,criterion) VALUES(?,?),(?,?)", t.ID, "模型返回非空交付结果", t.ID, "不存在未处理的工具错误")
	}
	if e == nil {
		e = tx.Commit()
	}
	return t, e
}
func (s *Store) Criteria(ctx context.Context, tid string) ([]domain.AcceptanceCriterion, error) {
	rows, e := s.db.QueryContext(ctx, "SELECT task_id,criterion,passed,reason FROM task_criteria WHERE task_id=? ORDER BY rowid", tid)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []domain.AcceptanceCriterion
	for rows.Next() {
		var c domain.AcceptanceCriterion
		if e = rows.Scan(&c.TaskID, &c.Criterion, &c.Passed, &c.Reason); e != nil {
			return nil, e
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s *Store) SetCriterion(ctx context.Context, tid, criterion string, passed bool, reason string) error {
	_, e := s.db.ExecContext(ctx, "UPDATE task_criteria SET passed=?,reason=? WHERE task_id=? AND criterion=?", passed, reason, tid, criterion)
	return e
}
func (s *Store) CreateNode(ctx context.Context, tid, title string) (domain.Node, error) {
	n := domain.Node{ID: id("node"), TaskID: tid, Title: title, Description: title, Status: "running", RiskLevel: "low", MaxAttempts: 2}
	b, _ := json.Marshal(n.DependsOn)
	ts := now()
	_, e := s.db.ExecContext(ctx, "INSERT INTO task_nodes(id,task_id,title,description,status,depends_on,risk_level,attempt,max_attempts,result,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", n.ID, n.TaskID, n.Title, n.Description, n.Status, string(b), n.RiskLevel, n.Attempt, n.MaxAttempts, n.Result, ts, ts)
	return n, e
}
func (s *Store) CreateGraph(ctx context.Context, tid string, nodes []domain.Node) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	for _, n := range nodes {
		b, _ := json.Marshal(n.DependsOn)
		if n.TaskID == "" {
			n.TaskID = tid
		}
		if n.MaxAttempts == 0 {
			n.MaxAttempts = 2
		}
		if n.RiskLevel == "" {
			n.RiskLevel = "low"
		}
		ts := now()
		if _, e = tx.ExecContext(ctx, "INSERT INTO task_nodes(id,task_id,title,description,status,depends_on,risk_level,attempt,max_attempts,result,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", n.ID, n.TaskID, n.Title, n.Description, n.Status, string(b), n.RiskLevel, n.Attempt, n.MaxAttempts, n.Result, ts, ts); e != nil {
			return e
		}
	}
	return tx.Commit()
}
func (s *Store) UpdateNode(ctx context.Context, nid, status, result string) error {
	_, e := s.db.ExecContext(ctx, "UPDATE task_nodes SET status=?,result=? WHERE id=?", status, result, nid)
	return e
}
func (s *Store) TransitionNode(ctx context.Context, nid, from, to, result string) error {
	r, e := s.db.ExecContext(ctx, "UPDATE task_nodes SET status=?,result=? WHERE id=? AND status=?", to, result, nid, from)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return fmt.Errorf("invalid or concurrent node transition %s -> %s", from, to)
	}
	return nil
}
func (s *Store) SaveCheckpoint(ctx context.Context, tid string, state any) error {
	b, _ := json.Marshal(state)
	_, e := s.db.ExecContext(ctx, "INSERT INTO task_checkpoints(task_id,state,created_at) VALUES(?,?,?)", tid, string(b), now())
	return e
}
func (s *Store) RecordModelUsage(ctx context.Context, tid, model, role string, usage map[string]any) error {
	num := func(k string) int64 {
		if v, ok := usage[k].(float64); ok {
			return int64(v)
		}
		return 0
	}
	prompt, completion, total := num("prompt_tokens"), num("completion_tokens"), num("total_tokens")
	_, e := s.db.ExecContext(ctx, "INSERT INTO model_usage(task_id,model,role,input_tokens,output_tokens,total_tokens,prompt_tokens,completion_tokens,duration_ms,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)", tid, model, role, prompt, completion, total, prompt, completion, 0, now())
	return e
}
func (s *Store) Nodes(ctx context.Context, tid string) ([]domain.Node, error) {
	rows, e := s.db.QueryContext(ctx, "SELECT id,task_id,title,description,status,depends_on,risk_level,attempt,max_attempts,result FROM task_nodes WHERE task_id=?", tid)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := make([]domain.Node, 0)
	for rows.Next() {
		var n domain.Node
		var d string
		rows.Scan(&n.ID, &n.TaskID, &n.Title, &n.Description, &n.Status, &d, &n.RiskLevel, &n.Attempt, &n.MaxAttempts, &n.Result)
		json.Unmarshal([]byte(d), &n.DependsOn)
		out = append(out, n)
	}
	return out, rows.Err()
}
func (s *Store) Task(ctx context.Context, tid string) (domain.Task, error) {
	var t domain.Task
	var c, u string
	e := s.db.QueryRowContext(ctx, "SELECT id,session_id,objective,status,result,error,created_at,updated_at FROM tasks WHERE id=?", tid).Scan(&t.ID, &t.SessionID, &t.Objective, &t.Status, &t.Result, &t.Error, &c, &u)
	t.CreatedAt, _ = time.Parse(time.RFC3339Nano, c)
	t.UpdatedAt, _ = time.Parse(time.RFC3339Nano, u)
	return t, e
}
func (s *Store) TaskWorkspaceRoot(ctx context.Context, tid string) (string, error) {
	var root string
	e := s.db.QueryRowContext(ctx, "SELECT w.root FROM tasks t JOIN sessions s ON s.id=t.session_id JOIN workspaces w ON w.id=s.workspace_id WHERE t.id=?", tid).Scan(&root)
	return root, e
}
func (s *Store) TasksBySession(ctx context.Context, sid string) ([]domain.Task, error) {
	rows, e := s.db.QueryContext(ctx, "SELECT id,session_id,objective,status,result,error,created_at,updated_at FROM tasks WHERE session_id=? ORDER BY created_at DESC", sid)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := make([]domain.Task, 0)
	for rows.Next() {
		var t domain.Task
		var c, u string
		rows.Scan(&t.ID, &t.SessionID, &t.Objective, &t.Status, &t.Result, &t.Error, &c, &u)
		t.CreatedAt, _ = time.Parse(time.RFC3339Nano, c)
		t.UpdatedAt, _ = time.Parse(time.RFC3339Nano, u)
		out = append(out, t)
	}
	return out, rows.Err()
}
func (s *Store) UpdateTask(ctx context.Context, tid, status, result, errText string) error {
	_, e := s.db.ExecContext(ctx, "UPDATE tasks SET status=?,result=?,error=?,updated_at=? WHERE id=?", status, result, errText, now(), tid)
	return e
}
func (s *Store) RunningTasks(ctx context.Context) ([]string, error) {
	rows, e := s.db.QueryContext(ctx, "SELECT id FROM tasks WHERE status IN ('running','waiting_approval')")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var a []string
	for rows.Next() {
		var x string
		rows.Scan(&x)
		a = append(a, x)
	}
	return a, rows.Err()
}
func (s *Store) CreateApproval(ctx context.Context, tid, nid, tool string, args any, risk, reason string) (domain.Approval, error) {
	b, _ := json.Marshal(args)
	h := sha256.Sum256(b)
	a := domain.Approval{ID: id("approval"), TaskID: tid, NodeID: nid, ToolName: tool, Arguments: string(b), ArgumentsDigest: hex.EncodeToString(h[:]), RiskLevel: risk, Reason: reason, Status: "pending", CreatedAt: time.Now().UTC()}
	var sid string
	_ = s.db.QueryRowContext(ctx, "SELECT session_id FROM tasks WHERE id=?", tid).Scan(&sid)
	_, e := s.db.ExecContext(ctx, "INSERT INTO approvals(id,task_id,session_id,node_id,tool_name,tool_call_id,arguments,arguments_digest,risk_level,reason,status,created_at,decided_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,NULL)", a.ID, a.TaskID, sid, a.NodeID, a.ToolName, "", a.Arguments, a.ArgumentsDigest, a.RiskLevel, a.Reason, a.Status, a.CreatedAt.Format(time.RFC3339Nano))
	return a, e
}
func (s *Store) Approvals(ctx context.Context, status string) ([]domain.Approval, error) {
	q := "SELECT id,task_id,COALESCE(node_id,''),tool_name,arguments,arguments_digest,risk_level,reason,status,created_at FROM approvals"
	var args []any
	if status != "" {
		q += " WHERE status=?"
		args = append(args, status)
	}
	q += " ORDER BY created_at DESC"
	rows, e := s.db.QueryContext(ctx, q, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := make([]domain.Approval, 0)
	for rows.Next() {
		var a domain.Approval
		var t string
		rows.Scan(&a.ID, &a.TaskID, &a.NodeID, &a.ToolName, &a.Arguments, &a.ArgumentsDigest, &a.RiskLevel, &a.Reason, &a.Status, &t)
		a.CreatedAt, _ = time.Parse(time.RFC3339Nano, t)
		out = append(out, a)
	}
	return out, rows.Err()
}
func (s *Store) DecideApproval(ctx context.Context, aid, decision string) (domain.Approval, error) {
	if decision != "approved" && decision != "rejected" {
		return domain.Approval{}, errors.New("invalid decision")
	}
	r, e := s.db.ExecContext(ctx, "UPDATE approvals SET status=?,decided_at=? WHERE id=? AND status='pending'", decision, now(), aid)
	if e != nil {
		return domain.Approval{}, e
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return domain.Approval{}, sql.ErrNoRows
	}
	rows, e := s.Approvals(ctx, "")
	if e != nil {
		return domain.Approval{}, e
	}
	for _, a := range rows {
		if a.ID == aid {
			return a, nil
		}
	}
	return domain.Approval{}, sql.ErrNoRows
}
func (s *Store) Approved(ctx context.Context, tid, tool, digest string) (bool, error) {
	var n int
	e := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM approvals WHERE task_id=? AND tool_name=? AND arguments_digest=? AND status='approved'", tid, tool, digest).Scan(&n)
	return n > 0, e
}
func (s *Store) ToolCall(ctx context.Context, tid, name, digest string) (string, string, bool, error) {
	var status, result string
	e := s.db.QueryRowContext(ctx, "SELECT status,COALESCE(result,'') FROM tool_calls WHERE task_id=? AND name=? AND arguments_digest=? ORDER BY created_at DESC LIMIT 1", tid, name, digest).Scan(&status, &result)
	if errors.Is(e, sql.ErrNoRows) {
		return "", "", false, nil
	}
	return status, result, e == nil, e
}
func (s *Store) BeginToolCall(ctx context.Context, id, tid, nid, name, digest string) error {
	_, e := s.db.ExecContext(ctx, "INSERT INTO tool_calls(id,task_id,node_id,name,arguments_digest,status,created_at,updated_at) VALUES(?,?,?,?,?,'running',?,?)", id, tid, nid, name, digest, now(), now())
	return e
}
func (s *Store) CompleteToolCall(ctx context.Context, id, status string, result any) error {
	b, _ := json.Marshal(result)
	_, e := s.db.ExecContext(ctx, "UPDATE tool_calls SET status=?,result=?,updated_at=? WHERE id=?", status, string(b), now(), id)
	return e
}
func (s *Store) Emit(ctx context.Context, sid, tid, typ string, payload any) (domain.Event, error) {
	b, _ := json.Marshal(payload)
	b = redactEvent(b)
	res, e := s.db.ExecContext(ctx, "INSERT INTO events(trace_id,session_id,task_id,node_id,type,payload,created_at) VALUES(?,?,?,?,?,?,?)", id("trace"), sid, tid, "", typ, string(b), now())
	if e != nil {
		return domain.Event{}, e
	}
	eid, _ := res.LastInsertId()
	var safe any
	_ = json.Unmarshal(b, &safe)
	ev := domain.Event{ID: eid, SessionID: sid, TaskID: tid, Type: typ, Payload: safe, CreatedAt: time.Now().UTC()}
	s.mu.Lock()
	for ch := range s.subscribers[sid] {
		select {
		case ch <- ev:
		default:
		}
	}
	s.mu.Unlock()
	return ev, nil
}

var secretPatterns = []*regexp.Regexp{regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9._~+/-]+`), regexp.MustCompile(`(?i)(api[_-]?key|password|token)(\\?"?\s*[:=]\s*\\?"?)[^\s",}]+`), regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}`)}

func redactEvent(b []byte) []byte {
	s := string(b)
	for _, r := range secretPatterns {
		s = r.ReplaceAllString(s, "[REDACTED]")
	}
	return []byte(s)
}
func (s *Store) Events(ctx context.Context, sid string, after int64) ([]domain.Event, error) {
	rows, e := s.db.QueryContext(ctx, "SELECT id,session_id,task_id,type,payload,created_at FROM events WHERE session_id=? AND id>? ORDER BY id", sid, after)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []domain.Event
	for rows.Next() {
		var ev domain.Event
		var p, t string
		rows.Scan(&ev.ID, &ev.SessionID, &ev.TaskID, &ev.Type, &p, &t)
		json.Unmarshal([]byte(p), &ev.Payload)
		ev.CreatedAt, _ = time.Parse(time.RFC3339Nano, t)
		out = append(out, ev)
	}
	return out, rows.Err()
}
func (s *Store) Subscribe(sid string) (chan domain.Event, func()) {
	ch := make(chan domain.Event, 32)
	s.mu.Lock()
	if s.subscribers[sid] == nil {
		s.subscribers[sid] = map[chan domain.Event]struct{}{}
	}
	s.subscribers[sid][ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() { s.mu.Lock(); delete(s.subscribers[sid], ch); close(ch); s.mu.Unlock() }
}
func (s *Store) PutArtifact(ctx context.Context, tid, mime string, b []byte) (string, error) {
	h := sha256.Sum256(b)
	sum := hex.EncodeToString(h[:])
	dir := filepath.Join(s.artifactRoot, sum[:2])
	if e := os.MkdirAll(dir, 0700); e != nil {
		return "", e
	}
	path := filepath.Join(dir, sum)
	tmp := path + ".tmp"
	f, e := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if e != nil {
		return "", e
	}
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	if x := f.Close(); e == nil {
		e = x
	}
	if e != nil {
		return "", e
	}
	if e := os.Rename(tmp, path); e != nil {
		return "", e
	}
	aid := "artifact_" + sum[:24]
	_, e = s.db.ExecContext(ctx, "INSERT OR IGNORE INTO artifacts(id,task_id,kind,mime_type,path,size,sha256,created_at) VALUES(?,?,?,?,?,?,?,?)", aid, tid, mime, mime, path, len(b), sum, now())
	return aid, e
}
func (s *Store) Artifact(ctx context.Context, aid string) (string, string, error) {
	var p, m string
	e := s.db.QueryRowContext(ctx, "SELECT path,mime_type FROM artifacts WHERE id=?", aid).Scan(&p, &m)
	return p, m, e
}
func (s *Store) DB() *sql.DB { return s.db }
func (s *Store) Metrics(ctx context.Context) (map[string]any, error) {
	out := map[string]any{}
	for k, q := range map[string]string{"tasks_total": "SELECT COUNT(*) FROM tasks", "tasks_completed": "SELECT COUNT(*) FROM tasks WHERE status='completed'", "tasks_failed": "SELECT COUNT(*) FROM tasks WHERE status='failed'", "approvals_pending": "SELECT COUNT(*) FROM approvals WHERE status='pending'", "tool_errors": "SELECT COUNT(*) FROM tool_calls WHERE status='failed'", "model_tokens": "SELECT COALESCE(SUM(total_tokens),0) FROM model_usage"} {
		var n int64
		if e := s.db.QueryRowContext(ctx, q).Scan(&n); e != nil {
			return nil, e
		}
		out[k] = n
	}
	return out, nil
}

var _ = fmt.Sprintf

package storage

import (
	"context"
	"database/sql"
	_ "modernc.org/sqlite"
	"path/filepath"
	"testing"
)

func TestPersistenceAndExactApproval(t *testing.T) {
	root := t.TempDir()
	s, e := Open(filepath.Join(root, "w.db"), root)
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	w, e := s.OpenWorkspace(ctx, "x", root, "local")
	if e != nil {
		t.Fatal(e)
	}
	sess, e := s.CreateSession(ctx, w.ID, "x")
	if e != nil {
		t.Fatal(e)
	}
	task, e := s.CreateTask(ctx, sess.ID, "test")
	if e != nil {
		t.Fatal(e)
	}
	a, e := s.CreateApproval(ctx, task.ID, "", "shell.execute", map[string]any{"command": "true"}, "L2", "test")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.DecideApproval(ctx, a.ID, "approved"); e != nil {
		t.Fatal(e)
	}
	ok, e := s.Approved(ctx, task.ID, "shell.execute", a.ArgumentsDigest)
	if e != nil || !ok {
		t.Fatalf("approval missing: %v %v", ok, e)
	}
	s.Close()
	s, e = Open(filepath.Join(root, "w.db"), root)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	got, e := s.Task(ctx, task.ID)
	if e != nil || got.Objective != "test" {
		t.Fatalf("persistence failed: %#v %v", got, e)
	}
}
func TestLegacyWorkspaceMigration(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "legacy.db")
	db, e := sql.Open("sqlite", path)
	if e != nil {
		t.Fatal(e)
	}
	_, e = db.Exec("CREATE TABLE workspaces(id TEXT PRIMARY KEY,path TEXT NOT NULL,created_at TEXT); CREATE TABLE sessions(id TEXT PRIMARY KEY,workspace_id TEXT,title TEXT,status TEXT NOT NULL,created_at TEXT,updated_at TEXT); INSERT INTO workspaces(id,path,created_at) VALUES('old',?, '')", root)
	if e != nil {
		t.Fatal(e)
	}
	db.Close()
	s, e := Open(path, root)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ws, e := s.Workspaces(context.Background())
	if e != nil || len(ws) != 1 || ws[0].Root != root || ws[0].Name != root {
		t.Fatalf("migration failed: %#v %v", ws, e)
	}
	session, e := s.CreateSession(context.Background(), "old", "legacy session")
	if e != nil || session.WorkspaceID != "old" {
		t.Fatalf("legacy session insert failed: %#v %v", session, e)
	}
}

func TestLegacyDatabaseSupportsCompleteWritePath(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "legacy-full.db")
	db, e := sql.Open("sqlite", path)
	if e != nil {
		t.Fatal(e)
	}
	legacySchema := `
CREATE TABLE workspaces(id TEXT PRIMARY KEY,name TEXT NOT NULL,type TEXT NOT NULL,path TEXT NOT NULL UNIQUE,created_at DATETIME NOT NULL);
CREATE TABLE sessions(id TEXT PRIMARY KEY,workspace_id TEXT NOT NULL,title TEXT NOT NULL,status TEXT NOT NULL,created_at DATETIME NOT NULL,updated_at DATETIME NOT NULL);
CREATE TABLE messages(id TEXT PRIMARY KEY,session_id TEXT NOT NULL,task_id TEXT NOT NULL DEFAULT '',role TEXT NOT NULL,content TEXT NOT NULL,metadata BLOB,created_at DATETIME NOT NULL);
CREATE TABLE tasks(id TEXT PRIMARY KEY,session_id TEXT NOT NULL,objective TEXT NOT NULL,status TEXT NOT NULL,result TEXT NOT NULL DEFAULT '',created_at DATETIME NOT NULL,updated_at DATETIME NOT NULL,completed_at DATETIME);
CREATE TABLE task_nodes(id TEXT PRIMARY KEY,task_id TEXT NOT NULL,title TEXT NOT NULL,status TEXT NOT NULL,depends_on BLOB NOT NULL,risk_level TEXT NOT NULL,result BLOB,attempt INTEGER NOT NULL DEFAULT 0,created_at DATETIME NOT NULL,updated_at DATETIME NOT NULL,max_attempts INTEGER NOT NULL DEFAULT 2);
CREATE TABLE approvals(id TEXT PRIMARY KEY,task_id TEXT NOT NULL,session_id TEXT NOT NULL,node_id TEXT NOT NULL DEFAULT '',tool_name TEXT NOT NULL,tool_call_id TEXT NOT NULL,arguments BLOB NOT NULL,arguments_digest TEXT NOT NULL,risk_level TEXT NOT NULL,reason TEXT NOT NULL,status TEXT NOT NULL,created_at DATETIME NOT NULL,decided_at DATETIME);
CREATE TABLE events(id INTEGER PRIMARY KEY AUTOINCREMENT,trace_id TEXT NOT NULL,session_id TEXT NOT NULL,task_id TEXT NOT NULL DEFAULT '',node_id TEXT NOT NULL DEFAULT '',type TEXT NOT NULL,payload BLOB NOT NULL,created_at DATETIME NOT NULL);
CREATE TABLE model_usage(id INTEGER PRIMARY KEY AUTOINCREMENT,task_id TEXT NOT NULL,model TEXT NOT NULL,prompt_tokens INTEGER NOT NULL DEFAULT 0,completion_tokens INTEGER NOT NULL DEFAULT 0,duration_ms INTEGER NOT NULL DEFAULT 0,created_at DATETIME NOT NULL);
CREATE TABLE artifacts(id TEXT PRIMARY KEY,task_id TEXT NOT NULL,kind TEXT NOT NULL,path TEXT NOT NULL,sha256 TEXT NOT NULL,size INTEGER NOT NULL,created_at DATETIME NOT NULL);
INSERT INTO workspaces(id,name,type,path,created_at) VALUES('old','legacy','local',?, '2026-01-01T00:00:00Z');`
	if _, e = db.Exec(legacySchema, root); e != nil {
		t.Fatal(e)
	}
	if e = db.Close(); e != nil {
		t.Fatal(e)
	}

	s, e := Open(path, root)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ctx := context.Background()

	if _, e = s.OpenWorkspace(ctx, "new", filepath.Join(root, "new"), "local"); e != nil {
		t.Fatalf("open workspace after migration: %v", e)
	}
	session, e := s.CreateSession(ctx, "old", "legacy session")
	if e != nil {
		t.Fatal(e)
	}
	task, e := s.CreateTask(ctx, session.ID, "legacy write path")
	if e != nil {
		t.Fatal(e)
	}
	node, e := s.CreateNode(ctx, task.ID, "execute")
	if e != nil {
		t.Fatalf("create node after migration: %v", e)
	}
	if _, e = s.AddMessage(ctx, session.ID, task.ID, "user", "hello"); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Emit(ctx, session.ID, task.ID, "task.started", map[string]any{"ok": true}); e != nil {
		t.Fatalf("emit after migration: %v", e)
	}
	if _, e = s.CreateApproval(ctx, task.ID, node.ID, "shell.execute", map[string]any{"command": "true"}, "L2", "test"); e != nil {
		t.Fatalf("approval after migration: %v", e)
	}
	if e = s.RecordModelUsage(ctx, task.ID, "test", "assistant", map[string]any{"prompt_tokens": float64(2), "completion_tokens": float64(3), "total_tokens": float64(5)}); e != nil {
		t.Fatalf("model usage after migration: %v", e)
	}
	if _, e = s.PutArtifact(ctx, task.ID, "text/plain", []byte("done")); e != nil {
		t.Fatalf("artifact after migration: %v", e)
	}
}

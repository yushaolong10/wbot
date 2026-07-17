package permission

import (
	"context"
	"github.com/wbot-dev/wbot/internal/config"
	"github.com/wbot-dev/wbot/internal/storage"
	"os"
	"path/filepath"
	"testing"
)

func TestWorkspaceEscapeAndApproval(t *testing.T) {
	root := t.TempDir()
	s := config.Settings{WorkspaceRoot: root, PermissionMode: "approval", AllowShell: true}
	st, e := storage.Open(filepath.Join(root, "x.db"), root)
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	p := New(s, st)
	if _, e = p.ResolvePath("../outside"); e == nil {
		t.Fatal("expected escape rejection")
	}
	d, e := p.Evaluate(context.Background(), "task", "shell.execute", map[string]any{"command": "true"})
	if e != nil || d.Kind != "ASK" {
		t.Fatalf("got %#v %v", d, e)
	}
	if _, e = st.CreateApproval(context.Background(), "task", "", "shell.execute", map[string]any{"command": "true"}, "L2", "test"); e != nil {
		t.Fatal(e)
	}
	outside := t.TempDir()
	if e = os.Symlink(outside, filepath.Join(root, "link")); e == nil {
		if _, e = p.ResolvePath("link/secret"); e == nil {
			t.Fatal("expected symlink escape rejection")
		}
	}
}

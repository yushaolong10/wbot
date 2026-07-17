package permission

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wbot-dev/wbot/internal/config"
	"github.com/wbot-dev/wbot/internal/storage"
)

type Decision struct{ Kind, Reason, Risk string }
type Engine struct {
	s     config.Settings
	store *storage.Store
}

func New(s config.Settings, st *storage.Store) *Engine { return &Engine{s: s, store: st} }
func Digest(args any) string {
	b, _ := json.Marshal(args)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (e *Engine) Evaluate(ctx context.Context, taskID, tool string, args map[string]any) (Decision, error) {
	risk := "L0"
	side := false
	switch tool {
	case "filesystem.write":
		risk = "L1"
		side = true
	case "shell.execute":
		risk = "L2"
		side = true
	case "filesystem.delete":
		risk = "L3"
		side = true
	}
	if strings.HasPrefix(tool, "filesystem.") {
		p, _ := args["path"].(string)
		if _, err := e.ResolveTaskPath(ctx, taskID, p); err != nil {
			return Decision{"DENY", err.Error(), risk}, nil
		}
	}
	if tool == "shell.execute" && !e.s.AllowShell {
		return Decision{"DENY", "shell is disabled", risk}, nil
	}
	if !side || e.s.PermissionMode == "full_access" || risk == "L1" {
		return Decision{"ALLOW", "policy allows operation", risk}, nil
	}
	ok, err := e.store.Approved(ctx, taskID, tool, Digest(args))
	if err != nil {
		return Decision{}, err
	}
	if ok {
		return Decision{"ALLOW", "exact arguments approved", risk}, nil
	}
	return Decision{"ASK", fmt.Sprintf("%s operation requires approval", risk), risk}, nil
}
func (e *Engine) WorkspaceRoot(ctx context.Context, taskID string) string {
	if taskID != "" {
		if root, x := e.store.TaskWorkspaceRoot(ctx, taskID); x == nil && root != "" {
			return root
		}
	}
	return e.s.WorkspaceRoot
}
func (e *Engine) ResolveTaskPath(ctx context.Context, taskID, p string) (string, error) {
	return e.resolve(e.WorkspaceRoot(ctx, taskID), p)
}
func (e *Engine) ResolvePath(p string) (string, error) {
	return e.resolve(e.s.WorkspaceRoot, p)
}
func (e *Engine) resolve(workspaceRoot, p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("path is required")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(workspaceRoot, p)
	}
	p = filepath.Clean(p)
	root := workspaceRoot
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	checked := p
	if _, err := os.Lstat(p); err == nil {
		if real, x := filepath.EvalSymlinks(p); x == nil {
			checked = real
		}
	} else {
		parent := filepath.Dir(p)
		if real, x := filepath.EvalSymlinks(parent); x == nil {
			checked = filepath.Join(real, filepath.Base(p))
		}
	}
	rel, err := filepath.Rel(root, checked)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes workspace")
	}
	return p, nil
}

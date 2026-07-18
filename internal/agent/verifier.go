package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/wbot-dev/wbot/internal/domain"
	"github.com/wbot-dev/wbot/internal/permission"
	"github.com/wbot-dev/wbot/internal/storage"
)

// Verifier is a deterministic or semantic check for a single acceptance criterion.
type Verifier interface {
	// Type returns the CriterionType this verifier handles.
	Type() domain.CriterionType
	// Verify checks the criterion and returns evidence.
	Verify(ctx context.Context, c domain.AcceptanceCriterion, store *storage.Store) (domain.Evidence, error)
}

// VerifierRegistry maps criterion types to their verifier implementations.
type VerifierRegistry struct {
	verifiers map[domain.CriterionType]Verifier
}

func NewVerifierRegistry(permissions ...*permission.Engine) *VerifierRegistry {
	var policy *permission.Engine
	if len(permissions) > 0 {
		policy = permissions[0]
	}
	r := &VerifierRegistry{verifiers: map[domain.CriterionType]Verifier{}}
	r.Register(&FileExistsVerifier{permissions: policy})
	r.Register(&FileContainsVerifier{permissions: policy})
	r.Register(&CommandVerifier{permissions: policy})
	r.Register(&ArtifactVerifier{})
	r.Register(&ModelRubricVerifier{})
	r.Register(&ToolResultVerifier{})
	return r
}

func (r *VerifierRegistry) Register(v Verifier) {
	r.verifiers[v.Type()] = v
}

func (r *VerifierRegistry) Verify(ctx context.Context, c domain.AcceptanceCriterion, store *storage.Store) (domain.Evidence, error) {
	v, ok := r.verifiers[c.Type]
	if !ok {
		return domain.Evidence{}, fmt.Errorf("no verifier for criterion type %q", c.Type)
	}
	return v.Verify(ctx, c, store)
}

// ---- Evidence Collector ----

type EvidenceCollector struct {
	registry *VerifierRegistry
	store    *storage.Store
}

func NewEvidenceCollector(store *storage.Store, registry *VerifierRegistry) *EvidenceCollector {
	return &EvidenceCollector{registry: registry, store: store}
}

// Collect runs all deterministic verifiers first, then collects results.
// Deterministic failures cannot be overridden by semantic evaluation.
func (c *EvidenceCollector) Collect(ctx context.Context, nodeID string, criteria []domain.AcceptanceCriterion) ([]domain.Evidence, []domain.AcceptanceCriterion, error) {
	var evidence []domain.Evidence
	updated := make([]domain.AcceptanceCriterion, 0, len(criteria))

	for _, crit := range criteria {
		if crit.NodeID == "" {
			crit.NodeID = nodeID
		}
		ev, err := c.registry.Verify(ctx, crit, c.store)
		if err != nil {
			return evidence, updated, fmt.Errorf("verify criterion %s (%s): %w", crit.ID, crit.Type, err)
		}
		if err := c.store.SaveEvidence(ctx, ev); err != nil {
			return evidence, updated, err
		}
		evidence = append(evidence, ev)

		status := "failed"
		if ev.Passed {
			status = "passed"
		}
		if err := c.store.UpdateAcceptanceCriterion(ctx, crit.ID, status, ev.Summary, []string{ev.ID}); err != nil {
			return evidence, updated, err
		}
		crit.Status = status
		crit.EvidenceIDs = append(crit.EvidenceIDs, ev.ID)
		updated = append(updated, crit)
	}

	return evidence, updated, nil
}

// ModelRubricVerifier records the result produced by the semantic/legacy
// evaluator as durable evidence. A pending rubric is not silently accepted.
type ModelRubricVerifier struct{}

func (v *ModelRubricVerifier) Type() domain.CriterionType { return domain.CriterionModelRubric }

func (v *ModelRubricVerifier) Verify(ctx context.Context, c domain.AcceptanceCriterion, store *storage.Store) (domain.Evidence, error) {
	if c.Status != "passed" && c.Status != "failed" {
		return domain.Evidence{}, fmt.Errorf("model rubric has not been evaluated")
	}
	passed := c.Status == "passed"
	summary := c.Reason
	if summary == "" {
		summary = c.Description
	}
	h := sha256.Sum256([]byte(c.ID + c.Status + summary))
	return domain.Evidence{ID: storage.NewID("ev"), TaskID: c.TaskID, NodeID: c.NodeID, CriterionID: c.ID, Type: "semantic", Source: "model_rubric", Digest: hex.EncodeToString(h[:]), Summary: summary, Passed: passed, CollectedAt: time.Now().UTC()}, nil
}

// ToolResultVerifier checks structured tool results instead of relying on
// string matching against the final answer.
type ToolResultVerifier struct{}

func (v *ToolResultVerifier) Type() domain.CriterionType { return domain.CriterionToolResult }

func (v *ToolResultVerifier) Verify(ctx context.Context, c domain.AcceptanceCriterion, store *storage.Store) (domain.Evidence, error) {
	msgs, err := store.TaskMessages(ctx, c.TaskID, 500)
	if err != nil {
		return domain.Evidence{}, err
	}
	passed, summary, err := evaluateToolResults(msgs)
	if err != nil {
		return domain.Evidence{}, err
	}
	h := sha256.Sum256([]byte(c.ID + fmt.Sprintf("%t", passed) + summary))
	return domain.Evidence{ID: storage.NewID("ev"), TaskID: c.TaskID, NodeID: c.NodeID, CriterionID: c.ID, Type: "deterministic", Source: "tool_result", Digest: hex.EncodeToString(h[:]), Summary: summary, Passed: passed, CollectedAt: time.Now().UTC()}, nil
}

func evaluateToolResults(msgs []domain.Message) (bool, string, error) {
	callKeys := make(map[string]string)
	latestByKey := make(map[string]domain.ToolResult)
	toolNameByKey := make(map[string]string)
	for _, msg := range msgs {
		if msg.Role == "assistant" && len(msg.ContentJSON) > 0 {
			var calls []domain.ToolCall
			if err := json.Unmarshal(msg.ContentJSON, &calls); err == nil {
				for _, call := range calls {
					key := call.Name + ":" + permission.Digest(call.Arguments)
					callKeys[call.ID] = key
					toolNameByKey[key] = call.Name
				}
			}
			continue
		}
		if msg.Role != "tool" {
			continue
		}
		var result domain.ToolResult
		if err := json.Unmarshal(msg.ContentJSON, &result); err != nil {
			return false, "", fmt.Errorf("decode tool result %s: %w", msg.ID, err)
		}
		key := callKeys[msg.ToolCallID]
		if key == "" {
			// Legacy messages may not retain the assistant call arguments. Keep
			// each such result independent so a later success cannot hide it.
			key = msg.ToolName + ":call:" + msg.ToolCallID
			toolNameByKey[key] = msg.ToolName
		}
		latestByKey[key] = result
	}
	passed := true
	summary := "未发现未处理的工具错误"
	ignoredReadFailures := 0
	for key, result := range latestByKey {
		toolName := toolNameByKey[key]
		if isBlockingToolFailure(toolName, result) {
			passed = false
			summary = fmt.Sprintf("工具 %s 仍有未处理错误: %s", toolName, result.Summary)
			break
		}
		if isFailedToolResult(result) {
			ignoredReadFailures++
		}
	}
	if passed && ignoredReadFailures > 0 {
		summary = fmt.Sprintf("未发现未处理的关键工具错误（已容忍 %d 次只读探查失败）", ignoredReadFailures)
	}
	return passed, summary, nil
}

func isFailedToolResult(result domain.ToolResult) bool {
	return result.Status == "error" || result.Status == "unknown" || result.Error != nil
}

func isBlockingToolFailure(toolName string, result domain.ToolResult) bool {
	if !isFailedToolResult(result) {
		return false
	}
	if result.Status == "unknown" || (result.Error != nil && (result.Error.Code == "RESULT_UNKNOWN" || result.Error.Code == "TOOL_TIMEOUT")) {
		return true
	}
	// Read-only calls are exploratory. Once the model has continued to a final
	// delivery, a missing candidate path or failed advisory lookup is not an
	// unresolved side effect. Required reads must be expressed as file criteria.
	return toolName != "filesystem.read" && toolName != "consult_advisor"
}

// ---- FileExistsVerifier ----

type FileExistsVerifier struct{ permissions *permission.Engine }

func (v *FileExistsVerifier) Type() domain.CriterionType { return domain.CriterionFileExists }

func (v *FileExistsVerifier) Verify(ctx context.Context, c domain.AcceptanceCriterion, store *storage.Store) (domain.Evidence, error) {
	var cfg struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(c.Config, &cfg); err != nil || cfg.Path == "" {
		return domain.Evidence{}, fmt.Errorf("file_exists requires path in config")
	}

	path, err := resolveVerifierPath(ctx, store, v.permissions, c.TaskID, cfg.Path)
	if err != nil {
		return domain.Evidence{}, err
	}
	_, err = os.Stat(path)
	passed := err == nil
	summary := fmt.Sprintf("文件 %s 存在", path)
	if !passed {
		summary = fmt.Sprintf("文件 %s 不存在: %v", path, err)
	}

	h := sha256.Sum256([]byte(cfg.Path + fmt.Sprintf("%v", passed)))
	return domain.Evidence{
		ID:          storage.NewID("ev"),
		TaskID:      c.TaskID,
		NodeID:      c.NodeID,
		CriterionID: c.ID,
		Type:        "deterministic",
		Source:      "file_exists",
		Digest:      hex.EncodeToString(h[:]),
		Summary:     summary,
		Passed:      passed,
		CollectedAt: time.Now().UTC(),
	}, nil
}

// ---- FileContainsVerifier ----

type FileContainsVerifier struct{ permissions *permission.Engine }

func (v *FileContainsVerifier) Type() domain.CriterionType { return domain.CriterionFileContains }

func (v *FileContainsVerifier) Verify(ctx context.Context, c domain.AcceptanceCriterion, store *storage.Store) (domain.Evidence, error) {
	var cfg struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(c.Config, &cfg); err != nil || cfg.Path == "" {
		return domain.Evidence{}, fmt.Errorf("file_contains requires path and content in config")
	}

	path, err := resolveVerifierPath(ctx, store, v.permissions, c.TaskID, cfg.Path)
	if err != nil {
		return domain.Evidence{}, err
	}
	data, err := os.ReadFile(path)
	passed := err == nil && strings.Contains(string(data), cfg.Content)
	summary := fmt.Sprintf("文件 %s 包含指定内容", path)
	if !passed {
		if err != nil {
			summary = fmt.Sprintf("文件 %s 读取失败: %v", path, err)
		} else {
			summary = fmt.Sprintf("文件 %s 不包含指定内容", path)
		}
	}

	h := sha256.Sum256([]byte(cfg.Path + cfg.Content + fmt.Sprintf("%v", passed)))
	return domain.Evidence{
		ID:          storage.NewID("ev"),
		TaskID:      c.TaskID,
		NodeID:      c.NodeID,
		CriterionID: c.ID,
		Type:        "deterministic",
		Source:      "file_contains",
		Digest:      hex.EncodeToString(h[:]),
		Summary:     summary,
		Passed:      passed,
		CollectedAt: time.Now().UTC(),
	}, nil
}

// ---- CommandVerifier ----

type CommandVerifier struct{ permissions *permission.Engine }

func (v *CommandVerifier) Type() domain.CriterionType { return domain.CriterionCommand }

func (v *CommandVerifier) Verify(ctx context.Context, c domain.AcceptanceCriterion, store *storage.Store) (domain.Evidence, error) {
	var cfg struct {
		Command          string `json:"command"`
		ExpectedExitCode int    `json:"expected_exit_code"`
		TimeoutSeconds   int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(c.Config, &cfg); err != nil || cfg.Command == "" {
		return domain.Evidence{}, fmt.Errorf("command criterion requires command in config")
	}

	if store == nil {
		return domain.Evidence{}, fmt.Errorf("command verifier requires task storage")
	}
	if v.permissions == nil {
		return domain.Evidence{}, fmt.Errorf("command verifier requires permission policy")
	}
	args := map[string]any{"command": cfg.Command}
	if cfg.TimeoutSeconds > 0 {
		args["timeout_seconds"] = cfg.TimeoutSeconds
	}
	decision, err := v.permissions.Evaluate(ctx, c.TaskID, "shell.execute", args)
	if err != nil {
		return domain.Evidence{}, err
	}
	if decision.Kind != "ALLOW" {
		return domain.Evidence{}, fmt.Errorf("command verifier denied by permission policy: %s", decision.Reason)
	}
	root, err := store.TaskWorkspaceRoot(ctx, c.TaskID)
	if err != nil {
		return domain.Evidence{}, err
	}
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 || timeout > 120*time.Second {
		timeout = 30 * time.Second
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, "sh", "-c", cfg.Command)
	cmd.Dir = root
	output, err := cmd.CombinedOutput()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	passed := exitCode == cfg.ExpectedExitCode
	summary := fmt.Sprintf("命令 %q 退出码 %d (期望 %d)", cfg.Command, exitCode, cfg.ExpectedExitCode)
	if !passed {
		summary += fmt.Sprintf("; 输出: %s", string(output[:minInt(len(output), 200)]))
	}

	h := sha256.Sum256([]byte(cfg.Command + fmt.Sprintf("%d", exitCode)))
	return domain.Evidence{
		ID:          storage.NewID("ev"),
		TaskID:      c.TaskID,
		NodeID:      c.NodeID,
		CriterionID: c.ID,
		Type:        "deterministic",
		Source:      "command",
		Digest:      hex.EncodeToString(h[:]),
		Summary:     summary,
		Passed:      passed,
		CollectedAt: time.Now().UTC(),
	}, nil
}

func resolveVerifierPath(ctx context.Context, store *storage.Store, policy *permission.Engine, taskID, requested string) (string, error) {
	if policy != nil {
		decision, err := policy.Evaluate(ctx, taskID, "filesystem.read", map[string]any{"path": requested})
		if err != nil {
			return "", err
		}
		if decision.Kind != "ALLOW" {
			return "", fmt.Errorf("file verifier denied by permission policy: %s", decision.Reason)
		}
		return policy.ResolveTaskPath(ctx, taskID, requested)
	}
	if store == nil {
		// Unit-only fallback: production always injects a permission engine.
		return filepath.Clean(requested), nil
	}
	root, err := store.TaskWorkspaceRoot(ctx, taskID)
	if err != nil {
		return "", err
	}
	p := requested
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	p = filepath.Clean(p)
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes workspace")
	}
	return p, nil
}

// ---- ArtifactVerifier ----

type ArtifactVerifier struct{}

func (v *ArtifactVerifier) Type() domain.CriterionType { return domain.CriterionArtifact }

func (v *ArtifactVerifier) Verify(ctx context.Context, c domain.AcceptanceCriterion, store *storage.Store) (domain.Evidence, error) {
	var cfg struct {
		MimeType string `json:"mime_type"`
		MinSize  int64  `json:"min_size"`
	}
	_ = json.Unmarshal(c.Config, &cfg)

	artifacts, err := store.TaskArtifactInfo(ctx, c.TaskID)
	if err != nil {
		return domain.Evidence{}, err
	}

	matched := make([]domain.ArtifactInfo, 0, len(artifacts))
	for _, artifact := range artifacts {
		if cfg.MimeType != "" && artifact.MimeType != cfg.MimeType {
			continue
		}
		if cfg.MinSize > 0 && artifact.Size < cfg.MinSize {
			continue
		}
		matched = append(matched, artifact)
	}
	passed := len(matched) > 0
	summary := fmt.Sprintf("任务产出 %d 个符合条件的 artifact", len(matched))
	if !passed {
		summary = fmt.Sprintf("任务没有符合 mime_type=%q min_size=%d 的 artifact", cfg.MimeType, cfg.MinSize)
	}

	h := sha256.Sum256([]byte(fmt.Sprintf("%v", matched)))
	artifactID := ""
	if len(matched) > 0 {
		artifactID = matched[0].ID
	}
	return domain.Evidence{
		ID:          storage.NewID("ev"),
		TaskID:      c.TaskID,
		NodeID:      c.NodeID,
		CriterionID: c.ID,
		Type:        "deterministic",
		Source:      "artifact",
		ArtifactID:  artifactID,
		Digest:      hex.EncodeToString(h[:]),
		Summary:     summary,
		Passed:      passed,
		CollectedAt: time.Now().UTC(),
	}, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

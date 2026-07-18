package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wbot-dev/wbot/internal/domain"
	"github.com/wbot-dev/wbot/internal/storage"
	taskgraph "github.com/wbot-dev/wbot/internal/task"
)

// Planner generates and manages dynamic task graphs.
type Planner struct {
	store *storage.Store
}

func NewPlanner(store *storage.Store) *Planner {
	return &Planner{store: store}
}

// DetectComplexity analyzes the objective text to decide the graph complexity.
// Simple: informational queries, quick checks, lookups.
// Complex: multi-step work involving changes, creation, or deployment.
func DetectComplexity(objective string) string {
	complexWords := []string{
		"项目", "完整", "系统", "设计", "实现", "开发", "部署", "发布", "重构",
		"迁移", "审计", "优化", "构建", "修复", "创建", "生成", "编写", "改造",
		"build", "deploy", "refactor", "implement", "develop", "migrate", "audit",
		"create", "generate", "fix", "optimize", "restructure",
	}
	simpleWords := []string{
		"查询", "检查", "查看", "显示", "列出", "告诉我", "是什么", "在哪里",
		"check", "show", "list", "what", "how", "find", "search", "look",
		"explain", "describe", "summarize",
	}

	lower := strings.ToLower(objective)
	complexScore := 0
	simpleScore := 0

	for _, w := range complexWords {
		if strings.Contains(lower, strings.ToLower(w)) {
			complexScore++
		}
	}
	for _, w := range simpleWords {
		if strings.Contains(lower, strings.ToLower(w)) {
			simpleScore++
		}
	}

	// Short objectives tend to be simple
	if len([]rune(objective)) < 20 {
		simpleScore++
	}
	// Long objectives with action verbs tend to be complex
	if len([]rune(objective)) > 80 {
		complexScore += 2
	}

	if complexScore > simpleScore {
		return "complex"
	}
	return "simple"
}

// GenerateGraph creates a dynamic task graph for the given objective.
// Complexity is auto-detected from the objective text unless explicitly provided.
func (p *Planner) GenerateGraph(ctx context.Context, task domain.Task, complexity string) ([]domain.Node, []domain.AcceptanceCriterion, domain.GraphRevision, error) {
	if complexity == "" {
		complexity = DetectComplexity(task.Objective)
	}
	var proposal domain.GraphProposal

	if complexity == "simple" {
		proposal = p.simpleGraph(task.Objective)
	} else {
		proposal = p.complexGraph(task.Objective)
	}

	// Validate before persisting
	if err := taskgraph.ValidateGraphProposal(proposal, 12); err != nil {
		return nil, nil, domain.GraphRevision{}, fmt.Errorf("graph proposal validation failed: %w", err)
	}

	// Convert proposal to nodes
	nodes, criteria := p.proposalToNodes(task.ID, proposal)

	// Build the revision; storage commits it atomically with the graph.
	version, err := p.store.NextGraphRevision(ctx, task.ID)
	if err != nil {
		return nil, nil, domain.GraphRevision{}, err
	}

	// Store the materialized nodes (with durable IDs), allowing the API/UI to
	// associate live node state with the exact graph revision.
	nodesJSON, _ := json.Marshal(nodes)
	revision := domain.GraphRevision{
		ID:           storage.NewID("gr"),
		TaskID:       task.ID,
		Version:      version,
		Reason:       fmt.Sprintf("initial plan (complexity=%s)", complexity),
		PlannerModel: "planner-v1",
		NodesJSON:    string(nodesJSON),
	}
	return nodes, criteria, revision, nil
}

// Replan generates a new graph revision after a replan condition is triggered.
// It preserves completed nodes and their evidence, only replanning remaining work.
func (p *Planner) Replan(ctx context.Context, task domain.Task, reason string) ([]domain.Node, []domain.AcceptanceCriterion, domain.GraphRevision, error) {
	// Replanning may change the route, but never the task goal or its required
	// acceptance contract. In particular, the only unfinished node is often the
	// verify node; using that node's description here would turn verification
	// into the new objective and lose the user's original request.
	existingCriteria, err := p.store.AcceptanceCriteria(ctx, task.ID)
	if err != nil {
		return nil, nil, domain.GraphRevision{}, err
	}

	proposal := p.complexGraph(task.Objective)

	// Validate
	if err := taskgraph.ValidateGraphProposal(proposal, 12); err != nil {
		return nil, nil, domain.GraphRevision{}, fmt.Errorf("replan validation failed: %w", err)
	}

	// Convert to nodes (new IDs, but preserve completed ones)
	newNodes, criteria := p.proposalToNodes(task.ID, proposal)
	existingNodes, err := p.store.Nodes(ctx, task.ID)
	if err != nil {
		return nil, nil, domain.GraphRevision{}, err
	}
	newNodes = reuseCompletedContextNodes(newNodes, existingNodes)
	newNodes, criteria = preserveRequiredCriteria(newNodes, criteria, existingCriteria)

	// Save revision
	version, err := p.store.NextGraphRevision(ctx, task.ID)
	if err != nil {
		return nil, nil, domain.GraphRevision{}, err
	}

	nodesJSON, _ := json.Marshal(newNodes)
	revision := domain.GraphRevision{
		ID:           storage.NewID("gr"),
		TaskID:       task.ID,
		Version:      version,
		Reason:       reason,
		PlannerModel: "planner-v1",
		NodesJSON:    string(nodesJSON),
	}
	return newNodes, criteria, revision, nil
}

// Replanning must not repeat durable context acquisition that already
// succeeded. Planning and execution nodes are intentionally regenerated so
// they can react to the failure that triggered the new revision.
func reuseCompletedContextNodes(planned, existing []domain.Node) []domain.Node {
	reusable := map[domain.NodeKind]bool{
		domain.NodeLoadHist: true,
		domain.NodeRetrieve: true,
		domain.NodeResearch: true,
	}
	completed := make(map[domain.NodeKind]domain.Node)
	for _, node := range existing {
		kind := effectiveNodeKind(node)
		if node.Status == "completed" && reusable[kind] {
			completed[kind] = node
		}
	}
	replacements := make(map[string]string)
	for i := range planned {
		old, ok := completed[planned[i].Kind]
		if !ok {
			continue
		}
		replacements[planned[i].ID] = old.ID
		old.DependsOn = append([]string(nil), planned[i].DependsOn...)
		old.Outputs = append([]domain.OutputContract(nil), planned[i].Outputs...)
		planned[i] = old
	}
	for i := range planned {
		for j, dependency := range planned[i].DependsOn {
			if replacement := replacements[dependency]; replacement != "" {
				planned[i].DependsOn[j] = replacement
			}
		}
	}
	return planned
}

// preserveRequiredCriteria carries the stable IDs and definitions of every
// required criterion into the replacement graph. Newly planned criteria with
// the same type and description are replaced by the existing contract rather
// than duplicated. Node IDs may change between revisions, so retained criteria
// are rebound to the corresponding new node.
func preserveRequiredCriteria(nodes []domain.Node, planned, existing []domain.AcceptanceCriterion) ([]domain.Node, []domain.AcceptanceCriterion) {
	type target struct {
		nodeID string
	}
	requiredKeys := make(map[string]bool)
	for _, c := range existing {
		if c.Required {
			requiredKeys[criterionKey(c)] = true
		}
	}

	targetByKey := make(map[string]target)
	result := make([]domain.AcceptanceCriterion, 0, len(planned)+len(existing))
	for _, c := range planned {
		key := criterionKey(c)
		if requiredKeys[key] {
			targetByKey[key] = target{nodeID: c.NodeID}
			continue
		}
		result = append(result, c)
	}

	defaultNodeID := ""
	for _, n := range nodes {
		if n.Kind == domain.NodeExecute {
			defaultNodeID = n.ID
			break
		}
	}
	if defaultNodeID == "" && len(nodes) > 0 {
		defaultNodeID = nodes[len(nodes)-1].ID
	}
	for _, c := range existing {
		if !c.Required {
			continue
		}
		key := criterionKey(c)
		nodeID := targetByKey[key].nodeID
		if nodeID == "" {
			nodeID = defaultNodeID
		}
		c.NodeID = nodeID
		result = append(result, c)
	}

	// Rebuild graph references after removing duplicate planned criteria.
	byNode := make(map[string][]string)
	for _, c := range result {
		byNode[c.NodeID] = append(byNode[c.NodeID], c.ID)
	}
	for i := range nodes {
		nodes[i].CriteriaIDs = byNode[nodes[i].ID]
	}
	return nodes, result
}

func criterionKey(c domain.AcceptanceCriterion) string {
	return string(c.Type) + "\x00" + c.Description
}

func (p *Planner) simpleGraph(objective string) domain.GraphProposal {
	return domain.GraphProposal{
		GoalSummary: objective,
		Nodes: []domain.NodeProposal{
			{TempID: "load_hist", Kind: domain.NodeLoadHist, Title: "加载会话历史", Description: "在上下文预算内加载并压缩历史"},
			{TempID: "retrieve", Kind: domain.NodeRetrieve, Title: "检索长期记忆", Description: "按任务语义检索记忆"},
			{TempID: "execute", Kind: domain.NodeExecute, Title: "执行目标", Description: objective, DependsOn: []string{"load_hist", "retrieve"},
				Outputs: []domain.OutputContract{{Label: "result", Description: "执行结果"}},
				Criteria: []domain.AcceptanceCriterion{
					{Type: domain.CriterionModelRubric, Description: "模型返回非空交付结果", Required: true, Config: json.RawMessage(`{}`)},
					{Type: domain.CriterionToolResult, Description: "不存在未处理的工具错误", Required: true, Config: json.RawMessage(`{}`)},
				},
			},
			{TempID: "verify", Kind: domain.NodeVerify, Title: "验收结果", Description: "按验收标准验证交付结果", DependsOn: []string{"execute"}},
		},
	}
}

func (p *Planner) complexGraph(objective string) domain.GraphProposal {
	return domain.GraphProposal{
		GoalSummary:   objective,
		OpenQuestions: []string{"任务的具体范围和交付物待探查后明确"},
		Nodes: []domain.NodeProposal{
			{TempID: "load_hist", Kind: domain.NodeLoadHist, Title: "加载会话历史", Description: "在上下文预算内加载并压缩历史"},
			{TempID: "retrieve", Kind: domain.NodeRetrieve, Title: "检索长期记忆", Description: "按任务语义检索记忆"},
			{TempID: "research", Kind: domain.NodeResearch, Title: "调研当前状态", Description: "检查工作区、依赖和现有条件", DependsOn: []string{"load_hist", "retrieve"},
				Outputs: []domain.OutputContract{{Label: "workspace_state", Description: "当前工作区和项目状态"}},
			},
			{TempID: "plan", Kind: domain.NodePlan, Title: "制定执行计划", Description: "基于调研结果制定详细执行方案", DependsOn: []string{"research"},
				Outputs: []domain.OutputContract{{Label: "execution_plan", Description: "详细执行计划"}},
			},
			{TempID: "execute", Kind: domain.NodeExecute, Title: "执行变更", Description: objective, DependsOn: []string{"plan"},
				Outputs: []domain.OutputContract{{Label: "deliverables", Description: "交付物"}},
				Criteria: []domain.AcceptanceCriterion{
					{Type: domain.CriterionModelRubric, Description: "模型返回非空交付结果", Required: true, Config: json.RawMessage(`{}`)},
					{Type: domain.CriterionToolResult, Description: "不存在未处理的工具错误", Required: true, Config: json.RawMessage(`{}`)},
				},
			},
			{TempID: "verify", Kind: domain.NodeVerify, Title: "验收结果", Description: "按验收标准验证交付结果", DependsOn: []string{"execute"}},
		},
	}
}

// defaultRisk returns a sensible risk level based on node kind.
func defaultRisk(k domain.NodeKind) string {
	switch k {
	case domain.NodeExecute:
		return "medium"
	case domain.NodeApproval:
		return "high"
	default:
		return "low"
	}
}

func (p *Planner) proposalToNodes(taskID string, proposal domain.GraphProposal) ([]domain.Node, []domain.AcceptanceCriterion) {
	// Build temp_id -> real_id mapping
	idMap := map[string]string{}
	for _, np := range proposal.Nodes {
		idMap[np.TempID] = storage.NewID("node")
	}

	nodes := make([]domain.Node, 0, len(proposal.Nodes))
	criteria := make([]domain.AcceptanceCriterion, 0)
	for _, np := range proposal.Nodes {
		depIDs := make([]string, 0, len(np.DependsOn))
		for _, d := range np.DependsOn {
			if realID, ok := idMap[d]; ok {
				depIDs = append(depIDs, realID)
			}
		}
		riskLevel := np.RiskLevel
		if riskLevel == "" {
			riskLevel = defaultRisk(np.Kind)
		}
		maxAttempts := 2
		if np.Kind == domain.NodeExecute || np.Kind == domain.NodeVerify {
			maxAttempts = 3
		}

		criteriaIDs := make([]string, 0, len(np.Criteria))
		for _, c := range np.Criteria {
			if c.ID == "" {
				c.ID = storage.NewID("ac")
			}
			c.TaskID = taskID
			c.NodeID = idMap[np.TempID]
			if c.Status == "" {
				c.Status = "pending"
			}
			criteriaIDs = append(criteriaIDs, c.ID)
			criteria = append(criteria, c)
		}

		nodes = append(nodes, domain.Node{
			ID:          idMap[np.TempID],
			TaskID:      taskID,
			Kind:        np.Kind,
			Title:       np.Title,
			Description: np.Description,
			Status:      "pending",
			DependsOn:   depIDs,
			Outputs:     np.Outputs,
			CriteriaIDs: criteriaIDs,
			RiskLevel:   riskLevel,
			MaxAttempts: maxAttempts,
		})
	}
	return nodes, criteria
}

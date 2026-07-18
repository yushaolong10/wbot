package task

import (
	"fmt"
	"strings"

	"github.com/wbot-dev/wbot/internal/domain"
)

// ValidateGraphProposal checks a planner-generated graph proposal against runtime policy.
func ValidateGraphProposal(proposal domain.GraphProposal, maxNodes int) error {
	if maxNodes <= 0 {
		maxNodes = 12
	}
	if len(proposal.Nodes) == 0 {
		return fmt.Errorf("graph proposal must contain at least one node")
	}
	if len(proposal.Nodes) > maxNodes {
		return fmt.Errorf("graph proposal has %d nodes, max allowed is %d", len(proposal.Nodes), maxNodes)
	}

	tempIDs := map[string]bool{}
	for _, np := range proposal.Nodes {
		if np.TempID == "" {
			return fmt.Errorf("every node proposal must have a temp_id")
		}
		if tempIDs[np.TempID] {
			return fmt.Errorf("duplicate temp_id %q", np.TempID)
		}
		tempIDs[np.TempID] = true
	}

	// Validate node kinds
	for _, np := range proposal.Nodes {
		if !isValidNodeKind(np.Kind) {
			return fmt.Errorf("node %q has invalid kind %q", np.TempID, np.Kind)
		}
		if np.RiskLevel != "" && !isValidRisk(np.RiskLevel) {
			return fmt.Errorf("node %q has invalid risk level %q", np.TempID, np.RiskLevel)
		}
		for _, criterion := range np.Criteria {
			if !isSupportedCriterionType(criterion.Type) {
				return fmt.Errorf("node %q uses unsupported acceptance criterion type %q", np.TempID, criterion.Type)
			}
		}
	}

	// Validate dependencies
	for _, np := range proposal.Nodes {
		for _, dep := range np.DependsOn {
			if !tempIDs[dep] {
				return fmt.Errorf("node %q depends on unknown temp_id %q", np.TempID, dep)
			}
		}
	}

	// Cycle detection on temp IDs
	visiting, done := map[string]bool{}, map[string]bool{}
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			return fmt.Errorf("graph proposal contains a cycle at %s", id)
		}
		if done[id] {
			return nil
		}
		visiting[id] = true
		for _, np := range proposal.Nodes {
			if np.TempID == id {
				for _, d := range np.DependsOn {
					if e := visit(d); e != nil {
						return e
					}
				}
				break
			}
		}
		visiting[id] = false
		done[id] = true
		return nil
	}
	for id := range tempIDs {
		if e := visit(id); e != nil {
			return e
		}
	}

	// Each execute node must have at least one output or criterion
	for _, np := range proposal.Nodes {
		if np.Kind == domain.NodeExecute {
			if len(np.Outputs) == 0 && len(np.Criteria) == 0 {
				return fmt.Errorf("execute node %q must declare at least one output or acceptance criterion", np.TempID)
			}
		}
	}

	// Verify that node kind dependencies are logical
	for _, np := range proposal.Nodes {
		for _, dep := range np.DependsOn {
			depKind := findKind(proposal.Nodes, dep)
			if np.Kind == domain.NodeVerify && depKind == domain.NodeVerify {
				return fmt.Errorf("verify node %q depends on another verify node %q — verify nodes should depend on execute nodes", np.TempID, dep)
			}
		}
	}

	return nil
}

func isSupportedCriterionType(t domain.CriterionType) bool {
	switch t {
	case domain.CriterionFileExists, domain.CriterionFileContains, domain.CriterionCommand,
		domain.CriterionArtifact, domain.CriterionToolResult, domain.CriterionModelRubric:
		return true
	}
	return false
}

func isValidNodeKind(k domain.NodeKind) bool {
	switch k {
	case domain.NodeResearch, domain.NodePlan, domain.NodeExecute,
		domain.NodeVerify, domain.NodeApproval, domain.NodeWait,
		domain.NodeLoadHist, domain.NodeRetrieve:
		return true
	}
	return false
}

func isValidRisk(r string) bool {
	switch strings.ToLower(r) {
	case "low", "medium", "high", "critical":
		return true
	}
	return false
}

func findKind(nodes []domain.NodeProposal, tempID string) domain.NodeKind {
	for _, n := range nodes {
		if n.TempID == tempID {
			return n.Kind
		}
	}
	return ""
}

// ReplanCondition describes why a replan was triggered.
type ReplanCondition string

const (
	ReplanNodeFailed      ReplanCondition = "node_failed"
	ReplanAssumptionWrong ReplanCondition = "assumption_wrong"
	ReplanUserChanged     ReplanCondition = "user_changed_goal"
	ReplanBudgetExhausted ReplanCondition = "budget_exhausted"
	ReplanEvaluatorReplan ReplanCondition = "evaluator_replan"
)

// ShouldReplan checks whether the current task state warrants replanning.
func ShouldReplan(node domain.Node, condition ReplanCondition, replanAfterFailures int) bool {
	if replanAfterFailures <= 0 {
		replanAfterFailures = 2
	}
	switch condition {
	case ReplanNodeFailed:
		return node.Attempt >= replanAfterFailures
	case ReplanAssumptionWrong, ReplanUserChanged, ReplanEvaluatorReplan:
		return true
	case ReplanBudgetExhausted:
		return true
	}
	return false
}

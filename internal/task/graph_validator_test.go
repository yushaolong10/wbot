package task

import (
	"testing"

	"github.com/wbot-dev/wbot/internal/domain"
)

func TestValidateGraphProposal(t *testing.T) {
	// Valid proposal
	valid := domain.GraphProposal{
		GoalSummary: "test task",
		Nodes: []domain.NodeProposal{
			{TempID: "research", Kind: domain.NodeResearch, Title: "调研"},
			{TempID: "execute", Kind: domain.NodeExecute, Title: "执行", DependsOn: []string{"research"},
				Outputs: []domain.OutputContract{{Label: "result"}},
			},
			{TempID: "verify", Kind: domain.NodeVerify, Title: "验收", DependsOn: []string{"execute"}},
		},
	}
	if err := ValidateGraphProposal(valid, 12); err != nil {
		t.Fatalf("valid proposal should pass: %v", err)
	}

	// Missing temp_id
	invalid := domain.GraphProposal{
		GoalSummary: "test",
		Nodes:       []domain.NodeProposal{{Kind: domain.NodeExecute, Title: "exec"}},
	}
	if err := ValidateGraphProposal(invalid, 12); err == nil {
		t.Fatal("expected error for missing temp_id")
	}

	// Execute node without outputs
	noOutput := domain.GraphProposal{
		GoalSummary: "test",
		Nodes: []domain.NodeProposal{
			{TempID: "exec", Kind: domain.NodeExecute, Title: "exec", DependsOn: []string{}},
		},
	}
	if err := ValidateGraphProposal(noOutput, 12); err == nil {
		t.Fatal("expected error for execute node without outputs")
	}

	// Cycle
	cycle := domain.GraphProposal{
		GoalSummary: "test",
		Nodes: []domain.NodeProposal{
			{TempID: "a", Kind: domain.NodeResearch, Title: "a", DependsOn: []string{"b"}},
			{TempID: "b", Kind: domain.NodeExecute, Title: "b", DependsOn: []string{"a"},
				Outputs: []domain.OutputContract{{Label: "out"}},
			},
		},
	}
	if err := ValidateGraphProposal(cycle, 12); err == nil {
		t.Fatal("expected error for cycle")
	}

	// Missing dependency
	missingDep := domain.GraphProposal{
		GoalSummary: "test",
		Nodes: []domain.NodeProposal{
			{TempID: "a", Kind: domain.NodeExecute, Title: "a", DependsOn: []string{"nonexistent"},
				Outputs: []domain.OutputContract{{Label: "out"}},
			},
		},
	}
	if err := ValidateGraphProposal(missingDep, 12); err == nil {
		t.Fatal("expected error for missing dependency")
	}
}

func TestValidateGraphProposalMaxNodes(t *testing.T) {
	tooMany := domain.GraphProposal{GoalSummary: "test"}
	for i := 0; i < 15; i++ {
		tooMany.Nodes = append(tooMany.Nodes, domain.NodeProposal{
			TempID: string(rune('a' + i)), Kind: domain.NodeResearch, Title: "n",
		})
	}
	if err := ValidateGraphProposal(tooMany, 12); err == nil {
		t.Fatal("expected error for too many nodes")
	}
}

func TestShouldReplan(t *testing.T) {
	node := domain.Node{Attempt: 2, MaxAttempts: 2}
	if !ShouldReplan(node, ReplanNodeFailed, 2) {
		t.Fatal("should replan when attempts >= replanAfterFailures")
	}

	node2 := domain.Node{Attempt: 1, MaxAttempts: 3}
	if ShouldReplan(node2, ReplanNodeFailed, 2) {
		t.Fatal("should NOT replan when attempts < replanAfterFailures")
	}

	if !ShouldReplan(domain.Node{}, ReplanUserChanged, 2) {
		t.Fatal("should replan on user change")
	}
}

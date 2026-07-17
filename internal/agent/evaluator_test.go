package agent

import (
	"github.com/wbot-dev/wbot/internal/domain"
	"testing"
)

func TestEvaluatorRejectsUnresolvedToolError(t *testing.T) {
	v := evaluate("完成", []domain.Message{{Role: "tool", Content: `{"Status":"error"}`}})
	if v.Passed {
		t.Fatal("should reject unresolved tool error")
	}
	if !evaluate("完成", nil).Passed {
		t.Fatal("valid result rejected")
	}
}

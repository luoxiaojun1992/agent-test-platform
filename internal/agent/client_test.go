package agent

import (
	"strings"
	"testing"
)

func TestNormalizeRejectsOversizedEvidence(t *testing.T) {
	t.Parallel()

	call := ToolCall{Sequence: 1, CallID: "q1", ToolName: "mysql.query", Arguments: map[string]any{"sql": strings.Repeat("x", maxArgumentBytes)}, Result: &ToolResult{Status: "SUCCESS"}}
	if _, err := normalizeToolCalls([]ToolCall{call}); err == nil {
		t.Fatal("oversized arguments were accepted")
	}
}

func TestNormalizeRejectsForgedResultDigest(t *testing.T) {
	t.Parallel()

	call := ToolCall{
		Sequence: 1, CallID: "q1", ToolName: "mysql.query", Arguments: map[string]any{"sql": "SELECT 1"},
		Result: &ToolResult{Status: "SUCCESS", Output: map[string]any{"value": 1}, SHA256: strings.Repeat("a", 64)},
	}
	if _, err := normalizeToolCalls([]ToolCall{call}); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("forged digest error = %v", err)
	}
}

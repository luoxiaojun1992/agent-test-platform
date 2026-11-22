package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// tightPolicy leaves room for the base request plus about two 1.5 KB Tool groups.
func tightPolicy() CompactionPolicy {
	return CompactionPolicy{
		ContextWindow: 4_000, ReservedOutputTokens: 1024, SafetyMarginTokens: 1024,
		KeepRecentGroups: 1, MaxSummaryBytes: 512, MaxCompactions: 100,
		MaxRetainedRecords: 2, MaxRollupBytes: 4096,
	}
}

func largeResult(fill byte, size int) string {
	return `{"rows":"` + strings.Repeat(string(fill), size) + `"}`
}

func TestSessionKeepsNewestClosedGroupVerbatimWhileOlderGroupCompacts(t *testing.T) {
	t.Parallel()

	session := NewSession()
	appendToolGroup(t, session, "call-old", largeResult('o', 2_200), json.RawMessage(`{"n":"old"}`))
	appendToolGroup(t, session, "call-new", largeResult('n', 2_200), json.RawMessage(`{"n":"new"}`))

	request, report, err := session.Prepare(sessionBaseRequest(), tightPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if report.CompactedGroups != 1 {
		t.Fatalf("compacted %d groups, want only the older one", report.CompactedGroups)
	}
	var newestVerbatim, oldestVerbatim bool
	for _, message := range request.Messages {
		if message.Content == nil {
			continue
		}
		newestVerbatim = newestVerbatim || *message.Content == largeResult('n', 2_200)
		oldestVerbatim = oldestVerbatim || *message.Content == largeResult('o', 2_200)
	}
	if !newestVerbatim || oldestVerbatim {
		t.Fatalf("newest verbatim=%v oldest verbatim=%v", newestVerbatim, oldestVerbatim)
	}
}

// A recent group is compacted only when it alone exceeds the budget. The newest group here fits
// alone with some room to spare, but the compacted record of the older group pushes the request
// over the limit. The Session must then fail explicitly, never summarize the newest result.
func TestSessionDoesNotCompactRecentGroupThatFitsAlone(t *testing.T) {
	t.Parallel()

	policy := testCompactionPolicy()
	base := sessionBaseRequest()
	limit, err := policy.inputTokenLimit(base.MaxOutputTokens)
	if err != nil {
		t.Fatal(err)
	}
	aloneTokens := func(size int) int {
		session := NewSession()
		appendToolGroup(t, session, "call-new", largeResult('n', size), json.RawMessage(`{"n":"new"}`))
		return EstimateTokens(requestWithSession(base, nil, nil, session.groups))
	}
	newestSize := 0
	for low, high := 0, 2*limit; low <= high; {
		middle := (low + high) / 2
		if aloneTokens(middle) <= limit-50 {
			newestSize = middle
			low = middle + 1
		} else {
			high = middle - 1
		}
	}
	if newestSize == 0 {
		t.Fatal("no newest group size fits alone")
	}

	session := NewSession()
	appendToolGroup(t, session, "call-old", largeResult('o', 2*limit), json.RawMessage(`{"n":"old"}`))
	appendToolGroup(t, session, "call-new", largeResult('n', newestSize), json.RawMessage(`{"n":"new"}`))

	_, report, err := session.Prepare(base, policy)
	if !errors.Is(err, ErrContextBudgetExceeded) {
		t.Fatalf("error = %v, want ErrContextBudgetExceeded", err)
	}
	if report.CompactedGroups != 1 {
		t.Fatalf("compacted %d groups, want only the older one before failing", report.CompactedGroups)
	}
}

func TestSessionFoldsOldRecordsIntoBoundedRollup(t *testing.T) {
	t.Parallel()

	run := func() (*Session, CompletionRequest) {
		session := NewSession()
		var request CompletionRequest
		for round := 0; round < 30; round++ {
			callID := fmt.Sprintf("call-%02d", round)
			appendToolGroup(t, session, callID, largeResult(byte('a'+round%26), 1_500), json.RawMessage(fmt.Sprintf(`{"round":%d}`, round)))
			prepared, report, err := session.Prepare(sessionBaseRequest(), tightPolicy())
			if err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
			if report.EstimatedAfter > report.InputTokenLimit {
				t.Fatalf("round %d over budget: %#v", round, report)
			}
			request = prepared
		}
		return session, request
	}

	session, request := run()
	if len(session.compacted) > tightPolicy().MaxRetainedRecords {
		t.Fatalf("retained %d compacted records, limit %d", len(session.compacted), tightPolicy().MaxRetainedRecords)
	}
	if session.rollup == nil || session.rollup.FoldedGroups == 0 {
		t.Fatalf("no records were folded into the rollup: %#v", session.rollup)
	}
	encoded, err := json.Marshal(session.rollup)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > tightPolicy().MaxRollupBytes {
		t.Fatalf("rollup has %d bytes, limit %d", len(encoded), tightPolicy().MaxRollupBytes)
	}

	_, replayed := run()
	first, _ := json.Marshal(request)
	second, _ := json.Marshal(replayed)
	if string(first) != string(second) {
		t.Fatal("compaction is not deterministic for the same Tool history")
	}
}

func TestFoldRecordRejectsRollupBeyondItsBound(t *testing.T) {
	t.Parallel()

	record := compactedGroup{SourceDigest: digestBytes([]byte("group")), ToolCalls: []compactedToolCall{{CallID: "call-1", ToolName: "read"}}}
	grow := func(previous, _ json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"padding":"` + strings.Repeat("p", 2_000) + `"}`), nil
	}
	if _, err := foldRecord(nil, grow, record, 1_024); !errors.Is(err, ErrContextBudgetExceeded) {
		t.Fatalf("oversized state error = %v, want ErrContextBudgetExceeded", err)
	}
	failing := func(previous, _ json.RawMessage) (json.RawMessage, error) {
		return nil, errors.New("coverage fragmented")
	}
	if _, err := foldRecord(nil, failing, record, 1_024); !errors.Is(err, ErrContextBudgetExceeded) {
		t.Fatalf("fold failure error = %v, want ErrContextBudgetExceeded", err)
	}
}

func TestToolLoopCarriesStateLedgerAndTokenUsage(t *testing.T) {
	t.Parallel()

	usage := TokenUsage{InputTokens: 100, OutputTokens: 10, TotalTokens: 110, Available: true}
	responses := make([]CompletionResponse, 0, 7)
	executor := &loopExecutor{contents: map[string]string{}, summaries: map[string]json.RawMessage{}}
	for index := 0; index < 6; index++ {
		callID := fmt.Sprintf("call-%d", index)
		responses = append(responses, CompletionResponse{
			StopReason: "tool_calls", Usage: usage,
			ToolCalls: []ModelToolCall{{ID: callID, Name: "tool", Arguments: json.RawMessage(`{}`)}},
		})
		executor.contents[callID] = largeResult(byte('a'+index), 1_500)
		executor.summaries[callID] = json.RawMessage(fmt.Sprintf(`{"call":%q}`, callID))
	}
	responses = append(responses, CompletionResponse{StopReason: "stop", Content: "complete", Usage: usage})
	completer := &loopCompleter{responses: responses}

	folds := 0
	stateFold := func(previous, _ json.RawMessage) (json.RawMessage, error) {
		folds++
		return json.RawMessage(fmt.Sprintf(`{"folds":%d}`, folds)), nil
	}
	loop, err := NewToolLoop(completer, executor, nil, ToolLoopConfig{
		ModelAlias: "generation", MaxModelCalls: 10, MaxToolCalls: 10,
		CompactionPolicy: tightPolicy(), StateFold: stateFold,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := sessionBaseRequest()
	request.ToolChoice = ToolChoice{Mode: ToolChoiceAuto}
	result, err := loop.Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Usage.InputTokens != 700 || result.Usage.OutputTokens != 70 || result.Usage.TotalTokens != 770 || !result.Usage.Available {
		t.Fatalf("usage = %#v", result.Usage)
	}
	if folds == 0 {
		t.Fatal("state ledger was never folded")
	}
	last := completer.requests[len(completer.requests)-1]
	found := false
	for _, message := range last.Messages {
		found = found || (message.Content != nil && strings.Contains(*message.Content, `"state":{"folds":`))
	}
	if !found {
		t.Fatal("final request does not carry the folded state ledger")
	}
}

func TestNewToolLoopRequiresBoundedRetentionAndRollup(t *testing.T) {
	t.Parallel()

	noRetention := tightPolicy()
	noRetention.MaxRetainedRecords = 0
	smallRollup := tightPolicy()
	smallRollup.MaxRollupBytes = 128
	for _, policy := range []CompactionPolicy{noRetention, smallRollup} {
		if _, err := NewToolLoop(&loopCompleter{}, &loopExecutor{}, nil, ToolLoopConfig{
			ModelAlias: "generation", MaxModelCalls: 2, MaxToolCalls: 2, CompactionPolicy: policy,
		}); err == nil {
			t.Fatalf("policy %#v was accepted", policy)
		}
	}
}

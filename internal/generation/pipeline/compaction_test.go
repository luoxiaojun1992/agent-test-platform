package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agent-test-platform/internal/generation"
	"agent-test-platform/internal/llm"
)

func TestCompactionRecorderPersistsFencedDraft(t *testing.T) {
	t.Parallel()

	store := &fakeStore{}
	request := generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusGeneratingTestCases,
		RepairRound: 1, Attempt: 2, Lease: validLease(),
	}
	recorder, err := NewCompactionRecorder(store, request)
	if err != nil {
		t.Fatal(err)
	}
	report := llm.CompactionReport{
		PolicyVersion: llm.CompactionPolicyVersion, EstimatedBefore: 10_000,
		EstimatedAfter: 5_000, InputTokenLimit: 8_000, CompactedGroups: 2,
		SourceDigests: []string{strings.Repeat("a", 64), strings.Repeat("b", 64)},
	}
	if err := recorder.RecordCompaction(context.Background(), report); err != nil {
		t.Fatal(err)
	}
	if store.saved == nil || store.saved.Kind != generation.ArtifactKindCompactionReport || store.saved.Key != "generating_test_cases-r1-a2-c1" || store.saved.TTL != draftTTL {
		t.Fatalf("draft = %#v", store.saved)
	}
	if store.lease != request.Lease || len(store.saved.SHA256) != 64 {
		t.Fatalf("lease=%#v digest=%s", store.lease, store.saved.SHA256)
	}
	var saved llm.CompactionReport
	if err := json.Unmarshal(store.saved.ContentJSON, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.CompactedGroups != report.CompactedGroups || len(saved.SourceDigests) != 2 {
		t.Fatalf("saved report = %#v", saved)
	}
}

func TestCompactionRecorderRejectsInvalidReport(t *testing.T) {
	t.Parallel()

	recorder, err := NewCompactionRecorder(&fakeStore{}, generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusGeneratingTestCases,
		Attempt: 1, Lease: validLease(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.RecordCompaction(context.Background(), llm.CompactionReport{
		PolicyVersion: llm.CompactionPolicyVersion, EstimatedBefore: 5_000,
		EstimatedAfter: 5_000, CompactedGroups: 1,
	}); err == nil {
		t.Fatal("non-reducing compaction report was accepted")
	}
}

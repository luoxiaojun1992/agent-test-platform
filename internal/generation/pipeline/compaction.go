package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"agent-test-platform/internal/generation"
	"agent-test-platform/internal/llm"
)

type CompactionRecorder struct {
	store       Store
	jobID       string
	stage       generation.JobStatus
	repairRound int
	attempt     int
	lease       generation.WorkLease
	next        int
}

func NewCompactionRecorder(store Store, request generation.StageRequest) (*CompactionRecorder, error) {
	if store == nil || request.JobID == "" || !request.Stage.Valid() || request.Attempt < 1 || request.RepairRound < 0 || request.RepairRound > generation.MaxRepairRounds {
		return nil, fmt.Errorf("store and complete Stage request are required")
	}
	if request.Lease.WorkItemID == "" || request.Lease.Owner == "" || request.Lease.FencingToken == 0 {
		return nil, fmt.Errorf("complete Stage lease is required")
	}
	return &CompactionRecorder{
		store: store, jobID: request.JobID, stage: request.Stage, repairRound: request.RepairRound,
		attempt: request.Attempt, lease: request.Lease,
	}, nil
}

func (recorder *CompactionRecorder) RecordCompaction(ctx context.Context, report llm.CompactionReport) error {
	if report.PolicyVersion != llm.CompactionPolicyVersion || report.CompactedGroups < 1 || report.EstimatedAfter >= report.EstimatedBefore {
		return fmt.Errorf("invalid compaction report")
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("encode compaction report: %w", err)
	}
	canonical, digest, err := generation.CanonicalJSON(encoded)
	if err != nil {
		return fmt.Errorf("canonicalize compaction report: %w", err)
	}
	recorder.next++
	key := fmt.Sprintf("%s-r%d-a%d-c%d", strings.ToLower(string(recorder.stage)), recorder.repairRound, recorder.attempt, recorder.next)
	validation, err := json.Marshal(map[string]any{
		"policy_version":      llm.CompactionPolicyVersion,
		"source_digest_count": len(report.SourceDigests),
		"valid":               true,
	})
	if err != nil {
		return fmt.Errorf("encode compaction validation: %w", err)
	}
	if err := recorder.store.SaveGenerationArtifact(ctx, recorder.jobID, recorder.lease, generation.ArtifactDraft{
		Kind: generation.ArtifactKindCompactionReport, Key: key, ContentJSON: canonical,
		SHA256: digest, Validation: validation, TTL: draftTTL,
	}); err != nil {
		return err
	}
	return nil
}

var _ llm.CompactionRecorder = (*CompactionRecorder)(nil)

package pipeline

import (
	"context"
	"errors"

	"agent-test-platform/internal/generation"
)

// reuseStoredOutput makes generation and repair stages idempotent (docs/architecture.md 19.2). If a
// stage's output is already stored - an earlier attempt saved it, and its transition was not
// committed before the Lease was taken over - the stage reports that output's transition without
// calling the model again, so the Job continues from the output that is actually stored.
func (processor *Processor) reuseStoredOutput(ctx context.Context, request generation.StageRequest) (generation.StageResult, bool, error) {
	kind, key, event, ok := storedOutputOf(request)
	if !ok {
		return generation.StageResult{}, false, nil
	}
	_, err := processor.store.LoadGenerationArtifact(ctx, request.JobID, kind, key)
	if errors.Is(err, generation.ErrNotFound) {
		return generation.StageResult{}, false, nil
	}
	if err != nil {
		return generation.StageResult{}, false, err
	}
	return generation.StageResult{Transition: generation.TransitionInput{Event: event}}, true, nil
}

// storedOutputOf names the artifact a generation or repair stage produces and the event that
// follows it. A repair stage produces the artifact of the next round.
func storedOutputOf(request generation.StageRequest) (kind, key string, event generation.JobEvent, ok bool) {
	switch request.Stage {
	case generation.StatusGeneratingScenario:
		return generation.ArtifactKindScenario, scenarioDraftKey(request.RepairRound), generation.EventScenarioStored, true
	case generation.StatusRepairingScenario:
		return generation.ArtifactKindScenario, scenarioDraftKey(request.RepairRound + 1), generation.EventRepairGenerated, true
	case generation.StatusGeneratingDataset:
		return generation.ArtifactKindDataset, datasetDraftKey(request.RepairRound), generation.EventDatasetStored, true
	case generation.StatusRepairingDataset:
		return generation.ArtifactKindDataset, datasetDraftKey(request.RepairRound + 1), generation.EventRepairGenerated, true
	case generation.StatusGeneratingTestCases:
		return generation.ArtifactKindTestCaseManifest, testCaseManifestKey(request.RepairRound), generation.EventCandidatesGenerated, true
	case generation.StatusRepairingTestCases:
		return generation.ArtifactKindTestCaseManifest, testCaseManifestKey(request.RepairRound + 1), generation.EventRepairGenerated, true
	default:
		return "", "", "", false
	}
}

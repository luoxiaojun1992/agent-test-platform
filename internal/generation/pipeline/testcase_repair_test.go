package pipeline

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"

	"agent-test-platform/internal/generation"
	"agent-test-platform/internal/llm"

	"github.com/DATA-DOG/go-sqlmock"
)

func seedManifest(t *testing.T, store *fakeTestCaseStore, round int, manifest testCaseManifest) {
	t.Helper()
	content, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	store.seed(generation.ArtifactKindTestCaseManifest, testCaseManifestKey(round), content, nil)
}

func TestValidateTestCasesAcceptsWhenCountMatches(t *testing.T) {
	t.Parallel()

	store, target, _ := newTestCaseHarness(t, 1)
	seedManifest(t, store, 0, testCaseManifest{Accepted: []string{"cancel-paid-order"}})
	processor := newTestCaseProcessor(t, store, &fakeTestCaseGateway{}, target)

	result, err := processor.Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusValidatingTestCases, RepairRound: 0, Attempt: 1, Lease: validLease(),
	})
	if err != nil || result.Failure != nil {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	if result.Transition.Event != generation.EventValidationAccepted || result.Transition.AcceptedTestCaseCount != 1 {
		t.Fatalf("transition = %#v", result.Transition)
	}
}

func TestValidateTestCasesRepairsWhenRetryable(t *testing.T) {
	t.Parallel()

	store, target, _ := newTestCaseHarness(t, 2)
	seedManifest(t, store, 0, testCaseManifest{
		Accepted: []string{"cancel-paid-order"},
		Rejected: []testCaseRejection{{ClientCaseID: "reject-shipped-order", Errors: []generation.ValidationError{
			{Code: "SCHEMA_INVALID", Path: "$.prompt", Message: "bad", Retryable: true},
		}}},
	})
	processor := newTestCaseProcessor(t, store, &fakeTestCaseGateway{}, target)

	result, err := processor.Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusValidatingTestCases, RepairRound: 0, Attempt: 1, Lease: validLease(),
	})
	if err != nil || result.Failure != nil {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	if result.Transition.Event != generation.EventValidationRejected || !result.Transition.AllErrorsRetryable {
		t.Fatalf("transition = %#v", result.Transition)
	}
}

func TestValidateTestCasesFailsWhenNotRetryable(t *testing.T) {
	t.Parallel()

	store, target, _ := newTestCaseHarness(t, 2)
	seedManifest(t, store, 0, testCaseManifest{
		Accepted: []string{"cancel-paid-order"},
		Rejected: []testCaseRejection{{ClientCaseID: "reject-shipped-order", Errors: []generation.ValidationError{
			{Code: "REFERENCE_SCOPE_MISMATCH", Path: "$", Message: "bad", Retryable: false},
		}}},
	})
	processor := newTestCaseProcessor(t, store, &fakeTestCaseGateway{}, target)

	result, err := processor.Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusValidatingTestCases, RepairRound: 0, Attempt: 1, Lease: validLease(),
	})
	if err != nil || result.Failure != nil {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	if result.Transition.Event != generation.EventValidationRejected || result.Transition.AllErrorsRetryable {
		t.Fatalf("transition = %#v", result.Transition)
	}
}

func TestRepairTestCasesGeneratesReplacementAndAdvancesManifest(t *testing.T) {
	t.Parallel()

	store, target, mock := newTestCaseHarness(t, 2)
	accepted := candidateFixture(t, "cancel-paid-order")
	store.seed(generation.ArtifactKindTestCase, "cancel-paid-order", accepted, []byte(`{"prompt_fingerprint":"`+digestHex([]byte("please cancel order o10001."))+`","oracle_fingerprint":"`+digestHex([]byte("oracle"))+`"}`))
	seedManifest(t, store, 0, testCaseManifest{
		Accepted: []string{"cancel-paid-order"},
		Rejected: []testCaseRejection{{ClientCaseID: "reject-shipped-order", Errors: []generation.ValidationError{
			{Code: "SCHEMA_INVALID", Path: "$.prompt", Message: "prompt too short", Retryable: true},
		}}},
	})
	mock.ExpectQuery(regexp.QuoteMeta("select `status` from orders where id = 'O10001'")).WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("PAID"))

	replacement := candidateFixture(t, "reject-shipped-order")
	var replacementDocument map[string]any
	if err := json.Unmarshal(replacement, &replacementDocument); err != nil {
		t.Fatal(err)
	}
	replacementDocument["prompt"] = "Please cancel my most recent order, O10001, right away."
	replacement, err := json.Marshal(replacementDocument)
	if err != nil {
		t.Fatal(err)
	}
	gateway := &fakeTestCaseGateway{responses: []llm.CompletionResponse{toolCallResponse("call-1", replacement)}}
	processor := newTestCaseProcessor(t, store, gateway, target)

	result, err := processor.Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusRepairingTestCases, RepairRound: 0, Attempt: 1, Lease: validLease(),
	})
	if err != nil || result.Failure != nil {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	if result.Transition.Event != generation.EventRepairGenerated {
		t.Fatalf("transition = %#v", result.Transition)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	manifest, err := processor.loadTestCaseManifest(context.Background(), pipelineJobID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Accepted) != 2 || len(manifest.Rejected) != 0 {
		t.Fatalf("next manifest = %#v", manifest)
	}
}

func TestRepairTestCasesCarriesManifestForwardWhenNothingRejected(t *testing.T) {
	t.Parallel()

	store, target, _ := newTestCaseHarness(t, 1)
	seedManifest(t, store, 0, testCaseManifest{Accepted: []string{"cancel-paid-order"}})
	processor := newTestCaseProcessor(t, store, &fakeTestCaseGateway{}, target)

	result, err := processor.Process(context.Background(), generation.StageRequest{
		JobID: pipelineJobID, Stage: generation.StatusRepairingTestCases, RepairRound: 0, Attempt: 1, Lease: validLease(),
	})
	if err != nil || result.Failure != nil || result.Transition.Event != generation.EventRepairGenerated {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	manifest, err := processor.loadTestCaseManifest(context.Background(), pipelineJobID, 1)
	if err != nil || len(manifest.Accepted) != 1 {
		t.Fatalf("manifest = %#v, err = %v", manifest, err)
	}
}

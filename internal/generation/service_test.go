package generation

import (
	"context"
	"strings"
	"testing"
)

type fakeJobStore struct {
	input        CreateJobInput
	job          Job
	reviewID     string
	reviewInput  ReviewInput
	draftID      string
	packageID    string
	profileInput CreateDatabaseProfileInput
	listCursor   string
	listLimit    int
}

const apiProfileID = "01K4Z6Y7X8W9V0T1S2R3Q4P5NC"

func (store *fakeJobStore) CreateGenerationJob(_ context.Context, input CreateJobInput) (Job, error) {
	store.input = input
	return store.job, nil
}
func (store *fakeJobStore) GetGenerationJob(context.Context, string) (Job, error) {
	return store.job, nil
}
func (store *fakeJobStore) CancelGenerationJob(context.Context, string, string) (Job, error) {
	return store.job, nil
}
func (store *fakeJobStore) ReviewGenerationJob(_ context.Context, id, _ string, input ReviewInput) (Job, error) {
	store.reviewID, store.reviewInput = id, input
	return store.job, nil
}

func (store *fakeJobStore) GetGenerationJobDraft(_ context.Context, id string) (DraftView, error) {
	store.draftID = id
	return DraftView{JobID: id, Kind: KindScenario}, nil
}

func (store *fakeJobStore) GetGenerationPackage(_ context.Context, id string) (Package, error) {
	store.packageID = id
	return Package{JobID: id}, nil
}

func (store *fakeJobStore) CreateDatabaseProfile(_ context.Context, input CreateDatabaseProfileInput) (DatabaseProfile, error) {
	store.profileInput = input
	return DatabaseProfile{ID: apiProfileID, Name: input.Request.Name}, nil
}

func (store *fakeJobStore) ListDatabaseProfiles(_ context.Context, cursor string, limit int) ([]DatabaseProfile, string, error) {
	store.listCursor, store.listLimit = cursor, limit
	return nil, "", nil
}

func TestServicePassesDraftReadThrough(t *testing.T) {
	t.Parallel()

	store := &fakeJobStore{}
	service, err := NewService(store, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	draft, err := service.GetGenerationJobDraft(context.Background(), "job")
	if err != nil || draft.JobID != "job" || store.draftID != "job" {
		t.Fatalf("draft=%#v id=%q err=%v", draft, store.draftID, err)
	}
}

func TestServiceCarriesManualReviewFlag(t *testing.T) {
	t.Parallel()

	store := &fakeJobStore{job: Job{ID: "job"}}
	service, err := NewService(store, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	for document, want := range map[string]bool{
		`{"validation_db_ids":["0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90"],"description":"Generate tests."}`:                               false,
		`{"validation_db_ids":["0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90"],"description":"Generate tests.","require_manual_review":true}`:  true,
		`{"validation_db_ids":["0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90"],"description":"Generate tests.","require_manual_review":false}`: false,
	} {
		if _, err := service.CreateGenerationJob(context.Background(), CreateJobInput{IdempotencyKey: "request-key", Request: []byte(document)}); err != nil {
			t.Fatal(err)
		}
		if store.input.RequireManualReview != want {
			t.Fatalf("%s: RequireManualReview = %v, want %v", document, store.input.RequireManualReview, want)
		}
	}
}

func TestServiceValidatesReviewInput(t *testing.T) {
	t.Parallel()

	store := &fakeJobStore{job: Job{ID: "job"}}
	service, err := NewService(store, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []ReviewInput{
		{Decision: ""},
		{Decision: "MAYBE"},
		{Decision: ReviewReject, Note: strings.Repeat("x", MaxReviewNoteBytes+1)},
	} {
		if _, err := service.ReviewGenerationJob(context.Background(), "job", "review-key", input); err == nil {
			t.Errorf("invalid review input %#v was accepted", input)
		}
	}
	if store.reviewID != "" {
		t.Fatalf("invalid review reached the store: %q", store.reviewID)
	}
	approval := ReviewInput{Decision: ReviewApprove, Note: "looks right"}
	if _, err := service.ReviewGenerationJob(context.Background(), "job", "review-key", approval); err != nil {
		t.Fatal(err)
	}
	if store.reviewID != "job" || store.reviewInput != approval {
		t.Fatalf("store review = %q %#v", store.reviewID, store.reviewInput)
	}
}

func TestServicePinsGeneratorConfig(t *testing.T) {
	t.Parallel()

	digest := strings.Repeat("a", 64)
	store := &fakeJobStore{job: Job{ID: "job"}}
	service, err := NewService(store, digest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateGenerationJob(context.Background(), CreateJobInput{IdempotencyKey: "request-key", Request: []byte(`{"validation_db_ids":["0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90"],"description":"Generate tests."}`)}); err != nil {
		t.Fatal(err)
	}
	if store.input.GeneratorConfigSHA256 != digest {
		t.Fatalf("config digest = %q", store.input.GeneratorConfigSHA256)
	}
	if !sha256Pattern.MatchString(store.input.RequestSHA256) {
		t.Fatalf("request digest = %q", store.input.RequestSHA256)
	}
}

func TestServiceCanonicalizesRequestDigest(t *testing.T) {
	t.Parallel()

	store := &fakeJobStore{}
	service, err := NewService(store, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateGenerationJob(context.Background(), CreateJobInput{Request: []byte(`{"validation_db_ids":["0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90"],"description":"Generate tests.","domain":"orders"}`)}); err != nil {
		t.Fatal(err)
	}
	first := store.input.RequestSHA256
	if _, err := service.CreateGenerationJob(context.Background(), CreateJobInput{Request: []byte(`{"validation_db_ids":["0b6d6f3e-2c4a-4f8e-9a1b-7c5d3e2f1a90"],"domain":"orders","description":"Generate tests."}`)}); err != nil {
		t.Fatal(err)
	}
	if store.input.RequestSHA256 != first {
		t.Fatalf("equivalent requests have different digests: %s != %s", first, store.input.RequestSHA256)
	}
}

func TestNewServiceRejectsInvalidConfig(t *testing.T) {
	t.Parallel()

	if _, err := NewService(&fakeJobStore{}, "invalid"); err == nil {
		t.Fatal("invalid config digest was accepted")
	}
}

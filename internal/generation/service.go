package generation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

var (
	ErrNotFound            = errors.New("resource not found")
	ErrConflict            = errors.New("resource conflict")
	ErrIdempotencyConflict = errors.New("idempotency key reused with different request")
	// ErrInvalidRequest reports a Generation request that fails normalization; the wrapped message names the rule.
	ErrInvalidRequest = errors.New("generation request is invalid")
	// ErrTargetNotFound reports a validation MySQL ID that the request names but that is not registered.
	ErrTargetNotFound = errors.New("validation MySQL is not registered")
)

var sha256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var failureCodePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,63}$`)

const MaxDescriptionTokens = 1_500

type CreateJobInput struct {
	IdempotencyKey          string
	Kind                    JobKind
	InputScenarioRevisionID *string
	InputDatasetRevisionID  *string
	Request                 json.RawMessage
	RequestSHA256           string
	DescriptionTokens       int
	RequestedTestCaseCount  int
	GeneratorConfigSHA256   string
	TargetAgentID           *string
	GenerateDataset         bool
	DatabaseProfileID       *string
	ValidationDBIDs         []string
	RequireManualReview     bool
}

type JobFailure struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details,omitempty"`
}

func (failure JobFailure) Validate() error {
	if !failureCodePattern.MatchString(failure.Code) {
		return fmt.Errorf("failure code is invalid")
	}
	if strings.TrimSpace(failure.Message) == "" || len(failure.Message) > 2000 {
		return fmt.Errorf("failure message is required and must not exceed 2000 bytes")
	}
	if _, err := json.Marshal(failure.Details); err != nil {
		return fmt.Errorf("failure details are not JSON-serializable: %w", err)
	}
	return nil
}

type Job struct {
	ID                      string      `json:"id"`
	Kind                    JobKind     `json:"job_kind"`
	PipelineID              *string     `json:"pipeline_id,omitempty"`
	InputScenarioRevisionID *string     `json:"input_scenario_revision_id,omitempty"`
	InputDatasetRevisionID  *string     `json:"input_dataset_revision_id,omitempty"`
	Status                  JobStatus   `json:"status"`
	RequireManualReview     bool        `json:"require_manual_review"`
	RequestedTestCaseCount  int         `json:"requested_test_case_count"`
	AcceptedTestCaseCount   int         `json:"accepted_test_case_count,omitempty"`
	RepairRound             int         `json:"repair_round,omitempty"`
	Error                   *JobFailure `json:"error,omitempty"`
	CreatedAt               time.Time   `json:"created_at"`
	UpdatedAt               time.Time   `json:"updated_at"`
}

type JobStore interface {
	CreateGenerationJob(context.Context, CreateJobInput) (Job, error)
	GetGenerationJob(context.Context, string) (Job, error)
	CancelGenerationJob(context.Context, string, string) (Job, error)
	ReviewGenerationJob(context.Context, string, string, ReviewInput) (Job, error)
	GetGenerationJobDraft(context.Context, string) (DraftView, error)
	GetGenerationPackage(context.Context, string) (Package, error)
	CreateDatabaseProfile(context.Context, CreateDatabaseProfileInput) (DatabaseProfile, error)
	ListDatabaseProfiles(context.Context, string, int) ([]DatabaseProfile, string, error)
}

// DraftView is the read-only content a reviewer needs to decide on a Job paused in
// AWAITING_REVIEW, shaped by the Job's kind: exactly one of Scenario, Dataset, or TestCases is set.
type DraftView struct {
	JobID                   string            `json:"job_id"`
	Kind                    JobKind           `json:"job_kind"`
	InputScenarioRevisionID *string           `json:"input_scenario_revision_id,omitempty"`
	InputDatasetRevisionID  *string           `json:"input_dataset_revision_id,omitempty"`
	Scenario                json.RawMessage   `json:"scenario,omitempty"`
	Dataset                 json.RawMessage   `json:"dataset,omitempty"`
	TestCases               []json.RawMessage `json:"test_cases,omitempty"`
}

type ReviewDecision string

const (
	ReviewApprove ReviewDecision = "APPROVE"
	ReviewReject  ReviewDecision = "REJECT"

	MaxReviewNoteBytes = 2000
)

// ReviewInput is a human reviewer's decision on a Job paused in AWAITING_REVIEW.
type ReviewInput struct {
	Decision ReviewDecision
	Note     string
}

func (input ReviewInput) Validate() error {
	if input.Decision != ReviewApprove && input.Decision != ReviewReject {
		return fmt.Errorf("review decision must be APPROVE or REJECT")
	}
	if len(input.Note) > MaxReviewNoteBytes {
		return fmt.Errorf("review note must not exceed %d bytes", MaxReviewNoteBytes)
	}
	return nil
}

type Service struct {
	store                 JobStore
	generatorConfigSHA256 string
}

func NewService(store JobStore, generatorConfigSHA256 string) (*Service, error) {
	if store == nil || !sha256Pattern.MatchString(generatorConfigSHA256) {
		return nil, fmt.Errorf("job store and generator config SHA-256 are required")
	}
	return &Service{store: store, generatorConfigSHA256: generatorConfigSHA256}, nil
}

func (service *Service) CreateGenerationJob(ctx context.Context, input CreateJobInput) (Job, error) {
	normalized, err := NormalizeRequest(input.Request)
	if err != nil {
		return Job{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	_, digest, err := CanonicalJSON(input.Request)
	if err != nil {
		return Job{}, fmt.Errorf("digest generation request: %w", err)
	}
	input.RequestSHA256 = digest
	input.GeneratorConfigSHA256 = service.generatorConfigSHA256
	input.TargetAgentID = normalized.TargetAgentID
	input.GenerateDataset = normalized.GenerateDataset
	input.DatabaseProfileID = normalized.DatabaseProfileID
	input.ValidationDBIDs = normalized.ValidationDBIDs
	input.RequireManualReview = normalized.RequireManualReview
	return service.store.CreateGenerationJob(ctx, input)
}

func (service *Service) GetGenerationJob(ctx context.Context, id string) (Job, error) {
	return service.store.GetGenerationJob(ctx, id)
}

func (service *Service) CancelGenerationJob(ctx context.Context, id, idempotencyKey string) (Job, error) {
	return service.store.CancelGenerationJob(ctx, id, idempotencyKey)
}

func (service *Service) ReviewGenerationJob(ctx context.Context, id, idempotencyKey string, input ReviewInput) (Job, error) {
	if err := input.Validate(); err != nil {
		return Job{}, err
	}
	return service.store.ReviewGenerationJob(ctx, id, idempotencyKey, input)
}

func (service *Service) GetGenerationJobDraft(ctx context.Context, id string) (DraftView, error) {
	return service.store.GetGenerationJobDraft(ctx, id)
}

// GetGenerationPackage returns the published Generation Package of the Pipeline the Job belongs to.
func (service *Service) GetGenerationPackage(ctx context.Context, id string) (Package, error) {
	return service.store.GetGenerationPackage(ctx, id)
}

// CanonicalJSON returns compact JSON with stable object-key ordering and its SHA-256 digest.
func CanonicalJSON(document json.RawMessage) (json.RawMessage, string, error) {
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, "", err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, "", fmt.Errorf("trailing JSON data")
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(canonical)
	return canonical, hex.EncodeToString(digest[:]), nil
}

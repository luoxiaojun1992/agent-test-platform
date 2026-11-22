package generation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
)

const NormalizedRequestSchemaVersion = "1.1"

const (
	ArtifactKindNormalizedRequest = "NORMALIZED_REQUEST"
	ArtifactKindCompactionReport  = "COMPACTION_REPORT"
	ArtifactKindScenario          = "SCENARIO"
	ArtifactKindScenarioID        = "SCENARIO_ID"
	ArtifactKindDataset           = "DATASET"
	ArtifactKindDatasetExecution  = "DATASET_EXECUTION"
	ArtifactKindTestCase          = "TEST_CASE"
	ArtifactKindTestCaseManifest  = "TEST_CASE_MANIFEST"
	ArtifactKindCriticReview      = "CRITIC_REVIEW"
)

// Critic scopes name the stage whose accepted output a CRITIC_REVIEW artifact covers. Each
// scope's key is CriticReviewKey(scope, round), shared by the pipeline (which writes it) and the
// publisher (which reads it into the Job's provenance).
const (
	CriticScopeScenario  = "scenario"
	CriticScopeDataset   = "dataset"
	CriticScopeTestCases = "test-cases"
)

// DescriptionTokenizerVersion names the conservative estimator that computes description_tokens
// for every request (docs/technical/02-llm-gateway.md, unknown tokenizer fallback). It is recorded
// as the Tokenizer Version in the Generation Manifest.
const DescriptionTokenizerVersion = "conservative-estimator/v1"

// MaxRequestTargets bounds how many registered validation MySQL instances one request may bind a Dataset to.
const MaxRequestTargets = 8

// CriticReviewKey is the artifact key of a stage round's accepted Critic review.
func CriticReviewKey(scope string, round int) string {
	return fmt.Sprintf("%s-round-%d", scope, round)
}

var (
	capabilityPattern  = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	requestULIDPattern = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)
	targetIDPattern    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

type DifficultyRange struct {
	Min string `json:"min"`
	Max string `json:"max"`
}

type NormalizedRequest struct {
	SchemaVersion        string           `json:"schema_version"`
	Description          string           `json:"description"`
	TargetAgentVersionID *string          `json:"target_agent_version_id"`
	GenerateDataset      bool             `json:"generate_dataset"`
	ValidationDBIDs      []string         `json:"validation_db_ids"`
	DatabaseProfileID    *string          `json:"database_profile_id"`
	Domain               *string          `json:"domain"`
	AgentMode            string           `json:"agent_mode"`
	Capabilities         *[]string        `json:"capabilities"`
	Difficulty           *DifficultyRange `json:"difficulty"`
	TestCaseCount        int              `json:"test_case_count"`
	Coverage             *[]string        `json:"coverage"`
	AutoActivate         bool             `json:"auto_activate"`
	RequireManualReview  bool             `json:"require_manual_review"`
}

type RequestSnapshot struct {
	Document               json.RawMessage
	SHA256                 string
	RequestedTestCaseCount int
}

type ArtifactDraft struct {
	Kind        string
	Key         string
	ContentJSON json.RawMessage
	ContentText *string
	SHA256      string
	Validation  json.RawMessage
	TTL         time.Duration
}

type ArtifactSnapshot struct {
	Kind        string
	Key         string
	ContentJSON json.RawMessage
	ContentText *string
	SHA256      string
	Validation  json.RawMessage
}

// ScenarioRevisionSnapshot is an already-published Scenario Revision, read by a DATASET or
// TEST_CASES kind Job through its input_scenario_revision_id.
type ScenarioRevisionSnapshot struct {
	ID              string
	ScenarioID      string
	ContentJSON     json.RawMessage
	SHA256          string
	LifecycleStatus string
}

// DatasetRevisionSnapshot is an already-published Dataset Revision, read by a TEST_CASES kind
// Job through its input_dataset_revision_id.
type DatasetRevisionSnapshot struct {
	ID                 string
	DatasetID          string
	ScenarioRevisionID string
	SchemaSQL          string
	SeedSQL            string
	ConstraintsJSON    json.RawMessage
	MetadataJSON       json.RawMessage
	SHA256             string
	ExecutionDatabase  string
	LifecycleStatus    string
	ValidationDBIDs    []string
}

// RevisionLifecycleUsable reports whether a published Revision may be used as a DATASET or
// TEST_CASES Job's input: only VALIDATED or ACTIVE, never STAGING or DEPRECATED
// (docs/technical/01-storage-and-workflows.md section 3).
func RevisionLifecycleUsable(status string) bool {
	return status == "VALIDATED" || status == "ACTIVE"
}

// RevisionLifecycleKnown reports whether a stored lifecycle_status is one of the Asset Revision
// states in docs/architecture.md section 15.2.
func RevisionLifecycleKnown(status string) bool {
	switch status {
	case "STAGING", "VALIDATED", "ACTIVE", "DEPRECATED":
		return true
	default:
		return false
	}
}

func (draft ArtifactDraft) Validate() error {
	if strings.TrimSpace(draft.Kind) == "" || len(draft.Kind) > 32 || strings.TrimSpace(draft.Key) == "" || len(draft.Key) > 128 {
		return fmt.Errorf("artifact kind and key are required")
	}
	hasJSON := len(draft.ContentJSON) > 0
	hasText := draft.ContentText != nil
	if hasJSON == hasText {
		return fmt.Errorf("artifact requires exactly one JSON or text content value")
	}
	if hasJSON && !json.Valid(draft.ContentJSON) {
		return fmt.Errorf("artifact JSON content is invalid")
	}
	if hasText && *draft.ContentText == "" {
		return fmt.Errorf("artifact text content is empty")
	}
	if !sha256Pattern.MatchString(draft.SHA256) {
		return fmt.Errorf("artifact SHA-256 is invalid")
	}
	if len(draft.Validation) > 0 && !json.Valid(draft.Validation) {
		return fmt.Errorf("artifact validation is invalid JSON")
	}
	if draft.TTL <= 0 || draft.TTL > 7*24*time.Hour {
		return fmt.Errorf("artifact TTL outside (0,168h]")
	}
	return nil
}

type generationRequestDocument struct {
	Description          string           `json:"description"`
	TargetAgentVersionID *string          `json:"target_agent_version_id"`
	GenerateDataset      *bool            `json:"generate_dataset"`
	ValidationDBIDs      []string         `json:"validation_db_ids"`
	DatabaseProfileID    *string          `json:"database_profile_id"`
	Domain               *string          `json:"domain"`
	AgentMode            string           `json:"agent_mode"`
	Capabilities         *[]string        `json:"capabilities"`
	Difficulty           *DifficultyRange `json:"difficulty"`
	TestCaseCount        int              `json:"test_case_count"`
	Coverage             *[]string        `json:"coverage"`
	AutoActivate         *bool            `json:"auto_activate"`
	RequireManualReview  *bool            `json:"require_manual_review"`

	// Routing fields select the Job kind and its input Revisions; they are Job columns, not normalized request content.
	JobKind                 string  `json:"job_kind"`
	InputScenarioRevisionID *string `json:"input_scenario_revision_id"`
	InputDatasetRevisionID  *string `json:"input_dataset_revision_id"`
}

func NormalizeRequest(document json.RawMessage) (NormalizedRequest, error) {
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.DisallowUnknownFields()
	var input generationRequestDocument
	if err := decoder.Decode(&input); err != nil {
		return NormalizedRequest{}, fmt.Errorf("decode generation request: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return NormalizedRequest{}, fmt.Errorf("generation request has trailing JSON data")
	}
	if strings.TrimSpace(input.Description) == "" {
		return NormalizedRequest{}, fmt.Errorf("description must contain non-whitespace text")
	}
	if input.Domain != nil && strings.TrimSpace(*input.Domain) == "" {
		return NormalizedRequest{}, fmt.Errorf("domain must contain non-whitespace text")
	}
	if input.TargetAgentVersionID != nil && !requestULIDPattern.MatchString(*input.TargetAgentVersionID) {
		return NormalizedRequest{}, fmt.Errorf("target Agent Version ID is invalid")
	}
	targetSeen := make(map[string]struct{}, len(input.ValidationDBIDs))
	minimumTargets := 1
	if JobKind(input.JobKind) == KindScenario || JobKind(input.JobKind) == KindTestCases {
		// A Scenario needs no database, and Test Cases use the Targets their input Dataset is bound to.
		minimumTargets = 0
	}
	if len(input.ValidationDBIDs) < minimumTargets || len(input.ValidationDBIDs) > MaxRequestTargets {
		return NormalizedRequest{}, fmt.Errorf("validation_db_ids must name 1 to %d registered validation MySQL instances", MaxRequestTargets)
	}
	for _, targetID := range input.ValidationDBIDs {
		if !targetIDPattern.MatchString(targetID) {
			return NormalizedRequest{}, fmt.Errorf("validation_db_ids contains an invalid ID (a lowercase UUID is required)")
		}
		if _, repeated := targetSeen[targetID]; repeated {
			return NormalizedRequest{}, fmt.Errorf("validation_db_ids repeats %s", targetID)
		}
		targetSeen[targetID] = struct{}{}
	}
	generateDataset := true
	if input.GenerateDataset != nil {
		generateDataset = *input.GenerateDataset
	}
	if !generateDataset && (input.DatabaseProfileID == nil || !requestULIDPattern.MatchString(*input.DatabaseProfileID)) {
		return NormalizedRequest{}, fmt.Errorf("valid Database Profile ID is required when Dataset generation is disabled")
	}
	if generateDataset && input.DatabaseProfileID != nil {
		return NormalizedRequest{}, fmt.Errorf("Database Profile ID is not allowed when Dataset generation is enabled")
	}

	agentMode := input.AgentMode
	if agentMode == "" {
		agentMode = "TASK"
	}
	if agentMode != "ANSWER" && agentMode != "TASK" {
		return NormalizedRequest{}, fmt.Errorf("unknown agent mode %q", agentMode)
	}
	testCaseCount := input.TestCaseCount
	if testCaseCount == 0 {
		testCaseCount = 10
	}
	if testCaseCount < 1 || testCaseCount > 20 {
		return NormalizedRequest{}, fmt.Errorf("test case count outside [1,20]")
	}
	autoActivate := true
	if input.AutoActivate != nil {
		autoActivate = *input.AutoActivate
	}
	requireManualReview := input.RequireManualReview != nil && *input.RequireManualReview

	capabilities, err := normalizedSet(input.Capabilities, capabilityPattern.MatchString, "capability")
	if err != nil {
		return NormalizedRequest{}, err
	}
	coverage, err := normalizedSet(input.Coverage, validCoverage, "coverage")
	if err != nil {
		return NormalizedRequest{}, err
	}
	if input.Difficulty != nil {
		minimum, minimumOK := difficultyRank(input.Difficulty.Min)
		maximum, maximumOK := difficultyRank(input.Difficulty.Max)
		if !minimumOK || !maximumOK || minimum > maximum {
			return NormalizedRequest{}, fmt.Errorf("difficulty range is invalid")
		}
	}

	return NormalizedRequest{
		SchemaVersion: NormalizedRequestSchemaVersion,
		Description:   input.Description, TargetAgentVersionID: cloneString(input.TargetAgentVersionID),
		GenerateDataset: generateDataset, DatabaseProfileID: cloneString(input.DatabaseProfileID),
		ValidationDBIDs: append([]string{}, input.ValidationDBIDs...),
		Domain:          cloneString(input.Domain), AgentMode: agentMode,
		Capabilities: capabilities, Difficulty: cloneDifficulty(input.Difficulty),
		TestCaseCount: testCaseCount, Coverage: coverage, AutoActivate: autoActivate,
		RequireManualReview: requireManualReview,
	}, nil
}

func normalizedSet(input *[]string, valid func(string) bool, field string) (*[]string, error) {
	if input == nil {
		return nil, nil
	}
	values := append([]string(nil), (*input)...)
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !valid(value) {
			return nil, fmt.Errorf("%s value %q is invalid", field, value)
		}
		if _, exists := seen[value]; exists {
			return nil, fmt.Errorf("%s value %q is duplicated", field, value)
		}
		seen[value] = struct{}{}
	}
	sort.Strings(values)
	return &values, nil
}

func validCoverage(value string) bool {
	switch value {
	case "NORMAL", "EDGE_CASE", "AMBIGUOUS", "MULTI_STEP", "CONFLICT", "ADVERSARIAL":
		return true
	default:
		return false
	}
}

func difficultyRank(value string) (int, bool) {
	switch value {
	case "EASY":
		return 1, true
	case "MEDIUM":
		return 2, true
	case "HARD":
		return 3, true
	case "EXTREME":
		return 4, true
	default:
		return 0, false
	}
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneDifficulty(value *DifficultyRange) *DifficultyRange {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

package generation

import (
	"context"
	"encoding/json"
	"fmt"
)

type JobStatus string
type JobEvent string

// JobKind is the one kind of output a Generation Job produces. A Job never produces more than
// one kind; a full "description in, package out" flow chains three Jobs (see Generation Pipeline
// in docs/architecture.md 6.3) instead of one Job doing all three.
type JobKind string

const (
	KindScenario  JobKind = "SCENARIO"
	KindDataset   JobKind = "DATASET"
	KindTestCases JobKind = "TEST_CASES"
)

const (
	StatusPending         JobStatus = "PENDING"
	StatusValidatingInput JobStatus = "VALIDATING_INPUT"

	StatusGeneratingScenario JobStatus = "GENERATING_SCENARIO"
	StatusValidatingScenario JobStatus = "VALIDATING_SCENARIO"
	StatusRepairingScenario  JobStatus = "REPAIRING_SCENARIO"

	StatusGeneratingDataset JobStatus = "GENERATING_DATASET"
	StatusValidatingDataset JobStatus = "VALIDATING_DATASET"
	StatusRepairingDataset  JobStatus = "REPAIRING_DATASET"

	StatusGeneratingTestCases JobStatus = "GENERATING_TEST_CASES"
	StatusValidatingTestCases JobStatus = "VALIDATING_TEST_CASES"
	StatusRepairingTestCases  JobStatus = "REPAIRING_TEST_CASES"

	StatusAwaitingReview JobStatus = "AWAITING_REVIEW"
	StatusPublishing     JobStatus = "PUBLISHING"
	StatusCompleted      JobStatus = "COMPLETED"
	StatusFailed         JobStatus = "FAILED"
	StatusCancelled      JobStatus = "CANCELLED"

	EventInputAccepted       JobEvent = "INPUT_ACCEPTED"
	EventInputValidated      JobEvent = "INPUT_VALIDATED"
	EventScenarioStored      JobEvent = "SCENARIO_STORED"
	EventDatasetStored       JobEvent = "DATASET_STORED"
	EventCandidatesGenerated JobEvent = "CANDIDATES_GENERATED"
	EventValidationAccepted  JobEvent = "VALIDATION_ACCEPTED"
	EventValidationRejected  JobEvent = "VALIDATION_REJECTED"
	EventRepairGenerated     JobEvent = "REPAIR_GENERATED"
	EventReviewApproved      JobEvent = "REVIEW_APPROVED"
	EventReviewRejected      JobEvent = "REVIEW_REJECTED"
	EventPublishCommitted    JobEvent = "PUBLISH_COMMITTED"
	EventFatalFailure        JobEvent = "FATAL_FAILURE"
	EventCancel              JobEvent = "CANCEL"
)

const MaxRepairRounds = 2

type JobState struct {
	Kind                   JobKind
	Status                 JobStatus
	RequestedTestCaseCount int
	AcceptedTestCaseCount  int
	RepairRound            int
	RequireManualReview    bool
}

type TransitionInput struct {
	Event                 JobEvent
	AcceptedTestCaseCount int
	AllErrorsRetryable    bool
}

type WorkLease struct {
	WorkItemID   string
	Owner        string
	FencingToken uint64
	AttemptCount int
	MaxAttempts  int
}

type StageRequest struct {
	JobID       string
	Stage       JobStatus
	RepairRound int
	Attempt     int
	Payload     json.RawMessage
	Lease       WorkLease
	// InputScenarioRevisionID and InputDatasetRevisionID are set for DATASET/TEST_CASES kind
	// Jobs (see JobKind) and name an already-published upstream Revision a stage handler must
	// read instead of a same-Job draft artifact. Both are nil for SCENARIO kind Jobs.
	InputScenarioRevisionID *string
	InputDatasetRevisionID  *string
}

type StageResult struct {
	Transition TransitionInput
	Failure    *JobFailure
}

type StageProcessor interface {
	Process(context.Context, StageRequest) (StageResult, error)
}

// Transition returns a new Job state or rejects an invalid transition without mutation. All
// three JobKind values share this one template: VALIDATING_INPUT branches to the kind's own
// GENERATING_<KIND>; VALIDATING_<KIND> represents deterministic validation and Semantic Critic
// combined into one Accept/Reject decision; a retryable rejection with rounds left goes to
// REPAIRING_<KIND> and back; an accepted result goes to AWAITING_REVIEW when the Job requires
// manual review, otherwise straight to PUBLISHING.
func Transition(current JobState, input TransitionInput) (JobState, error) {
	if err := current.Validate(); err != nil {
		return current, err
	}
	if current.Terminal() {
		return current, fmt.Errorf("job status %s is terminal", current.Status)
	}
	if input.Event == EventFatalFailure {
		current.Status = StatusFailed
		return current, nil
	}
	if input.Event == EventCancel {
		if current.Status == StatusPublishing {
			return current, fmt.Errorf("publishing cannot be cancelled")
		}
		current.Status = StatusCancelled
		return current, nil
	}

	if current.Status == StatusValidatingInput && input.Event == EventInputValidated {
		next, ok := generatingStatusByKind[current.Kind]
		if !ok {
			return current, fmt.Errorf("unknown job kind %q", current.Kind)
		}
		current.Status = next
		return current, nil
	}

	if repairTarget, ok := repairingStatusByValidating[current.Status]; ok {
		switch input.Event {
		case EventValidationAccepted:
			if current.Status == StatusValidatingTestCases {
				if input.AcceptedTestCaseCount != current.RequestedTestCaseCount {
					return current, fmt.Errorf("accepted test cases %d do not equal requested %d", input.AcceptedTestCaseCount, current.RequestedTestCaseCount)
				}
				current.AcceptedTestCaseCount = input.AcceptedTestCaseCount
			}
			current.Status = tailStatus(current)
			return current, nil
		case EventValidationRejected:
			if !input.AllErrorsRetryable || current.RepairRound >= MaxRepairRounds {
				current.Status = StatusFailed
				return current, nil
			}
			current.Status = repairTarget
			return current, nil
		}
	}

	if validatingTarget, ok := validatingStatusByRepairing[current.Status]; ok && input.Event == EventRepairGenerated {
		current.RepairRound++
		current.Status = validatingTarget
		return current, nil
	}

	if current.Status == StatusAwaitingReview {
		switch input.Event {
		case EventReviewApproved:
			current.Status = StatusPublishing
			return current, nil
		case EventReviewRejected:
			current.Status = StatusFailed
			return current, nil
		}
	}

	if next, ok := fixedTransitions[transitionKey{Status: current.Status, Event: input.Event}]; ok {
		current.Status = next
		return current, nil
	}
	return current, fmt.Errorf("event %s is invalid from status %s", input.Event, current.Status)
}

// tailStatus is where an accepted VALIDATING_<KIND> result goes next: the optional manual
// review gate (docs/architecture.md 9.2) if the Job asked for it, otherwise straight to publish.
func tailStatus(state JobState) JobStatus {
	if state.RequireManualReview {
		return StatusAwaitingReview
	}
	return StatusPublishing
}

func (state JobState) Validate() error {
	if !knownJobStatus(state.Status) {
		return fmt.Errorf("unknown job status %q", state.Status)
	}
	if !knownJobKind(state.Kind) {
		return fmt.Errorf("unknown job kind %q", state.Kind)
	}
	if state.Kind == KindTestCases {
		if state.RequestedTestCaseCount < 1 || state.RequestedTestCaseCount > 20 {
			return fmt.Errorf("requested test case count outside [1,20]")
		}
		if state.AcceptedTestCaseCount < 0 || state.AcceptedTestCaseCount > state.RequestedTestCaseCount {
			return fmt.Errorf("accepted test case count is invalid")
		}
	} else if state.RequestedTestCaseCount != 0 || state.AcceptedTestCaseCount != 0 {
		return fmt.Errorf("test case counts only apply to %s jobs", KindTestCases)
	}
	if state.RepairRound < 0 || state.RepairRound > MaxRepairRounds {
		return fmt.Errorf("repair round outside [0,%d]", MaxRepairRounds)
	}
	if state.Kind == KindTestCases && requiresExactTestCaseCount(state.Status) && state.AcceptedTestCaseCount != state.RequestedTestCaseCount {
		return fmt.Errorf("status %s requires exact accepted test case count", state.Status)
	}
	return nil
}

func requiresExactTestCaseCount(status JobStatus) bool {
	switch status {
	case StatusAwaitingReview, StatusPublishing, StatusCompleted:
		return true
	default:
		return false
	}
}

func (state JobState) Terminal() bool {
	return state.Status == StatusCompleted || state.Status == StatusFailed || state.Status == StatusCancelled
}

type transitionKey struct {
	Status JobStatus
	Event  JobEvent
}

// generatingStatusByKind is where VALIDATING_INPUT branches per JobKind.
var generatingStatusByKind = map[JobKind]JobStatus{
	KindScenario:  StatusGeneratingScenario,
	KindDataset:   StatusGeneratingDataset,
	KindTestCases: StatusGeneratingTestCases,
}

// repairingStatusByValidating and validatingStatusByRepairing describe the three symmetric
// (VALIDATING_<KIND>, REPAIRING_<KIND>) pairs that replace the old Test-Case-only REPAIRING
// status: any of the three kinds can now repair a retryable rejection.
var repairingStatusByValidating = map[JobStatus]JobStatus{
	StatusValidatingScenario:  StatusRepairingScenario,
	StatusValidatingDataset:   StatusRepairingDataset,
	StatusValidatingTestCases: StatusRepairingTestCases,
}

var validatingStatusByRepairing = map[JobStatus]JobStatus{
	StatusRepairingScenario:  StatusValidatingScenario,
	StatusRepairingDataset:   StatusValidatingDataset,
	StatusRepairingTestCases: StatusValidatingTestCases,
}

var fixedTransitions = map[transitionKey]JobStatus{
	{StatusPending, EventInputAccepted}:                   StatusValidatingInput,
	{StatusGeneratingScenario, EventScenarioStored}:       StatusValidatingScenario,
	{StatusGeneratingDataset, EventDatasetStored}:         StatusValidatingDataset,
	{StatusGeneratingTestCases, EventCandidatesGenerated}: StatusValidatingTestCases,
	{StatusPublishing, EventPublishCommitted}:             StatusCompleted,
}

func knownJobStatus(status JobStatus) bool {
	switch status {
	case StatusPending, StatusValidatingInput,
		StatusGeneratingScenario, StatusValidatingScenario, StatusRepairingScenario,
		StatusGeneratingDataset, StatusValidatingDataset, StatusRepairingDataset,
		StatusGeneratingTestCases, StatusValidatingTestCases, StatusRepairingTestCases,
		StatusAwaitingReview, StatusPublishing, StatusCompleted, StatusFailed, StatusCancelled:
		return true
	default:
		return false
	}
}

func (status JobStatus) Valid() bool {
	return knownJobStatus(status)
}

func knownJobKind(kind JobKind) bool {
	switch kind {
	case KindScenario, KindDataset, KindTestCases:
		return true
	default:
		return false
	}
}

func (kind JobKind) Valid() bool {
	return knownJobKind(kind)
}

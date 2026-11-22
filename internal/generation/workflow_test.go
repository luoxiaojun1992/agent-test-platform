package generation

import "testing"

func requestedCountFor(kind JobKind) int {
	if kind == KindTestCases {
		return 3
	}
	return 0
}

func TestValidatingInputBranchesByKind(t *testing.T) {
	t.Parallel()

	cases := []struct {
		kind JobKind
		want JobStatus
	}{
		{KindScenario, StatusGeneratingScenario},
		{KindDataset, StatusGeneratingDataset},
		{KindTestCases, StatusGeneratingTestCases},
	}
	for _, tc := range cases {
		state := JobState{Kind: tc.kind, Status: StatusValidatingInput, RequestedTestCaseCount: requestedCountFor(tc.kind)}
		next, err := Transition(state, TransitionInput{Event: EventInputValidated})
		if err != nil || next.Status != tc.want {
			t.Fatalf("kind %s: state=%#v err=%v", tc.kind, next, err)
		}
	}
}

func TestGenerationHappyPathPerKind(t *testing.T) {
	t.Parallel()

	cases := []struct {
		kind          JobKind
		generated     JobStatus
		validating    JobStatus
		generatedStep JobEvent
	}{
		{KindScenario, StatusGeneratingScenario, StatusValidatingScenario, EventScenarioStored},
		{KindDataset, StatusGeneratingDataset, StatusValidatingDataset, EventDatasetStored},
		{KindTestCases, StatusGeneratingTestCases, StatusValidatingTestCases, EventCandidatesGenerated},
	}
	for _, tc := range cases {
		state := JobState{Kind: tc.kind, Status: StatusPending, RequestedTestCaseCount: requestedCountFor(tc.kind)}
		steps := []TransitionInput{
			{Event: EventInputAccepted},
			{Event: EventInputValidated},
			{Event: tc.generatedStep},
			{Event: EventValidationAccepted, AcceptedTestCaseCount: requestedCountFor(tc.kind)},
			{Event: EventPublishCommitted},
		}
		for _, input := range steps {
			var err error
			state, err = Transition(state, input)
			if err != nil {
				t.Fatalf("kind %s event %s: %v", tc.kind, input.Event, err)
			}
		}
		if state.Status != StatusCompleted || !state.Terminal() {
			t.Fatalf("kind %s: state = %#v", tc.kind, state)
		}
	}
}

func TestGenerationRepairAppliesToEveryKind(t *testing.T) {
	t.Parallel()

	cases := []struct {
		kind       JobKind
		validating JobStatus
		repairing  JobStatus
	}{
		{KindScenario, StatusValidatingScenario, StatusRepairingScenario},
		{KindDataset, StatusValidatingDataset, StatusRepairingDataset},
		{KindTestCases, StatusValidatingTestCases, StatusRepairingTestCases},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			state := JobState{Kind: tc.kind, Status: tc.validating, RequestedTestCaseCount: requestedCountFor(tc.kind)}
			for round := 1; round <= MaxRepairRounds; round++ {
				var err error
				state, err = Transition(state, TransitionInput{Event: EventValidationRejected, AllErrorsRetryable: true})
				if err != nil || state.Status != tc.repairing {
					t.Fatalf("begin repair %d: state=%#v err=%v", round, state, err)
				}
				state, err = Transition(state, TransitionInput{Event: EventRepairGenerated})
				if err != nil || state.Status != tc.validating || state.RepairRound != round {
					t.Fatalf("finish repair %d: state=%#v err=%v", round, state, err)
				}
			}
			state, err := Transition(state, TransitionInput{Event: EventValidationRejected, AllErrorsRetryable: true})
			if err != nil || state.Status != StatusFailed {
				t.Fatalf("exhausted repair: state=%#v err=%v", state, err)
			}
		})
	}
}

func TestGenerationRejectsPartialTestCaseCount(t *testing.T) {
	t.Parallel()

	state := JobState{Kind: KindTestCases, Status: StatusValidatingTestCases, RequestedTestCaseCount: 10}
	next, err := Transition(state, TransitionInput{Event: EventValidationAccepted, AcceptedTestCaseCount: 7})
	if err == nil || next != state {
		t.Fatalf("partial package transition: state=%#v err=%v", next, err)
	}
}

func TestGenerationManualReviewGate(t *testing.T) {
	t.Parallel()

	state := JobState{Kind: KindScenario, Status: StatusValidatingScenario}
	published, err := Transition(state, TransitionInput{Event: EventValidationAccepted})
	if err != nil || published.Status != StatusPublishing {
		t.Fatalf("auto-publish without review: state=%#v err=%v", published, err)
	}

	state.RequireManualReview = true
	awaiting, err := Transition(state, TransitionInput{Event: EventValidationAccepted})
	if err != nil || awaiting.Status != StatusAwaitingReview {
		t.Fatalf("await review: state=%#v err=%v", awaiting, err)
	}
	approved, err := Transition(awaiting, TransitionInput{Event: EventReviewApproved})
	if err != nil || approved.Status != StatusPublishing {
		t.Fatalf("approve: state=%#v err=%v", approved, err)
	}
	rejected, err := Transition(awaiting, TransitionInput{Event: EventReviewRejected})
	if err != nil || rejected.Status != StatusFailed {
		t.Fatalf("reject: state=%#v err=%v", rejected, err)
	}
	if _, err := Transition(awaiting, TransitionInput{Event: EventCancel}); err != nil {
		t.Fatalf("awaiting review should be cancellable: %v", err)
	}
}

func TestGenerationFatalAndNonRetryableFailures(t *testing.T) {
	t.Parallel()

	state := JobState{Kind: KindDataset, Status: StatusGeneratingDataset}
	next, err := Transition(state, TransitionInput{Event: EventFatalFailure})
	if err != nil || next.Status != StatusFailed {
		t.Fatalf("fatal failure: state=%#v err=%v", next, err)
	}
	state = JobState{Kind: KindTestCases, Status: StatusValidatingTestCases, RequestedTestCaseCount: 1}
	next, err = Transition(state, TransitionInput{Event: EventValidationRejected, AllErrorsRetryable: false})
	if err != nil || next.Status != StatusFailed {
		t.Fatalf("non-retryable failure: state=%#v err=%v", next, err)
	}
}

func TestGenerationRejectsInvalidStateOrEvent(t *testing.T) {
	t.Parallel()

	invalidStates := []JobState{
		{Kind: KindTestCases, Status: "UNKNOWN", RequestedTestCaseCount: 1},
		{Kind: "UNKNOWN", Status: StatusPending},
		{Kind: KindTestCases, Status: StatusPending, RequestedTestCaseCount: 0},
		{Kind: KindTestCases, Status: StatusPending, RequestedTestCaseCount: 1, AcceptedTestCaseCount: 2},
		{Kind: KindTestCases, Status: StatusPending, RequestedTestCaseCount: 1, RepairRound: 3},
		{Kind: KindTestCases, Status: StatusPublishing, RequestedTestCaseCount: 2, AcceptedTestCaseCount: 1},
		{Kind: KindScenario, Status: StatusPending, RequestedTestCaseCount: 1},
	}
	for _, state := range invalidStates {
		if _, err := Transition(state, TransitionInput{Event: EventInputAccepted}); err == nil {
			t.Errorf("invalid state passed: %#v", state)
		}
	}
	valid := JobState{Kind: KindTestCases, Status: StatusPending, RequestedTestCaseCount: 1}
	if next, err := Transition(valid, TransitionInput{Event: EventDatasetStored}); err == nil || next != valid {
		t.Fatalf("invalid event changed state: %#v err=%v", next, err)
	}
}

func TestGenerationCancellationAndTerminalRules(t *testing.T) {
	t.Parallel()

	cancellable := []JobState{
		{Kind: KindTestCases, Status: StatusPending, RequestedTestCaseCount: 1},
		{Kind: KindTestCases, Status: StatusValidatingInput, RequestedTestCaseCount: 1},
		{Kind: KindScenario, Status: StatusGeneratingScenario},
		{Kind: KindScenario, Status: StatusValidatingScenario},
		{Kind: KindScenario, Status: StatusRepairingScenario},
		{Kind: KindDataset, Status: StatusGeneratingDataset},
		{Kind: KindDataset, Status: StatusValidatingDataset},
		{Kind: KindDataset, Status: StatusRepairingDataset},
		{Kind: KindTestCases, Status: StatusGeneratingTestCases, RequestedTestCaseCount: 1},
		{Kind: KindTestCases, Status: StatusValidatingTestCases, RequestedTestCaseCount: 1},
		{Kind: KindTestCases, Status: StatusRepairingTestCases, RequestedTestCaseCount: 1},
		{Kind: KindTestCases, Status: StatusAwaitingReview, RequestedTestCaseCount: 1, AcceptedTestCaseCount: 1},
	}
	for _, state := range cancellable {
		next, err := Transition(state, TransitionInput{Event: EventCancel})
		if err != nil || next.Status != StatusCancelled {
			t.Errorf("cancel from %s/%s: state=%#v err=%v", state.Kind, state.Status, next, err)
		}
	}
	publishing := JobState{Kind: KindTestCases, Status: StatusPublishing, RequestedTestCaseCount: 1, AcceptedTestCaseCount: 1}
	if _, err := Transition(publishing, TransitionInput{Event: EventCancel}); err == nil {
		t.Fatal("publishing was cancelled")
	}
	for _, terminal := range []JobStatus{StatusCompleted, StatusFailed, StatusCancelled} {
		state := JobState{Kind: KindTestCases, Status: terminal, RequestedTestCaseCount: 1}
		if terminal == StatusCompleted {
			state.AcceptedTestCaseCount = 1
		}
		if _, err := Transition(state, TransitionInput{Event: EventFatalFailure}); err == nil {
			t.Errorf("terminal state %s accepted transition", terminal)
		}
	}
}

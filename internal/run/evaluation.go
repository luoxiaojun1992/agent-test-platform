package run

import "encoding/json"

const (
	StatusEvaluationFailed = "EVALUATION_FAILED"
	EvaluationCompleted    = "COMPLETED"
	EvaluationFailed       = "FAILED"
	EvaluationPending      = "PENDING"
	EvaluationRunning      = "RUNNING"
	EvaluationCancelled    = "CANCELLED"
	DefaultJudgeAlias      = "default-judge"
)

// EvaluationJob is the persisted evidence the Judge needs to score one turn of a Test Run. The
// judge pulls the authoritative session context from the Target through the paginated context API
// (bounded by UntilSequence), so only the session_id, the turn number, and the Agent's call
// configuration are carried here.
type EvaluationJob struct {
	RunID         string
	NodeID        string
	Iteration     int
	Outcome       string
	CaseJSON      json.RawMessage
	PipelineJSON  json.RawMessage
	TurnNo        int
	TotalTurns    int
	SessionID     string
	UntilSequence int
	Agent         AgentSpec
	AttemptNo     int
	JudgeAlias    string
}

// EvaluationResult is one completed Judge attempt.
type EvaluationResult struct {
	Iteration         int
	TurnNo            int
	Provider          string
	Model             string
	Alias             string
	PromptVersion     string
	ConfigSHA256      string
	BaseURL           string
	ConfigJSON        json.RawMessage
	RequestJSON       json.RawMessage
	ResponseJSON      json.RawMessage
	CriteriaJSON      json.RawMessage
	FailureCategories json.RawMessage
	LLMScore          float64
	FinalScore        float64
	Pass              bool
	ProviderRequestID string
	InputTokens       *int64
	OutputTokens      *int64
}

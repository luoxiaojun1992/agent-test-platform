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

// EvaluationJob is the persisted evidence the Judge needs for one Test Run. The judge pulls the
// authoritative session context from the Target through the paginated context API, so only the
// session_id and the Agent's call configuration are carried here, not the collected Tool calls.
type EvaluationJob struct {
	RunID       string
	Outcome     string
	Prompt      string
	CaseJSON    json.RawMessage
	FinalOutput *string
	SessionID   string
	Agent       AgentSpec
	AttemptNo   int
	JudgeAlias  string
}

// EvaluationResult is one completed Judge attempt.
type EvaluationResult struct {
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

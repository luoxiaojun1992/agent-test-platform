package llm

import "encoding/json"

const (
	ProviderOpenAI Provider = "openai"
	ProviderOllama Provider = "ollama"

	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"

	ToolChoiceNone     ToolChoiceMode = "none"
	ToolChoiceAuto     ToolChoiceMode = "auto"
	ToolChoiceRequired ToolChoiceMode = "required"
	ToolChoiceNamed    ToolChoiceMode = "named"
)

type Provider string
type Role string
type ToolChoiceMode string

type ModelRef struct {
	Provider Provider
	Model    string
}

type Message struct {
	Role       Role
	Content    *string
	ToolCallID string
	ToolCalls  []ModelToolCall
}

type ToolDefinition struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

type ToolChoice struct {
	Mode ToolChoiceMode
	Name string
}

type CompletionRequest struct {
	RequestID       string
	Model           ModelRef
	Messages        []Message
	Tools           []ToolDefinition
	ToolChoice      ToolChoice
	ResponseSchema  json.RawMessage
	MaxOutputTokens int
	Temperature     *float64
	Seed            *int64
}

type ModelToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

type TokenUsage struct {
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
	Available    bool
}

type CompletionResponse struct {
	ProviderRequestID string
	Model             ModelRef
	StopReason        string
	Content           string
	ToolCalls         []ModelToolCall
	Usage             TokenUsage
	RawDigest         string
}

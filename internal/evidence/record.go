package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

const (
	SourceAgentReported Source = "AGENT_REPORTED"

	StatusSuccess Status = "SUCCESS"
	StatusError   Status = "ERROR"
)

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type Source string
type Status string

// Record is the provider-neutral persisted Tool Call evidence.
type Record struct {
	Sequence       int
	CallID         string
	ParentCallID   *string
	ToolName       string
	Arguments      json.RawMessage
	ArgumentDigest string
	Result         json.RawMessage
	ResultDigest   string
	Error          json.RawMessage
	ErrorDigest    string
	Status         Status
	Source         Source
	Truncated      bool
	OriginalBytes  *int64
	DurationMS     int64
	StartedAt      *time.Time
}

func (record Record) Validate() error {
	if record.Sequence < 1 || record.Sequence > 32 {
		return fmt.Errorf("sequence outside [1,32]")
	}
	if strings.TrimSpace(record.CallID) == "" || strings.TrimSpace(record.ToolName) == "" {
		return fmt.Errorf("call ID and tool name are required")
	}
	if len(record.Arguments) == 0 || !json.Valid(record.Arguments) || !digestPattern.MatchString(record.ArgumentDigest) {
		return fmt.Errorf("arguments and SHA-256 digest are required")
	}
	argumentDigest, err := CanonicalJSONDigest(record.Arguments)
	if err != nil || argumentDigest != record.ArgumentDigest {
		return fmt.Errorf("arguments SHA-256 does not match content")
	}
	if record.Source != SourceAgentReported {
		return fmt.Errorf("invalid trace source %q", record.Source)
	}
	if record.DurationMS < 0 {
		return fmt.Errorf("duration cannot be negative")
	}
	switch record.Status {
	case StatusSuccess:
		if len(record.Error) != 0 {
			return fmt.Errorf("successful call cannot contain an error")
		}
		if len(record.Result) == 0 || !json.Valid(record.Result) || !digestPattern.MatchString(record.ResultDigest) {
			return fmt.Errorf("successful call requires result JSON and digest")
		}
		if !record.Truncated {
			outputDigest, err := resultOutputDigest(record.Result)
			if err != nil || outputDigest != record.ResultDigest {
				return fmt.Errorf("result SHA-256 does not match output")
			}
		}
	case StatusError:
		if len(record.Result) != 0 || record.ResultDigest != "" {
			return fmt.Errorf("failed call cannot contain a result")
		}
		if len(record.Error) == 0 || !json.Valid(record.Error) || !digestPattern.MatchString(record.ErrorDigest) {
			return fmt.Errorf("failed call requires error JSON")
		}
		errorDigest, err := CanonicalJSONDigest(record.Error)
		if err != nil || errorDigest != record.ErrorDigest {
			return fmt.Errorf("error SHA-256 does not match content")
		}
	default:
		return fmt.Errorf("invalid status %q", record.Status)
	}
	if record.Truncated && (record.OriginalBytes == nil || *record.OriginalBytes < 1) {
		return fmt.Errorf("truncated result requires original byte count")
	}
	return nil
}

// CanonicalJSONDigest hashes one complete JSON value while preserving JSON number syntax.
func CanonicalJSONDigest(data []byte) (string, error) {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return "", err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return "", fmt.Errorf("trailing JSON data")
	}
	canonical, err := json.Marshal(document)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(canonical)
	return hex.EncodeToString(hash[:]), nil
}

func resultOutputDigest(result []byte) (string, error) {
	decoder := json.NewDecoder(strings.NewReader(string(result)))
	decoder.UseNumber()
	var document struct {
		Output any `json:"output"`
	}
	if err := decoder.Decode(&document); err != nil {
		return "", err
	}
	output, err := json.Marshal(document.Output)
	if err != nil {
		return "", err
	}
	return CanonicalJSONDigest(output)
}

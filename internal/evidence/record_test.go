package evidence

import (
	"strings"
	"testing"
)

func TestRecordValidate(t *testing.T) {
	t.Parallel()

	arguments := []byte(`{"sql":"SELECT 1"}`)
	argumentDigest, _ := CanonicalJSONDigest(arguments)
	result := []byte(`{"status":"SUCCESS","output":{"value":1},"truncated":false}`)
	resultDigest, _ := CanonicalJSONDigest([]byte(`{"value":1}`))
	record := Record{
		Sequence: 1, CallID: "call-1", ToolName: "mysql.query",
		Arguments: arguments, ArgumentDigest: argumentDigest,
		Result: result, ResultDigest: resultDigest,
		Status: StatusSuccess, Source: SourceAgentReported,
	}
	if err := record.Validate(); err != nil {
		t.Fatal(err)
	}

	record.Source = "FORGED"
	if err := record.Validate(); err == nil {
		t.Fatal("invalid source was accepted")
	}
}

func TestTruncatedRecordRequiresOriginalSize(t *testing.T) {
	t.Parallel()

	arguments := []byte(`{}`)
	argumentDigest, _ := CanonicalJSONDigest(arguments)
	record := Record{
		Sequence: 1, CallID: "call-1", ToolName: "mysql.query",
		Arguments: arguments, ArgumentDigest: argumentDigest,
		Result: []byte(`{"output":{}}`), ResultDigest: strings.Repeat("a", 64),
		Status: StatusSuccess, Source: SourceAgentReported, Truncated: true,
	}
	if err := record.Validate(); err == nil {
		t.Fatal("truncated record without original size was accepted")
	}
}

func TestRecordRejectsMismatchedDigest(t *testing.T) {
	t.Parallel()

	record := Record{
		Sequence: 1, CallID: "call-1", ToolName: "mysql.query",
		Arguments: []byte(`{}`), ArgumentDigest: strings.Repeat("a", 64),
		Result: []byte(`{"output":null}`), ResultDigest: strings.Repeat("b", 64),
		Status: StatusSuccess, Source: SourceAgentReported,
	}
	if err := record.Validate(); err == nil {
		t.Fatal("mismatched digest was accepted")
	}
}

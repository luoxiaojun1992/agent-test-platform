package runexec

import (
	"strings"
	"testing"
	"time"

	"agent-test-platform/internal/run"
)

func TestCapOutput(t *testing.T) {
	short, digest := capOutput("hello")
	if short != "hello" || len(digest) != 64 {
		t.Fatalf("unexpected %q %q", short, digest)
	}
	long := strings.Repeat("好", run.MaxFinalOutputLength+5)
	capped, longDigest := capOutput(long)
	if len([]rune(capped)) != run.MaxFinalOutputLength || longDigest == digest {
		t.Fatal("output not capped on rune boundary")
	}
}

func TestBuildRequestPromptNamesDatabase(t *testing.T) {
	job := run.Job{RunID: "run1", Prompt: "do it", Agent: run.AgentSpec{Model: "m"}}
	request, err := buildRequest(job, "atp_run_x", time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(request.Message.Content, "`atp_run_x`") {
		t.Fatalf("unexpected request %+v", request)
	}
	if request.Model != "m" || request.RunID != "run1" {
		t.Fatalf("unexpected request %+v", request)
	}
}

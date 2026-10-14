package hubapi

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

// hubRunner answers the curl scripts like a hub: the project list, and one
// agent list page per call.
type hubRunner struct{ agents string }

func (h hubRunner) reply(args []string) (proc.Result, error) {
	switch url := args[4]; {
	case strings.HasSuffix(url, "/api/v1/projects"):
		return proc.Result{Stdout: projectsBody + "\n200"}, nil
	case strings.Contains(url, "/api/v1/agents?"):
		return proc.Result{Stdout: h.agents + "\n200"}, nil
	}
	return proc.Result{Stdout: "\n404"}, nil
}

func (h hubRunner) Run(_ context.Context, _ map[string]string, _ string, args ...string) (proc.Result, error) {
	return h.reply(args)
}

func (h hubRunner) RunIn(_ context.Context, _ string, _ map[string]string, _ string, args ...string) (proc.Result, error) {
	return h.reply(args)
}

func (h hubRunner) RunStdin(_ context.Context, _ io.Reader, _ map[string]string, _ string, args ...string) (proc.Result, error) {
	return h.reply(args)
}

// One agent whose task text alone passes 64 KiB: its record is what resume
// and the stored-role check read.
func TestAgentsReadsARecordWithALargeTask(t *testing.T) {
	task := strings.Repeat("t", 70<<10)
	h := hubRunner{agents: `{"agents":[{"slug":"big","appliedConfig":{"agentRole":"baseline","task":"` + task + `"}}]}`}
	c := &Client{T: &JailCurl{Runner: h, BaseURL: "http://127.0.0.1:8080", Token: func() string { return "t" }}}
	got, err := c.Agents(context.Background(), "lever", "hub")
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	if len(got) != 1 || got[0].Slug != "big" || got[0].Role != "baseline" {
		t.Fatalf("got %+v", got)
	}
}

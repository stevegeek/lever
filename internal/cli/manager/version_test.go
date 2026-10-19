package manager

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/agent"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/jailabi"
)

func runManager(t *testing.T, c brokerCaller, args ...string) (string, error) {
	t.Helper()
	root := newRoot(c)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestVersionPrintsBothSides(t *testing.T) {
	t.Setenv(bootstrapEnv, "")
	var method, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		fmt.Fprintf(w, `{"version":"0.0.1 (feedfacecafe)","jail_abi":%d}`, jailabi.JailABI+1)
	}))
	defer srv.Close()
	out, err := runManager(t, httpCaller{client: srv.Client(), baseURL: srv.URL}, "version")
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodGet || path != "/version" {
		t.Fatalf("asked %s %s, want GET /version", method, path)
	}
	for _, want := range []string{
		agent.BuildLine("lever-manager", cli.Version) + "\n",
		fmt.Sprintf("host lever 0.0.1 (feedfacecafe), jail ABI %d\n", jailabi.JailABI+1),
		"mismatch: image ",
		": rebuild the agent image, then lever up --fresh --keep-conversation",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q lacks %q", out, want)
		}
	}
}

func TestVersionAdvisesAWorkerToAskTheOperator(t *testing.T) {
	t.Setenv(bootstrapEnv, "/run/lever/bootstrap.json")
	c := fakeBroker(t, func(w http.ResponseWriter, _ string, _ map[string]any) {
		fmt.Fprintf(w, `{"version":"0.0.1","jail_abi":%d}`, jailabi.JailABI-1)
	})
	out, err := runManager(t, c, "version")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ask the operator to rebuild the agent image") {
		t.Fatalf("worker output %q lacks the ask-the-operator advice", out)
	}
}

func TestVersionFailsWhenTheBrokerRefuses(t *testing.T) {
	c := fakeBroker(t, func(w http.ResponseWriter, _ string, _ map[string]any) {
		http.Error(w, "forbidden", http.StatusForbidden)
	})
	out, err := runManager(t, c, "version")
	if err == nil {
		t.Fatalf("a refused /version must fail; output %q", out)
	}
	if !strings.HasPrefix(out, agent.BuildLine("lever-manager", cli.Version)+"\n") {
		t.Fatalf("the local line must print before the error; output %q", out)
	}
}

// --version is local: it prints the build line and never asks the broker.
func TestVersionFlagIsLocal(t *testing.T) {
	c := fakeBroker(t, func(http.ResponseWriter, string, map[string]any) {
		t.Error("--version asked the broker")
	})
	out, err := runManager(t, c, "--version")
	if err != nil {
		t.Fatal(err)
	}
	if want := agent.BuildLine("lever-manager", cli.Version) + "\n"; out != want {
		t.Fatalf("--version printed %q, want %q", out, want)
	}
}

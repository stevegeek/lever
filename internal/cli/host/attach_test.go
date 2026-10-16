package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/cli/clitest"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/testutil"
)

func attachApp() *config.App {
	return &config.App{
		Name: "assistant",
		Workers: []config.Worker{
			{Name: "scratch", Dir: "workers/scratch"},
			{Name: "worker", Dir: "workers/worker"},
		},
	}
}

func wantAttachTarget(t *testing.T, to, wantSlug string) {
	t.Helper()
	slug, project, err := attachTarget(attachApp(), "/lever", to)
	if err != nil {
		t.Fatalf("attachTarget: %v", err)
	}
	if slug != wantSlug || project != "/lever" {
		t.Fatalf("got (%q, %q), want (%s, /lever)", slug, project, wantSlug)
	}
}

func TestAttachTargetDefaultsToManager(t *testing.T) { wantAttachTarget(t, "", "assistant") }

func TestAttachTargetManagerByName(t *testing.T) { wantAttachTarget(t, "assistant", "assistant") }

// The worker's record lives in the instance project, not
// /lever/workers/<name>.
func TestAttachTargetWorker(t *testing.T) { wantAttachTarget(t, "scratch", "scratch") }

func TestAttachTargetUnknownListsValidNames(t *testing.T) {
	_, _, err := attachTarget(attachApp(), "/lever", "nope")
	if err == nil {
		t.Fatal("want error for unknown name")
	}
	testutil.WantErrContaining(t, err, "nope", "assistant", "scratch", "worker")
}

// TestAttachNamePositionalIsNotAConfigPath: the positional is the agent name
// and config comes from the CWD; taking "scratch" as a config path would fail
// before the jail-not-up hint.
func TestAttachNamePositionalIsNotAConfigPath(t *testing.T) {
	dir := writeInstance(t, managerYAML)
	t.Chdir(dir)

	sb := &stubBackend{resolveRunUserErr: fmt.Errorf("machine %q does not exist", "lever-demo")}
	root := stubRoot(sb)
	_, err := clitest.Exec(t, root, "attach", "scratch")
	wantJailNotUp(t, err)
}

// TestAttachIsPassiveWhenJailNotUp: attach against a down jail fails fast with
// a `lever up` hint and never provisions the machine.
func TestAttachIsPassiveWhenJailNotUp(t *testing.T) {
	dir := writeInstance(t, managerYAML)
	t.Chdir(dir)

	sb := &stubBackend{resolveRunUserErr: fmt.Errorf("machine %q does not exist", "lever-demo")}
	root := stubRoot(sb)
	_, err := clitest.Exec(t, root, "attach")
	wantJailNotUp(t, err)
	if sb.up {
		t.Fatal("attach must never call EnsureUp — it must not provision the jail")
	}
}

// TestAttachArgvKeepsHubTokenOffHostArgv: the attach's host argv is readable
// by every local user via `ps`, so the PAT is staged in the guest on stdin and
// appears in no host argv.
func TestAttachArgvKeepsHubTokenOffHostArgv(t *testing.T) {
	host := proc.NewFakeRunner()
	host.Script("orb", proc.Result{})
	jr := jail.New(jail.Config{Host: host, Prefix: []string{"orb", "-m", "lever-demo", "-u", "leveruser"}, UID: "501"})
	sb := &stubBackend{runner: jr}
	sc := scion.New(jr, scion.Options{HubEndpoint: scion.DefaultHubEndpoint, HubTokenSource: func() string { return "pat-attach-secret" }})

	argv, err := attachArgv(context.Background(), sb, sc, "assistant", "/lever")
	if err != nil {
		t.Fatalf("attachArgv: %v", err)
	}
	if joined := strings.Join(argv, " "); strings.Contains(joined, "pat-attach-secret") {
		t.Fatalf("token in the exec argv: %q", joined)
	} else if !strings.Contains(joined, "SCION_HUB_ENDPOINT="+scion.DefaultHubEndpoint) || !strings.HasSuffix(joined, "scion attach assistant -g /lever") {
		t.Fatalf("exec argv must still pin the endpoint and end with the attach command: %q", joined)
	}
	if len(host.Calls) != 1 {
		t.Fatalf("want one staging call through the jail, got %d", len(host.Calls))
	}
	stage := host.Calls[0]
	if joined := strings.Join(append([]string{stage.Name}, stage.Args...), " "); strings.Contains(joined, "pat-attach-secret") {
		t.Fatalf("token in the staging host argv: %q", joined)
	}
	if stage.Stdin != "pat-attach-secret" {
		t.Fatalf("staging stdin = %q, want the token", stage.Stdin)
	}
}

// Without a token (subscription mode) nothing is staged and the argv is the
// plain backend-wrapped attach.
func TestAttachArgvWithoutTokenStagesNothing(t *testing.T) {
	host := proc.NewFakeRunner()
	sb := &stubBackend{runner: host}
	sc := scion.New(host, scion.Options{HubEndpoint: scion.DefaultHubEndpoint})
	argv, err := attachArgv(context.Background(), sb, sc, "assistant", "/lever")
	if err != nil {
		t.Fatalf("attachArgv: %v", err)
	}
	want := []string{"stub-attach", "env", "SCION_HUB_ENDPOINT=" + scion.DefaultHubEndpoint, "scion", "attach", "assistant", "-g", "/lever"}
	if strings.Join(argv, " ") != strings.Join(want, " ") {
		t.Fatalf("argv = %q, want %q", argv, want)
	}
	if len(host.Calls) != 0 {
		t.Fatalf("nothing may run without a token; got %d call(s)", len(host.Calls))
	}
}

// stubDetachOthers replaces the detachOtherClients seam, answers n, and
// returns the refs it was asked about.
func stubDetachOthers(t *testing.T, n int, err error) *[]string {
	t.Helper()
	var refs []string
	old := detachOtherClients
	detachOtherClients = func(_ context.Context, _ proc.Runner, ref string) (int, error) {
		refs = append(refs, ref)
		return n, err
	}
	t.Cleanup(func() { detachOtherClients = old })
	return &refs
}

// noAttachBinary empties PATH, so the attach fails before it starts
// anything.
func noAttachBinary(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

// By default an attach leaves other terminals attached; with --detach-others
// they are detached first (a stale one can freeze the view), and the count is
// printed.
func TestDetachBeforeAttach(t *testing.T) {
	for _, tc := range []struct {
		name         string
		detachOthers bool
		n            int
		err          error
		want         string
		asked        bool
	}{
		{"default keeps others", false, 3, nil, "", false},
		{"one stale", true, 1, nil, `lever: detached 1 other terminal attached to "assistant" (--detach-others)`, true},
		{"two", true, 2, nil, "detached 2 other terminals", true},
		{"none", true, 0, nil, "", true},
		{"probe fails", true, 0, errors.New("podman: no"), "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refs := stubDetachOthers(t, tc.n, tc.err)
			var out bytes.Buffer
			detachBeforeAttach(context.Background(), &out, proc.NewFakeRunner(), "assistant", "/lever", tc.detachOthers)
			if tc.asked != (len(*refs) == 1) || (tc.asked && (*refs)[0] != "lever--assistant") {
				t.Fatalf("refs = %q, want asked %t", *refs, tc.asked)
			}
			got := out.String()
			if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
				t.Errorf("output %q, want %q", got, tc.want)
			}
		})
	}
}

// The attach (`lever attach`, and `lever up` on a terminal or off one)
// detaches other terminals only when asked, and before it attaches.
func TestAttachDetachesOnlyWhenAsked(t *testing.T) {
	for _, detachOthers := range []bool{false, true} {
		t.Run(fmt.Sprintf("detach-others=%t", detachOthers), func(t *testing.T) {
			noAttachBinary(t)
			refs := stubDetachOthers(t, 1, nil)
			sb := &stubBackend{runner: proc.NewFakeRunner()}
			sc := scion.New(proc.NewFakeRunner(), scion.Options{HubEndpoint: scion.DefaultHubEndpoint})
			var out bytes.Buffer
			argv, err := prepareAttach(context.Background(), &out, sb, sc, "assistant", "/lever", detachOthers)
			if err == nil {
				err = attachChild(argv, nil, nil, nil)
			}
			testutil.WantErrContaining(t, err, "stub-attach")
			if got := len(*refs) == 1 && (*refs)[0] == "lever--assistant"; got != detachOthers || len(*refs) > 1 {
				t.Fatalf("detach refs = %q with --detach-others=%t", *refs, detachOthers)
			}
			if said := strings.Contains(out.String(), "detached 1 other terminal"); said != detachOthers {
				t.Errorf("output %q with --detach-others=%t", out.String(), detachOthers)
			}
		})
	}
}

// `lever attach` keeps the other terminals by default and detaches them
// with --detach-others, end to end through the command.
func TestAttachCommandDetachOthersFlag(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		asked bool
	}{
		{[]string{"attach"}, false},
		{[]string{"attach", "--detach-others"}, true},
		{[]string{"attach", "scratch", "--detach-others"}, true},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			dir := writeInstance(t, managerYAML+"workers:\n  - name: scratch\n    dir: workers/scratch\n")
			t.Chdir(dir)
			noAttachBinary(t)
			refs := stubDetachOthers(t, 0, nil)
			sb := &stubBackend{runner: proc.NewFakeRunner()}
			_, err := clitest.Exec(t, stubRoot(sb), tc.args...)
			// The attach reached its binary lookup, so the detach decision was made.
			testutil.WantErrContaining(t, err, "stub-attach")
			if (len(*refs) == 1) != tc.asked || len(*refs) > 1 {
				t.Fatalf("detach refs = %q, want asked %t", *refs, tc.asked)
			}
			if tc.asked && !strings.HasPrefix((*refs)[0], "lever--") {
				t.Errorf("detach ref %q is not an agent container", (*refs)[0])
			}
		})
	}
}

// Both attaching verbs take --detach-others, off by default, and not --shared.
func TestAttachAndUpDetachOthersFlag(t *testing.T) {
	root := stubRoot(&stubBackend{})
	for _, verb := range []string{"attach", "up"} {
		c, _, err := root.Find([]string{verb})
		if err != nil {
			t.Fatalf("%s: %v", verb, err)
		}
		f := c.Flags().Lookup("detach-others")
		if f == nil || f.Value.Type() != "bool" || f.DefValue != "false" {
			t.Errorf("%s: --detach-others = %+v, want a bool flag defaulting to false", verb, f)
		}
		if c.Flags().Lookup("shared") != nil {
			t.Errorf("%s: --shared must be gone", verb)
		}
	}
}

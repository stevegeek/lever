package broker

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/scion"
)

// rewritableImages are record images the hub rewrites to its image registry:
// unqualified names, and none at all (keepEmptyImage).
var rewritableImages = []string{"scionlocal/evil:1", "evil:1", "localhost/evil:1", ""}

var imageSpec = WorkerSpec{Name: "worker", WorkspaceSubdir: "workers/worker", TicketDir: "/run/user/501/lever/tickets/worker", Image: "lever.local/lever-claude:1"}

// imagePaths are the ways a host program, the manager or the chat page bring
// a worker's existing record up.
var imagePaths = map[string]func(t *testing.T, b *Broker) *httptest.ResponseRecorder{
	"manager resume": func(t *testing.T, b *Broker) *httptest.ResponseRecorder {
		return callWorker(t, b, "/worker/resume", `{"worker":"worker"}`, "test-manager")
	},
	"manager start": func(t *testing.T, b *Broker) *httptest.ResponseRecorder {
		return callWorker(t, b, "/worker/start", `{"worker":"worker"}`, "test-manager")
	},
	"operator resume": func(t *testing.T, b *Broker) *httptest.ResponseRecorder {
		return callOperator(t, b, http.MethodPost, PathOperatorWorkerResume, `{"worker":"worker"}`)
	},
	// Autostart is the operator's start with no task.
	"operator start": func(t *testing.T, b *Broker) *httptest.ResponseRecorder {
		return callOperator(t, b, http.MethodPost, PathOperatorWorkerStart, `{"worker":"worker"}`)
	},
	"chat resume": func(t *testing.T, b *Broker) *httptest.ResponseRecorder {
		return postWake(t, b, `{"worker":"worker","login":"c@x","tier":"operator"}`)
	},
}

func imageRuntime(phase, image string) *fakeRuntime {
	return &fakeRuntime{keepEmptyImage: image == "", agents: map[string][]scion.Agent{
		testInstanceProject: {{Slug: "worker", Phase: phase, Image: image}},
	}}
}

// Every path refuses a record the hub would rewrite before it stages a
// ticket or resumes, and names the operator's recycle and purge.
func TestWorkerPathsRefuseARewritableRecordImage(t *testing.T) {
	for name, call := range imagePaths {
		for _, phase := range []string{scion.PhaseSuspended, scion.PhaseStopped, scion.PhaseError} {
			if name == "chat resume" && phase == scion.PhaseError {
				continue
			}
			for _, image := range rewritableImages {
				rt := imageRuntime(phase, image)
				b := newTestBroker(t, rt, imageSpec)
				var audit bytes.Buffer
				b.log = slog.New(slog.NewTextHandler(&audit, nil))
				rec := call(t, b)
				what := name + " " + phase + " " + image
				if rec.Code != http.StatusConflict || rec.Header().Get(ReasonHeader) != ReasonImageUnqualified {
					t.Errorf("%s: %d %q, want 409 %q: %s", what, rec.Code, rec.Header().Get(ReasonHeader), ReasonImageUnqualified, rec.Body)
					continue
				}
				for _, want := range []string{"lever worker recycle worker", "lever worker purge worker --force", "lever worker start worker", "lever.local/lever-claude:1", "nothing was changed"} {
					if !strings.Contains(rec.Body.String(), want) {
						t.Errorf("%s: the refusal does not say %q: %s", what, want, rec.Body)
					}
				}
				if image != "" && strings.Contains(rec.Body.String(), image) {
					t.Errorf("%s: the refusal echoes the hub's image text: %s", what, rec.Body)
				}
				if len(rt.staged)+len(rt.resumed)+len(rt.resumeForced)+len(rt.started)+len(rt.stopped)+len(rt.suspend) != 0 {
					t.Errorf("%s: a refusal must change nothing: staged=%d resumed=%v forced=%v started=%d", what, len(rt.staged), rt.resumed, rt.resumeForced, len(rt.started))
				}
				if !strings.Contains(audit.String(), "decision=deny") {
					t.Errorf("%s: no deny audit line:\n%s", what, audit.String())
				}
			}
		}
	}
}

// The manager is told the operator must act; the operator is told to run it.
func TestRewritableImageRefusalNamesWhoActs(t *testing.T) {
	rec := callWorker(t, newTestBroker(t, imageRuntime(scion.PhaseSuspended, "x:1"), imageSpec), "/worker/resume", `{"worker":"worker"}`, "test-manager")
	if !strings.Contains(rec.Body.String(), "The operator must run `lever worker recycle worker`") {
		t.Errorf("manager: %s", rec.Body)
	}
	rec = callOperator(t, newTestBroker(t, imageRuntime(scion.PhaseSuspended, "x:1"), imageSpec), http.MethodPost, PathOperatorWorkerResume, `{"worker":"worker"}`)
	if !strings.Contains(rec.Body.String(), "Run `lever worker recycle worker`") {
		t.Errorf("operator: %s", rec.Body)
	}
}

// A qualified or digest-pinned record image resumes as before.
func TestWorkerPathsResumeAFixedRecordImage(t *testing.T) {
	for name, call := range imagePaths {
		for _, image := range []string{"lever.local/lever-claude:1", "ghcr.io/org/x:1", "scionlocal/x@sha256:abc"} {
			rt := imageRuntime(scion.PhaseSuspended, image)
			rec := call(t, newTestBroker(t, rt, imageSpec))
			if rec.Code != http.StatusOK || len(rt.resumed) != 1 {
				t.Errorf("%s %s: %d %s resumed=%v", name, image, rec.Code, rec.Body, rt.resumed)
			}
		}
	}
}

// A heal bounces nothing and stages no ticket for a record the hub would
// rewrite, whatever its phase.
func TestHealRefusesARewritableRecordImage(t *testing.T) {
	for _, phase := range []string{scion.PhaseRunning, scion.PhaseSuspended, scion.PhaseStopped, scion.PhaseError} {
		for _, image := range rewritableImages {
			rt := &fakeRuntime{staticPhases: true, keepEmptyImage: image == "", agents: map[string][]scion.Agent{
				testInstanceProject: {{Slug: "scratch", Phase: phase, ContainerStatus: "Up 2 minutes", Image: image}},
			}}
			b, _, _ := reenrolBroker(t, rt, "all")
			var audit bytes.Buffer
			b.log = slog.New(slog.NewTextHandler(&audit, nil))
			b.healLapse(context.Background(), "scratch")
			if len(rt.staged)+len(rt.suspend)+len(rt.resumed)+len(rt.resumeForced) != 0 {
				t.Errorf("%s %q: staged=%d suspend=%v resume=%v forced=%v, want none", phase, image, len(rt.staged), rt.suspend, rt.resumed, rt.resumeForced)
			}
			if !strings.Contains(audit.String(), "decision=deny") || !strings.Contains(audit.String(), "lever worker recycle scratch") {
				t.Errorf("%s %q: no deny line naming the recycle:\n%s", phase, image, audit.String())
			}
		}
	}
}

// The bounce checks the record it is about to resume too, in case it was
// recreated after the heal's first look.
func TestBounceRefusesARewritableRecordImage(t *testing.T) {
	rt := &fakeRuntime{staticPhases: true, agents: map[string][]scion.Agent{
		testInstanceProject: {{Slug: "scratch", Phase: scion.PhaseSuspended, Image: "x:1"}},
	}}
	b, _, _ := reenrolBroker(t, rt, "all")
	if _, ok := b.bounceForReenrol(context.Background(), "scratch", "scratch"); ok || len(rt.resumed) != 0 {
		t.Fatalf("bounced a rewritable record: resumed=%v", rt.resumed)
	}
}

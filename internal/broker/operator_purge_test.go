package broker

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/scion"
)

// purgeBroker declares "worker"; the hub holds records for the manager
// ("appname"), for the worker in phase (none if ""), and for an orphan
// "ghost".
func purgeBroker(t *testing.T, phase string) (*Broker, *fakeRuntime, *bytes.Buffer) {
	t.Helper()
	recs := []scion.Agent{{Slug: "appname", Phase: "running"}, {Slug: "ghost", Phase: "running", ContainerStatus: "Up 1 minute"}}
	if phase != "" {
		recs = append(recs, scion.Agent{Slug: "worker", Phase: phase})
	}
	rt := &fakeRuntime{agents: map[string][]scion.Agent{testInstanceProject: recs}, staticPhases: true}
	var buf bytes.Buffer
	b := New(testConfig(t, withManager("test-manager", "appname"), withRuntime(rt, WorkerSpec{Name: "worker"}), withAudit(&buf)))
	return b, rt, &buf
}

func purgeAnswer(t *testing.T, rec *httptest.ResponseRecorder) WorkerPurgeResponse {
	t.Helper()
	var got WorkerPurgeResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	return got
}

func superseded(b *Broker, name string) bool { return !b.leafFrom(name).IsZero() }

// A declared worker's record is deleted whatever its phase (--force is the
// operator's confirmation), and its certificates are superseded.
func TestOperatorPurgeDeclaredWorker(t *testing.T) {
	for _, phase := range []string{scion.PhaseRunning, scion.PhaseSuspended, scion.PhaseError} {
		b, rt, audit := purgeBroker(t, phase)
		got := purgeAnswer(t, callOperator(t, b, http.MethodPost, PathOperatorWorkerPurge, `{"worker":"worker"}`))
		if got.Worker != "worker" || got.Was != phase || got.TicketLeft || got.CutoffUnsaved {
			t.Fatalf("%s: %+v", phase, got)
		}
		if len(rt.purged) != 1 || rt.purged[0] != "worker" || !superseded(b, "worker") {
			t.Fatalf("%s: purged %v, superseded %v", phase, rt.purged, superseded(b, "worker"))
		}
		if !strings.Contains(audit.String(), "purge worker: record deleted (was "+phase+")") {
			t.Fatalf("%s: audit\n%s", phase, audit)
		}
	}
}

// A declared worker with no record has nothing to delete: only the cutoff.
func TestOperatorPurgeDeclaredWorkerWithoutRecord(t *testing.T) {
	b, rt, _ := purgeBroker(t, "")
	got := purgeAnswer(t, callOperator(t, b, http.MethodPost, PathOperatorWorkerPurge, `{"worker":"worker"}`))
	if got.Was != PhaseNone || len(rt.purged) != 0 || !superseded(b, "worker") {
		t.Fatalf("%+v purged %v", got, rt.purged)
	}
}

// An orphan is purged only with orphan set, and only when the hub has it.
func TestOperatorPurgeOrphan(t *testing.T) {
	b, rt, _ := purgeBroker(t, scion.PhaseSuspended)
	got := purgeAnswer(t, callOperator(t, b, http.MethodPost, PathOperatorWorkerPurge, `{"worker":"ghost","orphan":true}`))
	if got.Was != scion.PhaseRunning || len(rt.purged) != 1 || rt.purged[0] != "ghost" {
		t.Fatalf("%+v purged %v", got, rt.purged)
	}
	rec := callOperator(t, b, http.MethodPost, PathOperatorWorkerPurge, `{"worker":"ghost","orphan":true}`)
	if rec.Code != http.StatusNotFound || rec.Header().Get(ReasonHeader) != ReasonNoRecord || len(rt.purged) != 1 {
		t.Fatalf("again: %d %q purged %v", rec.Code, rec.Header().Get(ReasonHeader), rt.purged)
	}
}

// Every refusal deletes nothing and supersedes nothing.
func TestOperatorPurgeRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		body   string
		status int
		reason string
	}{
		"bad body":                    {`{"worker":`, http.StatusBadRequest, ReasonBadRequest},
		"not an agent name":           {`{"worker":"../x"}`, http.StatusBadRequest, ReasonBadRequest},
		"empty name":                  {`{"worker":""}`, http.StatusBadRequest, ReasonBadRequest},
		"the manager's slug":          {`{"worker":"appname"}`, http.StatusForbidden, ReasonNotAWorker},
		"the manager's slug, orphan":  {`{"worker":"appname","orphan":true}`, http.StatusForbidden, ReasonNotAWorker},
		"the manager's CN, orphan":    {`{"worker":"test-manager","orphan":true}`, http.StatusForbidden, ReasonNotAWorker},
		"an orphan without the flag":  {`{"worker":"ghost"}`, http.StatusForbidden, ReasonNotAWorker},
		"a worker named as an orphan": {`{"worker":"worker","orphan":true}`, http.StatusForbidden, ReasonNotOrphan},
	} {
		b, rt, _ := purgeBroker(t, scion.PhaseSuspended)
		rec := callOperator(t, b, http.MethodPost, PathOperatorWorkerPurge, tc.body)
		if rec.Code != tc.status || rec.Header().Get(ReasonHeader) != tc.reason {
			t.Errorf("%s: %d %q, want %d %q", name, rec.Code, rec.Header().Get(ReasonHeader), tc.status, tc.reason)
		}
		if len(rt.purged) != 0 || superseded(b, "worker") || superseded(b, "ghost") || superseded(b, "appname") {
			t.Errorf("%s: purged %v", name, rt.purged)
		}
	}
}

// A purge never waits behind another lifecycle action of the worker.
func TestOperatorPurgeBusy(t *testing.T) {
	b, rt, _ := purgeBroker(t, scion.PhaseSuspended)
	unlock, ok := b.tryLockWorker("worker")
	if !ok {
		t.Fatal("lock")
	}
	defer unlock()
	rec := callOperator(t, b, http.MethodPost, PathOperatorWorkerPurge, `{"worker":"worker"}`)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get(ReasonHeader) != ReasonBusy || len(rt.purged) != 0 || superseded(b, "worker") {
		t.Fatalf("%d %q purged %v", rec.Code, rec.Header().Get(ReasonHeader), rt.purged)
	}
}

// A delete that fails, or is not confirmed, keeps the certificates: the
// record may still be there and be resumed.
func TestOperatorPurgeFailedDeleteKeepsCertificates(t *testing.T) {
	for _, err := range []error{errors.New("boom"), scion.ErrDeleteUnconfirmed} {
		b, rt, _ := purgeBroker(t, scion.PhaseSuspended)
		rt.purgeErr = err
		rec := callOperator(t, b, http.MethodPost, PathOperatorWorkerPurge, `{"worker":"worker"}`)
		if rec.Code != http.StatusBadGateway || rec.Header().Get(ReasonHeader) != ReasonRuntimeError || superseded(b, "worker") {
			t.Fatalf("%v: %d %q superseded %v", err, rec.Code, rec.Header().Get(ReasonHeader), superseded(b, "worker"))
		}
	}
	b, rt, _ := purgeBroker(t, scion.PhaseSuspended)
	rt.listErr = errors.New("hub down")
	if rec := callOperator(t, b, http.MethodPost, PathOperatorWorkerPurge, `{"worker":"worker"}`); rec.Code != http.StatusBadGateway || len(rt.purged) != 0 || superseded(b, "worker") {
		t.Fatalf("list error: %d purged %v", rec.Code, rt.purged)
	}
}

// A ticket left behind, or a cutoff not saved, is reported, not a failure.
func TestOperatorPurgeWarnings(t *testing.T) {
	b, rt, _ := purgeBroker(t, scion.PhaseSuspended)
	rt.ticketErr = errors.New("rm failed")
	b.persist = func(RevocationState) error { return errors.New("disk full") }
	got := purgeAnswer(t, callOperator(t, b, http.MethodPost, PathOperatorWorkerPurge, `{"worker":"worker"}`))
	if !got.TicketLeft || !got.CutoffUnsaved || !superseded(b, "worker") {
		t.Fatalf("%+v", got)
	}
}

// The purge route is on the operator socket only.
func TestOperatorPurgeIsNotOnTheTCPListeners(t *testing.T) {
	b, rt, _ := purgeBroker(t, scion.PhaseSuspended)
	for name, h := range map[string]http.Handler{"admin": b.AdminHandler(), "jail": b.JailHandler()} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, PathOperatorWorkerPurge, strings.NewReader(`{"worker":"worker"}`)))
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s handler answers the purge: %d", name, rec.Code)
		}
	}
	if len(rt.purged) != 0 {
		t.Fatal("purged through a TCP listener")
	}
}

// A recycle's purge supersedes the old certificates before the fresh start
// enrols, so the new leaf is accepted and the old one is not.
func TestRecycleSupersedesTheOldCertificates(t *testing.T) {
	b, _, _, _ := recycleBroker(t, scion.PhaseSuspended, true)
	old := issueLeaf(t, b, "worker")
	if rec := callWorker(t, b, "/worker/recycle", recycleBody, "test-manager"); rec.Code != http.StatusOK {
		t.Fatalf("recycle: %d %s", rec.Code, rec.Body)
	}
	if gated(b, old) != http.StatusForbidden || gated(b, issueLeaf(t, b, "worker")) != http.StatusOK {
		t.Fatal("recycle did not supersede the old leaf, or refused the new one")
	}
}

// The worker list names the orphans: neither the manager (by slug or CN)
// nor a declared worker. A slug that is not an agent name is only counted.
func TestOperatorWorkerListNamesOrphans(t *testing.T) {
	rt := &fakeRuntime{agents: map[string][]scion.Agent{testInstanceProject: {
		{Slug: "appname", Phase: "running"},
		{Slug: "test-manager", Phase: "running"},
		{Slug: "worker", Phase: "suspended"},
		{Slug: "zed", Phase: "stopped"},
		{Slug: "ghost", Phase: "bogus\x1b[2J"},
		{Slug: "Bad Name\n", Phase: "running"},
	}}}
	b := New(testConfig(t, withManager("test-manager", "appname"), withRuntime(rt, WorkerSpec{Name: "worker"})))
	rec := callOperator(t, b, http.MethodGet, PathOperatorWorkerList, "")
	var got WorkerListResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(got.Orphans) != 2 || got.Orphans[0].Name != "ghost" || got.Orphans[1] != (OrphanEntry{Name: "zed", Phase: "stopped"}) || got.UnnamedOrphans != 1 {
		t.Fatalf("orphans %+v unnamed %d", got.Orphans, got.UnnamedOrphans)
	}
	if strings.ContainsAny(got.Orphans[0].Phase, "\x1b[") {
		t.Fatalf("hub phase text leaked: %q", got.Orphans[0].Phase)
	}
}

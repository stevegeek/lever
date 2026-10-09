package broker

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/scion"
)

// sharedTree is strictTree with one shared folder, tools/releases, that
// the worker mounts read-only (or read-write when writer), and a record
// reader answering vols (or err).
func sharedTree(t *testing.T, rt *fakeRuntime, buf *bytes.Buffer, writer bool, vols []scion.VolumeMount, readErr error) (string, *Broker) {
	t.Helper()
	tree := t.TempDir()
	for _, d := range []string{"tools/releases", "workers/worker", "other"} {
		if err := os.MkdirAll(filepath.Join(tree, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	spec := WorkerSpec{Name: "worker", WorkspaceSubdir: "workers/worker",
		HostWorkspace: filepath.Join(tree, "workers", "worker"),
		TicketDir:     "/run/user/501/lever/tickets/worker", Image: "img:1",
		Shared: []SharedMount{{Rel: "tools/releases", Volume: scion.VolumeMount{Source: "/lever/tools/releases", Target: "/shared/releases", ReadOnly: !writer}}}}
	b := New(testConfig(t, withAudit(buf), withManager("test-manager", ""), withRuntime(rt, spec), func(c *Config) {
		c.Dispatch.Tree = tree
		c.Dispatch.ReadOnlyDirs = []string{"tools/releases"}
		c.Dispatch.SharesConfigured = true
		c.Dispatch.RecordVolumes = func(context.Context, string) ([]scion.VolumeMount, error) { return vols, readErr }
	}))
	return tree, b
}

func TestStaleShares(t *testing.T) {
	ro := scion.VolumeMount{Source: "/lever/tools/releases", Target: "/shared/releases", ReadOnly: true}
	rw := ro
	rw.ReadOnly = false
	ticket := scion.VolumeMount{Source: "/run/user/501/lever/tickets/worker", Target: "/run/lever", ReadOnly: true}
	reader := WorkerSpec{Name: "worker", Shared: []SharedMount{{Rel: "tools/releases", Volume: ro}}}
	writer := WorkerSpec{Name: "worker", Shared: []SharedMount{{Rel: "tools/releases", Volume: rw}}}
	none := WorkerSpec{Name: "worker"}
	for _, tc := range []struct {
		name  string
		spec  WorkerSpec
		vols  []scion.VolumeMount
		stale bool
	}{
		{"matches, reader", reader, []scion.VolumeMount{ticket, ro}, false},
		{"matches, writer", writer, []scion.VolumeMount{ticket, rw}, false},
		{"record lacks it (less access)", reader, []scion.VolumeMount{ticket}, false},
		{"writer demoted to reader", reader, []scion.VolumeMount{ticket, rw}, true},
		{"reader promoted to writer: read-only record is less access", writer, []scion.VolumeMount{ticket, ro}, false},
		{"access withdrawn", none, []scion.VolumeMount{ticket, ro}, true},
		{"other source", reader, []scion.VolumeMount{{Source: "/lever/other", Target: "/shared/releases", ReadOnly: true}}, true},
		{"unclean target", none, []scion.VolumeMount{{Source: "/lever/x", Target: "/shared/./x/", ReadOnly: true}}, true},
		{"the root itself", none, []scion.VolumeMount{{Source: "/lever", Target: "/shared", ReadOnly: true}}, true},
		{"unrelated mount", none, []scion.VolumeMount{ticket, {Source: "/x", Target: "/sharedx"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := staleShares(tc.spec, tc.vols)
			if got := errors.Is(err, ErrStaleSharedMount); got != tc.stale {
				t.Fatalf("staleShares = %v, want stale=%v", err, tc.stale)
			}
		})
	}
}

// A fresh start mounts the plan beside the ticket.
func TestWorkerStart_mountsSharedFolders(t *testing.T) {
	rt := &fakeRuntime{agents: map[string][]scion.Agent{}}
	var buf bytes.Buffer
	_, b := sharedTree(t, rt, &buf, false, nil, nil)
	rec := callWorker(t, b, "/worker/start", `{"worker":"worker","task":"t"}`, "test-manager")
	if rec.Code != http.StatusOK || len(rt.started) != 1 {
		t.Fatalf("status=%d started=%d (%s)", rec.Code, len(rt.started), rec.Body.String())
	}
	want := scion.VolumeMount{Source: "/lever/tools/releases", Target: "/shared/releases", ReadOnly: true}
	if !slices.Contains(rt.started[0].Volumes, want) {
		t.Fatalf("volumes = %+v, want %+v among them", rt.started[0].Volumes, want)
	}
}

// A shared folder missing on the host, or reached through a link, refuses
// the start and every resume, before a ticket is staged.
func TestWorkerStart_sharedFolderSourceChecked(t *testing.T) {
	for _, tc := range []struct {
		name   string
		phase  string
		break_ func(t *testing.T, tree string)
	}{
		{"start, missing", "", func(t *testing.T, tree string) {
			if err := os.RemoveAll(filepath.Join(tree, "tools", "releases")); err != nil {
				t.Fatal(err)
			}
		}},
		{"resume, link", "stopped", func(t *testing.T, tree string) {
			swapToLink(t, filepath.Join(tree, "tools"), "other")
		}},
		{"resume, folder is a link", "suspended", func(t *testing.T, tree string) {
			swapToLink(t, filepath.Join(tree, "tools", "releases"), "../other")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agents := map[string][]scion.Agent{}
			if tc.phase != "" {
				agents[testInstanceProject] = []scion.Agent{{Slug: "worker", Phase: tc.phase}}
			}
			rt := &fakeRuntime{agents: agents}
			var buf bytes.Buffer
			tree, b := sharedTree(t, rt, &buf, false, nil, nil)
			tc.break_(t, tree)
			body := `{"worker":"worker"}`
			if tc.phase == "" {
				body = `{"worker":"worker","task":"t"}`
			}
			rec := callWorker(t, b, "/worker/start", body, "test-manager")
			if rec.Code < 400 {
				t.Fatalf("status = %d, want a refusal (%s)", rec.Code, rec.Body.String())
			}
			if len(rt.started)+len(rt.resumed)+len(rt.resumeForced)+len(rt.staged) != 0 {
				t.Fatalf("nothing may be staged, started or resumed; started=%d resumed=%d staged=%d", len(rt.started), len(rt.resumed), len(rt.staged))
			}
			if !strings.Contains(buf.String(), "shared folder") {
				t.Fatalf("refusal must name the shared folder; log=%s", buf.String())
			}
		})
	}
}

// A resume of a worker whose record holds access the config withdrew is
// refused, and the record kept; a matching record resumes.
func TestWorkerResume_refusesStaleSharedMount(t *testing.T) {
	rw := []scion.VolumeMount{{Source: "/lever/tools/releases", Target: "/shared/releases"}}
	rt := &fakeRuntime{agents: map[string][]scion.Agent{testInstanceProject: {{Slug: "worker", Phase: "stopped"}}}}
	var buf bytes.Buffer
	_, b := sharedTree(t, rt, &buf, false, rw, nil)
	rec := callWorker(t, b, "/worker/start", `{"worker":"worker"}`, "test-manager")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "lever worker purge worker") {
		t.Fatalf("status = %d, want 403 naming the purge (%s)", rec.Code, rec.Body.String())
	}
	if len(rt.resumed)+len(rt.resumeForced)+len(rt.staged)+len(rt.purged) != 0 {
		t.Fatalf("no ticket, resume or purge for a stale record")
	}
	// The healer's bounce refuses it too.
	if _, ok := b.bounceForReenrol(context.Background(), "worker", "worker"); ok {
		t.Fatal("the healer must not bounce a worker with a stale shared mount")
	}

	ro := []scion.VolumeMount{{Source: "/lever/tools/releases", Target: "/shared/releases", ReadOnly: true}}
	rt = &fakeRuntime{agents: map[string][]scion.Agent{testInstanceProject: {{Slug: "worker", Phase: "stopped"}}}}
	_, b = sharedTree(t, rt, &buf, false, ro, nil)
	if rec := callWorker(t, b, "/worker/start", `{"worker":"worker"}`, "test-manager"); rec.Code != http.StatusOK || len(rt.resumed) != 1 {
		t.Fatalf("a matching record must resume; status=%d resumed=%d (%s)", rec.Code, len(rt.resumed), rec.Body.String())
	}
}

// With shared folders configured, a record that cannot be read refuses the
// resume (fail closed).
func TestWorkerResume_unreadableRecordRefusedWithShares(t *testing.T) {
	rt := &fakeRuntime{agents: map[string][]scion.Agent{testInstanceProject: {{Slug: "worker", Phase: "suspended"}}}}
	var buf bytes.Buffer
	_, b := sharedTree(t, rt, &buf, false, nil, errors.New("hub down"))
	if rec := callWorker(t, b, "/worker/start", `{"worker":"worker"}`, "test-manager"); rec.Code != http.StatusForbidden || len(rt.resumed) != 0 {
		t.Fatalf("status=%d resumed=%d, want 403 and no resume (%s)", rec.Code, len(rt.resumed), rec.Body.String())
	}
}

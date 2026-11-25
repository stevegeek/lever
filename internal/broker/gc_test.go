package broker

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// gc collects only agents destroyed long enough ago: their archive and
// backups go, their record goes, and their name stays retired, revoked and
// never drawn again. A dry run changes nothing.
func TestAgentGC(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{})
	var removed []string
	e.b.removeConversations = func(agent string, dryRun bool) (int64, error) {
		if !dryRun {
			removed = append(removed, agent)
		}
		return 100, nil
	}
	old := e.spawn(t, `{"template":"session"}`).Name
	if rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentDestroy, `{"name":"`+old+`","stop":true}`); rec.Code != http.StatusOK {
		t.Fatalf("destroy: %d %s", rec.Code, rec.Body)
	}
	archive := filepath.Join(e.archives, old, archiveWorkspaceFile)
	fi, err := os.Stat(archive)
	if err != nil {
		t.Fatal(err)
	}
	live := e.spawn(t, `{"template":"session"}`).Name

	// Destroyed just now: too recent for --older 1h.
	gc := func(body string) GCResponse {
		t.Helper()
		rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentGC, body)
		var out GCResponse
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("gc %s: %d %s", body, rec.Code, rec.Body)
		}
		return out
	}
	if out := gc(`{"older_than_seconds":3600}`); len(out.Agents) != 0 {
		t.Fatalf("collected a recent destroy: %+v", out)
	}
	e.b.clock = func() time.Time { return time.Now().Add(2 * time.Hour) }
	out := gc(`{"older_than_seconds":3600,"dry_run":true}`)
	if len(out.Agents) != 1 || out.Agents[0] != old || out.Freed != fi.Size()+100 || !out.DryRun {
		t.Fatalf("dry run: %+v", out)
	}
	if _, err := os.Stat(archive); err != nil || len(removed) != 0 {
		t.Fatal("a dry run deleted something")
	}
	out = gc(`{"older_than_seconds":3600}`)
	if len(out.Agents) != 1 || out.Agents[0] != old {
		t.Fatalf("gc: %+v", out)
	}
	if _, err := os.Stat(archive); !os.IsNotExist(err) || len(removed) != 1 || removed[0] != old {
		t.Fatalf("archive %v, backups removed %v", err, removed)
	}
	if _, err := os.Stat(filepath.Dir(archive)); !os.IsNotExist(err) {
		t.Fatalf("archive directory: %v", err)
	}
	st := e.state()
	if len(st.Retired) != 1 || st.Retired[0] != old {
		t.Fatalf("retired %+v", st.Retired)
	}
	for _, r := range st.Agents {
		if r.Name == old {
			t.Fatal("the record was kept")
		}
		if r.Name == live && r.State != AgentActive {
			t.Fatal("gc touched a live agent")
		}
	}
	// After a restart the name is still revoked and never drawn.
	again := newSpawnEnv(t, st)
	if !again.b.isRevoked(old) {
		t.Fatal("a retired name is not revoked after a restart")
	}
	if _, err := again.b.workers.reserve(sessionTemplate(again.tree), MemorySize{}, "operator", "", time.Now(), func(string) bool { return false },
		func() (string, error) { return old[2:], nil }); err == nil {
		t.Fatal("a retired name was drawn again")
	}
	// Bad bodies.
	for _, body := range []string{`{"older_than_seconds":-1}`, `{"older":1}`} {
		if rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentGC, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", body, rec.Code)
		}
	}
}

package host

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/testutil"
)

// TestUpDecisions pins up's path over the shared observation set
// (characterization; start-manager's table is apply's
// TestStartManagerDecisions). up decides on the phase alone: a running record
// over a dead container takes the attach-only path here, and its gate
// (ObserveManagerLive) refuses it later.
func TestUpDecisions(t *testing.T) {
	want := map[string]upAction{
		"absent": upApply, "running": upNone, "running-exited": upNone, "running-created": upNone,
		"running-no-status": upNone, "suspended": upResume, "stopped": upApply, "error": upApply,
		"created": upApply, "provisioning": upApply, "cloning": upApply, "starting": upApply,
		"stopping": upApply, "resumed": upApply, "unknown": upApply,
	}
	for _, o := range testutil.ManagerObservations {
		w, ok := want[o.Name]
		if !ok {
			t.Errorf("%s: the observation set grew; pin up's decision for it", o.Name)
			continue
		}
		if got := upDecision(o.Phase, false); got != w {
			t.Errorf("%s: up = %s, want %s", o.Name, got, w)
		}
		if got := upDecision(o.Phase, true); got != upApply {
			t.Errorf("%s: up --fresh = %s, want apply", o.Name, got)
		}
	}
	// A failed probe that proves the manager is down is "absent"; any other
	// failure stops up rather than forcing an apply.
	if p, err := phaseOrAbsent("", errors.New("hub at http://127.0.0.1:8080 is not responding")); err != nil || upDecision(p, false) != upApply {
		t.Errorf("hub down: phase %q err %v", p, err)
	}
	if _, err := phaseOrAbsent("", errors.New("exit status 2")); err == nil {
		t.Error("an unknown probe failure must stop up")
	}
}

// convEntry is how conversation new and restore begin over an observation:
// the scion calls they made (all of them, to the end) or the refusal.
type convEntry struct {
	calls   string
	refusal string
}

// TestConversationDecisions pins how conversation new and restore start over
// the shared observation set. Both read the phase only: a running record is
// suspended first whatever its container shows.
func TestConversationDecisions(t *testing.T) {
	const noRecord, notReady = "no manager record", "running or suspended"
	refuse := func(why string) [2]convEntry { return [2]convEntry{{"", why}, {"", why}} }
	running := [2]convEntry{{"suspend,stop,resume-fresh", ""}, {"suspend,resume", ""}}
	// [0] is conversation new, [1] restore.
	want := map[string][2]convEntry{
		"absent": refuse(noRecord), "running": running, "running-exited": running,
		"running-created": running, "running-no-status": running,
		"suspended": {{"stop,resume-fresh", ""}, {"resume", ""}},
		"stopped":   refuse(notReady), "error": refuse(notReady), "created": refuse(notReady),
		"provisioning": refuse(notReady), "cloning": refuse(notReady), "starting": refuse(notReady),
		"stopping": refuse(notReady), "resumed": refuse(notReady), "unknown": refuse(notReady),
	}
	for _, o := range testutil.ManagerObservations {
		w, ok := want[o.Name]
		if !ok {
			t.Errorf("%s: the observation set grew; pin conversation new and restore for it", o.Name)
			continue
		}
		t.Run(o.Name, func(t *testing.T) {
			f, _ := newConvFixtureWith(t)
			writeConvFile(t, f.transcript("old.jsonl"), "{\"old\":1}\n")
			m, _, err := f.ops.backup(context.Background(), "manual")
			if err != nil {
				t.Fatal(err)
			}
			f.sc.phase, f.sc.container, f.sc.calls = o.Phase, o.Container, nil
			_, _, err = f.ops.newConversation(context.Background(), newConvRequest{Reason: "x"})
			checkConvEntry(t, "new", f.calls(), err, w[0])

			f.sc.phase, f.sc.container, f.sc.calls = o.Phase, o.Container, nil
			err = f.ops.restore(context.Background(), m.ID, false)
			checkConvEntry(t, "restore", f.calls(), err, w[1])
		})
	}
}

func checkConvEntry(t *testing.T, what, calls string, err error, w convEntry) {
	t.Helper()
	if calls != w.calls {
		t.Errorf("%s: calls %q, want %q", what, calls, w.calls)
	}
	switch {
	case w.refusal == "" && err != nil:
		t.Errorf("%s: %v", what, err)
	case w.refusal != "" && (err == nil || !strings.Contains(err.Error(), w.refusal)):
		t.Errorf("%s: err %v, want %q", what, err, w.refusal)
	}
}

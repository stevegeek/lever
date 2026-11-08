package broker

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/broker/registry"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/sentledger"
	"github.com/stevegeek/lever/internal/servicehost"
)

// msgRuntime is fakeRuntime that keeps what it was asked to send.
type msgRuntime struct {
	*fakeRuntime
	mu   sync.Mutex
	sent []scion.MsgOpts
}

func (m *msgRuntime) Message(_ context.Context, o scion.MsgOpts) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, o)
	return nil
}

func (m *msgRuntime) messages() []scion.MsgOpts {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]scion.MsgOpts(nil), m.sent...)
}

// hostWatchBroker is a broker whose tools "db" and "cal" run on service host
// "laptop", with the manager (slug "appname") in phase phase and a probe
// that answers *up.
func hostWatchBroker(t *testing.T, phase string, up *bool) (*Broker, *msgRuntime, *bytes.Buffer) {
	t.Helper()
	rt := &msgRuntime{fakeRuntime: &fakeRuntime{staticPhases: true, agents: map[string][]scion.Agent{
		testInstanceProject: {{Slug: "appname", Phase: phase}},
	}}}
	var audit bytes.Buffer
	b := New(testConfig(t, withAudit(&audit), withManager("test-manager", "appname"), withRuntime(rt)))
	b.serviceHosts = map[string]ServiceHostRoute{"db": {Host: "laptop"}, "cal": {Host: "laptop"}}
	for _, name := range []string{"db", "cal"} {
		if err := b.reg.Register(registry.Tool{Name: name, Backend: "https://127.0.0.1:9/" + name, External: true,
			Operations: map[string]registry.Operation{"read": {Name: "read"}}}); err != nil {
			t.Fatal(err)
		}
	}
	b.hostProbe = func(_ context.Context, ht hostTarget) bool {
		if ht.name != "laptop" || ht.infoURL != "https://127.0.0.1:9"+servicehost.InfoPath {
			t.Errorf("probe target %+v", ht)
		}
		return *up
	}
	return b, rt, &audit
}

// The manager hears once that a host it could not reach is back: not on the
// first probe after a start, not while the host stays up, and at most once
// per hostBackGap when it flaps.
func TestHostWatchTellsTheManagerWhenAHostIsBack(t *testing.T) {
	up := true
	b, rt, _ := hostWatchBroker(t, scion.PhaseRunning, &up)
	ctx := context.Background()
	b.watchHostsOnce(ctx) // first sighting: no news
	b.watchHostsOnce(ctx)
	if n := len(rt.messages()); n != 0 {
		t.Fatalf("a host that was never seen down was announced (%d messages)", n)
	}
	up = false
	b.watchHostsOnce(ctx)
	up = true
	b.watchHostsOnce(ctx)
	msgs := rt.messages()
	if len(msgs) != 1 {
		t.Fatalf("want one notice after down then up, got %d", len(msgs))
	}
	body := msgs[0].Body
	if msgs[0].To != "agent:appname" || msgs[0].Interrupt ||
		!strings.HasPrefix(body, serviceHostMarker) || !strings.Contains(body, "Service host laptop is reachable again") ||
		!strings.Contains(body, "Its tools: cal, db.") {
		t.Fatalf("notice %+v", msgs[0])
	}
	// Flapping within the gap: no second notice.
	up = false
	b.watchHostsOnce(ctx)
	up = true
	b.watchHostsOnce(ctx)
	if n := len(rt.messages()); n != 1 {
		t.Fatalf("a flap inside hostBackGap was announced again (%d messages)", n)
	}
	// After the gap it is news again.
	b.hostWatch.mu.Lock()
	b.hostWatch.state["laptop"].lastNotice = time.Now().Add(-hostBackGap - time.Second)
	b.hostWatch.mu.Unlock()
	up = false
	b.watchHostsOnce(ctx)
	up = true
	b.watchHostsOnce(ctx)
	if n := len(rt.messages()); n != 2 {
		t.Fatalf("want a second notice after the gap, got %d messages", n)
	}
}

// A manager that is not running is not messaged: it lists the tools afresh
// when it starts.
func TestHostWatchSkipsAManagerThatIsNotRunning(t *testing.T) {
	up := false
	b, rt, audit := hostWatchBroker(t, scion.PhaseSuspended, &up)
	b.watchHostsOnce(context.Background())
	up = true
	b.watchHostsOnce(context.Background())
	if n := len(rt.messages()); n != 0 {
		t.Fatalf("a suspended manager was messaged (%d)", n)
	}
	if !strings.Contains(audit.String(), "service host laptop back: not sent") {
		t.Fatalf("the skipped notice is not audited:\n%s", audit)
	}
}

// waitChecks waits for every probe-after-failure to finish.
func waitChecks(t *testing.T, b *Broker) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b.hostWatch.mu.Lock()
		n := len(b.hostWatch.checking)
		b.hostWatch.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a probe after a failed call never finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A failed tool call probes its host at once and marks it down only when
// the probe fails too, so an outage shorter than the probe interval is still
// announced, but a call that failed for the caller's own reasons is not.
func TestGatewayFailureProbesTheHost(t *testing.T) {
	r := newServiceHostRig(t, nil)
	r.b.manager = "test-manager"
	r.b.runtime = &msgRuntime{fakeRuntime: &fakeRuntime{}}
	up := true
	r.b.hostProbe = func(context.Context, hostTarget) bool { return up }
	r.b.hostWatch.markUp("laptop", time.Now()) // seen up

	// The host is fine but the call fails (here: the host's port is closed
	// while the probe, faked, still answers): not down.
	r.ts.Close()
	if w := r.call("read"); w.Code != 503 {
		t.Fatalf("call: %d", w.Code)
	}
	waitChecks(t, r.b)
	if r.b.hostWatch.markUp("laptop", time.Now()) {
		t.Fatal("a failed call whose host still answers staged a back notice")
	}
	// Now the probe fails as well: down, and the next up is news.
	up = false
	if w := r.call("read"); w.Code != 503 {
		t.Fatalf("call: %d", w.Code)
	}
	waitChecks(t, r.b)
	if !r.b.hostWatch.markUp("laptop", time.Now()) {
		t.Fatal("a failed call with a failed probe did not mark the host down")
	}
}

// failingRuntime fails every send.
type failingRuntime struct{ *msgRuntime }

func (f *failingRuntime) Message(context.Context, scion.MsgOpts) error {
	return errors.New("hub refused")
}

// A notice the hub did not take is sent again at the next check.
func TestHostWatchRetriesAFailedNotice(t *testing.T) {
	up := false
	b, rt, audit := hostWatchBroker(t, scion.PhaseRunning, &up)
	ok := b.runtime
	b.runtime = &failingRuntime{rt}
	b.watchHostsOnce(context.Background())
	up = true
	b.watchHostsOnce(context.Background())
	if !strings.Contains(audit.String(), "retried at the next check") {
		t.Fatalf("the failed send is not audited:\n%s", audit)
	}
	b.runtime = ok
	b.watchHostsOnce(context.Background())
	if n := len(rt.messages()); n != 1 {
		t.Fatalf("want the notice sent at the next check, got %d", n)
	}
}

// The notice is recorded: message_verify's ledger lookup finds it with its
// kind and exact text.
func TestHostWatchNoticeIsRecorded(t *testing.T) {
	up := false
	b, rt, _ := hostWatchBroker(t, scion.PhaseRunning, &up)
	b.sent = &sentRecord{dir: t.TempDir()}
	b.watchHostsOnce(context.Background())
	up = true
	b.watchHostsOnce(context.Background())
	msgs := rt.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages %d", len(msgs))
	}
	first, _, _ := strings.Cut(msgs[0].Body, "\n")
	ref := strings.TrimPrefix(first, serviceHostMarker+" ref=")
	led, err := b.sent.get(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := led.ByRef("test-manager", ref)
	if err != nil || !ok {
		t.Fatalf("ByRef(%q): %v %v", ref, ok, err)
	}
	if got.Kind != sentledger.KindServiceHostNotice || got.Body != msgs[0].Body {
		t.Fatalf("recorded %+v", got)
	}
}

func TestHostWatchNeedsAServiceHostAndAManager(t *testing.T) {
	up := true
	b, _, _ := hostWatchBroker(t, scion.PhaseRunning, &up)
	if !b.hostWatchEnabled() {
		t.Fatal("watch off with a service host, a runtime and a manager")
	}
	b.serviceHosts = nil
	if b.hostWatchEnabled() {
		t.Fatal("watch on with no service host")
	}
}

// The real probe reaches a live service host's info page over the gateway's
// pinned transport, and fails once the host is gone.
func TestProbeHostAgainstARealServiceHost(t *testing.T) {
	r := newServiceHostRig(t, nil)
	targets := r.b.serviceHostTargets()
	if len(targets) != 1 || targets[0].name != "laptop" {
		t.Fatalf("targets %+v", targets)
	}
	if !probeHost(context.Background(), targets[0]) {
		t.Fatal("a live service host probed as down")
	}
	r.ts.Close()
	if probeHost(context.Background(), targets[0]) {
		t.Fatal("a closed service host probed as up")
	}
}

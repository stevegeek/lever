package host

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/testutil"
	"github.com/stevegeek/lever/internal/wire"
)

const (
	idOurs  = "0123456789abcdef0123456789abcdef"
	idOther = "fedcba9876543210fedcba9876543210"
)

func answer(id, version, hash string) broker.EpochAnswer {
	return broker.EpochAnswer{EpochResponse: wire.EpochResponse{Version: version, ConfigHash: hash}, InstanceID: id}
}

// judgeRunningBroker: reuse or restart only this instance's broker; another
// instance's, and one without an id, are refused.
func TestJudgeRunningBroker(t *testing.T) {
	cases := []struct {
		name string
		got  broker.EpochAnswer
		ours string
		want brokerAction
	}{
		{"ours, same binary and config", answer(idOurs, "v", "h"), idOurs, brokerReuse},
		{"ours, other binary", answer(idOurs, "v0", "h"), idOurs, brokerRestart},
		{"ours, other config", answer(idOurs, "v", "h0"), idOurs, brokerRestart},
		{"another instance, identical version and config", answer(idOther, "v", "h"), idOurs, brokerRefuse},
		{"predates ids", answer("", "v0", ""), idOurs, brokerRefuse},
		{"predates ids even with matching fields", answer("", "v", "h"), idOurs, brokerRefuse},
		{"no id of our own", answer(idOther, "v", "h"), "", brokerRefuse},
		{"neither has an id", answer("", "v", "h"), "", brokerRefuse},
	}
	for _, c := range cases {
		if got := judgeRunningBroker(c.got, c.ours, "v", "h"); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func epochServer(t *testing.T, a broker.EpochAnswer) (url string, port int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wire.PathEpoch {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(a)
	}))
	t.Cleanup(srv.Close)
	_, p, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ = strconv.Atoi(p)
	return srv.URL, port
}

func stateWithID(t *testing.T) (state.State, string) {
	t.Helper()
	st := state.State{Dir: t.TempDir()}
	id, err := st.EnsureInstanceID()
	if err != nil {
		t.Fatal(err)
	}
	return st, id
}

// Another instance's broker on this admin port, same version and hash: apply
// refuses it and stops nothing (the pid file names this test process).
func TestBrokerStartRefusesAnotherInstancesBroker(t *testing.T) {
	st, _ := stateWithID(t)
	app := &config.App{Name: "a"}
	url, port := epochServer(t, answer(idOther, cli.VersionString(), ""))
	app.Broker.AdminPort = port
	// A stop would kill the test.
	if err := os.WriteFile(st.PID(), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	bc := &brokerController{app: app, state: st, adminURL: url, admin: http.DefaultClient}
	err := bc.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "belongs to another lever instance") {
		t.Fatalf("Start = %v, want the another-instance refusal", err)
	}
	if !strings.Contains(err.Error(), "broker.admin_port") {
		t.Errorf("the refusal does not name the ports to change: %v", err)
	}
}

// A broker without an instance id and no live broker of ours: refused, never
// adopted.
func TestBrokerStartRefusesAnUnownedOldBroker(t *testing.T) {
	st, _ := stateWithID(t)
	url, port := epochServer(t, answer("", "v0.35.0", "h"))
	app := &config.App{Name: "a"}
	app.Broker.AdminPort = port
	bc := &brokerController{app: app, state: st, adminURL: url, admin: http.DefaultClient}
	if err := bc.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "no instance id") {
		t.Fatalf("Start = %v, want the no-instance-id refusal", err)
	}
}

// This instance's broker, same binary and config: reused, no spawn.
func TestBrokerStartReusesItsOwnBroker(t *testing.T) {
	st, id := stateWithID(t)
	app := &config.App{Name: "a"}
	// ConfigHash(app) covers the port, so answer with the hash it will have.
	url, port := epochServerFor(t, app, id)
	app.Broker.AdminPort = port
	bc := &brokerController{app: app, state: st, adminURL: url, admin: http.DefaultClient}
	if err := bc.Start(context.Background()); err != nil {
		t.Fatalf("Start = %v, want reuse", err)
	}
}

func epochServerFor(t *testing.T, app *config.App, id string) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	probe := *app
	probe.Broker.AdminPort = port
	a := answer(id, cli.VersionString(), brokerctl.ConfigHash(&probe))
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(a)
	})}}
	srv.Start()
	t.Cleanup(srv.Close)
	return fmt.Sprintf("http://127.0.0.1:%d", port), port
}

// Healthy accepts only this instance's broker: another instance's answer
// fails at once rather than after the timeout.
func TestBrokerHealthyChecksTheInstance(t *testing.T) {
	st, id := stateWithID(t)
	app := &config.App{Name: "a"}
	url, port := epochServerFor(t, app, id)
	app.Broker.AdminPort = port
	bc := &brokerController{app: app, state: st, adminURL: url, admin: http.DefaultClient}
	if err := bc.Healthy(context.Background()); err != nil {
		t.Fatalf("own broker: %v", err)
	}
	url, _ = epochServerFor(t, app, idOther)
	bc.adminURL = url
	if err := bc.Healthy(context.Background()); err == nil || !strings.Contains(err.Error(), "another lever instance") {
		t.Fatalf("other broker: %v", err)
	}
}

// revoke and bump-epoch act only on this instance's broker.
func TestRequireOwnBroker(t *testing.T) {
	st, id := stateWithID(t)
	for _, c := range []struct {
		name string
		id   string
		want string
	}{
		{"own", id, ""},
		{"another instance", idOther, "another lever instance"},
		{"predates ids", "", "no instance id"},
	} {
		_, port := epochServer(t, answer(c.id, "v", "h"))
		err := requireOwnBroker(context.Background(), http.DefaultClient, port, st)
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	_, port := epochServer(t, answer(id, "v", "h"))
	if err := requireOwnBroker(context.Background(), http.DefaultClient, port, state.State{Dir: t.TempDir()}); err == nil {
		t.Error("a state dir with no id accepted a broker")
	}
}

// This instance's broker from an older binary, with no live pid: the stop
// stops nothing, the broker still answers, and apply refuses rather than
// adopt whatever holds the port.
func TestBrokerStartRefusesWhenTheStopLeavesABroker(t *testing.T) {
	defer func(w time.Duration) { brokerQuietWait = w }(brokerQuietWait)
	brokerQuietWait = 300 * time.Millisecond
	st, id := stateWithID(t)
	url, port := epochServer(t, answer(id, "some-older-binary", "h"))
	app := &config.App{Name: "a"}
	app.Broker.AdminPort = port
	bc := &brokerController{app: app, state: st, adminURL: url, admin: http.DefaultClient}
	if err := bc.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "still answers") || !strings.Contains(err.Error(), "stop the broker by hand") {
		t.Fatalf("Start = %v, want the still-answers refusal", err)
	}
}

// instanceState is a state dir with an instance id beside a config, and that
// config's path.
func instanceState(t *testing.T) (state.State, string) {
	t.Helper()
	root := t.TempDir()
	st := state.ForConfig(root)
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureInstanceID(); err != nil {
		t.Fatal(err)
	}
	return st, filepath.Join(root, "lever.yaml")
}

// A broker without an id answers and broker.pid names a reused pid (this test
// process): a stale file, so apply refuses and signals nothing.
func TestBrokerStartTreatsAReusedPIDAsStale(t *testing.T) {
	st, _ := instanceState(t)
	url, port := epochServer(t, answer("", "v0.35.0", "h"))
	app := &config.App{Name: "a"}
	app.Broker.AdminPort = port
	if err := os.WriteFile(st.PID(), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	bc := &brokerController{app: app, state: st, adminURL: url, admin: http.DefaultClient}
	if err := bc.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "no instance id") {
		t.Fatalf("Start = %v, want the no-instance-id refusal", err)
	}
}

// A broker with no instance id is refused even when broker.pid names this
// instance's `lever broker serve`, and nothing is stopped.
func TestBrokerStartRefusesAnIDLessBrokerOfOurs(t *testing.T) {
	st, cfg := instanceState(t)
	url, port := epochServer(t, answer("", "v0.35.0", "h"))
	app := &config.App{Name: "a"}
	app.Broker.AdminPort = port
	old, oldDone := testutil.StartServeProcess(t, state.BrokerDaemon, cfg)
	if err := os.WriteFile(st.PID(), []byte(strconv.Itoa(old.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	bc := &brokerController{app: app, state: st, adminURL: url, admin: http.DefaultClient}
	if err := bc.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "no instance id") {
		t.Fatalf("Start = %v, want the no-instance-id refusal", err)
	}
	select {
	case <-oldDone:
		t.Fatal("an id-less broker was stopped")
	case <-time.After(200 * time.Millisecond):
	}
}

// heldBrokerErr: only the broker this apply runs is accepted; every other
// answer names the port, and another instance's names the ports to change.
func TestHeldBrokerErr(t *testing.T) {
	if err := heldBrokerErr(answer(idOurs, "v", "h"), idOurs, "v", "h", 8444); err != nil {
		t.Fatalf("own broker: %v", err)
	}
	for _, c := range []struct {
		name string
		got  broker.EpochAnswer
		ours string
		want string
	}{
		{"another instance, same binary and config", answer(idOther, "v", "h"), idOurs, "broker.admin_port"},
		{"ours, other binary", answer(idOurs, "0.34.2", "h"), idOurs, "not as the broker this apply runs"},
		{"ours, other config", answer(idOurs, "v", "other"), idOurs, "not as the broker this apply runs"},
		{"predates ids, same binary and config", answer("", "v", "h"), idOurs, "no instance id"},
		{"no id of our own", answer(idOurs, "v", "h"), "", "another lever instance"},
	} {
		err := heldBrokerErr(c.got, c.ours, "v", "h", 8444)
		if err == nil || !strings.Contains(err.Error(), "127.0.0.1:8444") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want the port and %q", c.name, err, c.want)
		}
	}
}

func TestBrokerStillAnswering(t *testing.T) {
	url, _ := epochServer(t, answer(idOther, "0.34.2", "x"))
	bc := &brokerController{adminURL: url, admin: http.DefaultClient}
	if got, ok := bc.stillAnswering(context.Background(), 300*time.Millisecond); !ok || got.Version != "0.34.2" || got.InstanceID != idOther {
		t.Fatalf("answering broker: %+v %v", got, ok)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + ln.Addr().String()
	ln.Close()
	bc.adminURL = closed
	if _, ok := bc.stillAnswering(context.Background(), 300*time.Millisecond); ok {
		t.Fatal("a closed port must not read as answering")
	}
}

// stillAnswering waits out a broker that answers a few more probes and then
// falls silent.
func TestBrokerStillAnsweringWaitsOutASlowStop(t *testing.T) {
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		left := n <= 3
		mu.Unlock()
		if !left {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(answer(idOurs, "v", "h"))
	}))
	t.Cleanup(srv.Close)
	bc := &brokerController{adminURL: srv.URL, admin: http.DefaultClient}
	start := time.Now()
	if got, ok := bc.stillAnswering(context.Background(), 5*time.Second); ok {
		t.Fatalf("a broker that fell silent read as answering: %+v", got)
	}
	if time.Since(start) >= 5*time.Second {
		t.Fatalf("took %s: quiet must end the wait", time.Since(start))
	}
}

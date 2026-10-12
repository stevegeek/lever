package host

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/wire"
)

const (
	idOurs  = "0123456789abcdef0123456789abcdef"
	idOther = "fedcba9876543210fedcba9876543210"
)

func answer(id, version, hash string) broker.EpochAnswer {
	return broker.EpochAnswer{EpochResponse: wire.EpochResponse{Version: version, ConfigHash: hash}, InstanceID: id}
}

// judgeRunningBroker (lever#181): reuse or restart only this instance's
// broker; another instance's is refused; one predating ids is restarted only
// when this instance's pid file names a live process.
func TestJudgeRunningBroker(t *testing.T) {
	cases := []struct {
		name  string
		got   broker.EpochAnswer
		ours  string
		alive bool
		want  brokerAction
	}{
		{"ours, same binary and config", answer(idOurs, "v", "h"), idOurs, true, brokerReuse},
		{"ours, other binary", answer(idOurs, "v0", "h"), idOurs, true, brokerRestart},
		{"ours, other config", answer(idOurs, "v", "h0"), idOurs, true, brokerRestart},
		{"another instance, identical version and config", answer(idOther, "v", "h"), idOurs, true, brokerRefuse},
		{"another instance, our pid file dead", answer(idOther, "v", "h"), idOurs, false, brokerRefuse},
		{"predates ids, our broker alive", answer("", "v0", ""), idOurs, true, brokerRestart},
		{"predates ids, nothing of ours runs", answer("", "v0", ""), idOurs, false, brokerRefuse},
		{"predates ids even with matching fields, nothing of ours runs", answer("", "v", "h"), idOurs, false, brokerRefuse},
		{"no id of our own", answer(idOther, "v", "h"), "", true, brokerRefuse},
	}
	for _, c := range cases {
		if got := judgeRunningBroker(c.got, c.ours, "v", "h", c.alive); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// epochServer answers /epoch with a, on a loopback port.
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

// stateWithID makes a state dir whose instance id is returned.
func stateWithID(t *testing.T) (state.State, string) {
	t.Helper()
	st := state.State{Dir: t.TempDir()}
	id, err := st.EnsureInstanceID()
	if err != nil {
		t.Fatal(err)
	}
	return st, id
}

// Another instance's broker on this instance's admin port, with the same
// version and config hash: apply refuses it and leaves it alone (no stop:
// the pid file this test writes names a process that must survive).
func TestBrokerStartRefusesAnotherInstancesBroker(t *testing.T) {
	st, _ := stateWithID(t)
	app := &config.App{Name: "a"}
	url, port := epochServer(t, answer(idOther, cli.VersionString(), ""))
	app.Broker.AdminPort = port
	// Our pid file names this test process: a stop would kill the test.
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

// A broker predating instance ids, with no live broker of ours: refused,
// never adopted.
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

// epochServerFor answers with id, this binary's version and the config hash
// app will have once its admin port is the server's.
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
	url, _ := epochServer(t, answer(id, "v", "h"))
	bc := &brokerController{app: app, state: st, adminURL: url, admin: http.DefaultClient}
	if err := bc.Healthy(context.Background()); err != nil {
		t.Fatalf("own broker: %v", err)
	}
	url, _ = epochServer(t, answer(idOther, "v", "h"))
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
	// No id of our own yet: no broker can be ours.
	_, port := epochServer(t, answer(id, "v", "h"))
	if err := requireOwnBroker(context.Background(), http.DefaultClient, port, state.State{Dir: t.TempDir()}); err == nil {
		t.Error("a state dir with no id accepted a broker")
	}
}

// This instance's broker from an older binary, but its pid file names no
// process: the stop stops nothing, the broker still answers, and apply
// refuses rather than adopting whatever holds the port.
func TestBrokerStartRefusesWhenTheStopLeavesABroker(t *testing.T) {
	st, id := stateWithID(t)
	url, port := epochServer(t, answer(id, "some-older-binary", "h"))
	app := &config.App{Name: "a"}
	app.Broker.AdminPort = port
	bc := &brokerController{app: app, state: st, adminURL: url, admin: http.DefaultClient}
	if err := bc.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "still answers") {
		t.Fatalf("Start = %v, want the still-answers refusal", err)
	}
}

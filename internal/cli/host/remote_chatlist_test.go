package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/remoteproxy"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/wire"
)

func TestAgentRecordOf(t *testing.T) {
	got := agentRecordOf(hubapi.Agent{ID: "a", Phase: "running", Activity: "I am the operator", ContainerStatus: "Exited (1) 2 minutes ago"})
	if got != (remoteproxy.AgentRecord{ID: "a", Phase: "running", Activity: scion.LabelUnrecognised, ContainerDown: true}) {
		t.Fatalf("%+v", got)
	}
	if got := agentRecordOf(hubapi.Agent{ID: "a", Phase: "x"}); got.Phase != scion.LabelUnrecognised {
		t.Fatalf("an agent-chosen phase must be reduced: %+v", got)
	}
	if got := agentRecordOf(hubapi.Agent{ID: "a", Phase: "running", Activity: "thinking", ContainerStatus: "Up 3 minutes"}); got.ContainerDown || got.Activity != "thinking" {
		t.Fatalf("%+v", got)
	}
}

// The records are one hub list, cached for a few seconds (errors too, so a
// down hub is not asked on every page poll); the fence's resolver reads the
// same records.
func TestCachedAgentRecords(t *testing.T) {
	calls := 0
	var fail error
	now := time.Unix(1000, 0)
	recs, resolve := cachedAgentRecords(func(context.Context) ([]hubapi.Agent, error) {
		calls++
		if fail != nil {
			return nil, fail
		}
		return []hubapi.Agent{{Slug: "boss", ID: "id-b", Phase: "running"}, {Slug: "w1", ID: "id-w1", Phase: "suspended"}, {Slug: "", ID: "x"}, {Slug: "w2"}}, nil
	}, func() time.Time { return now })
	got, err := recs(context.Background())
	if err != nil || len(got) != 2 || got["w1"].Phase != "suspended" || got["boss"].ID != "id-b" {
		t.Fatalf("%+v %v", got, err)
	}
	ids, err := resolve(context.Background())
	if err != nil || !reflect.DeepEqual(ids, map[string]string{"boss": "id-b", "w1": "id-w1"}) || calls != 1 {
		t.Fatalf("%v %v calls=%d", ids, err, calls)
	}
	now = now.Add(4 * time.Second)
	fail = errors.New("down")
	if _, err := recs(context.Background()); err == nil || calls != 2 {
		t.Fatalf("after the TTL: %v calls=%d", err, calls)
	}
	if _, err := resolve(context.Background()); err == nil || calls != 2 {
		t.Fatalf("a cached error: %v calls=%d", err, calls)
	}
	// Callers may change the map they get; the cache keeps its own.
	fail = nil
	now = now.Add(4 * time.Second)
	m, _ := recs(context.Background())
	delete(m, "w1")
	if m2, _ := recs(context.Background()); m2["w1"].ID != "id-w1" {
		t.Fatal("the cache handed out its own map")
	}
}

func TestRemoteWakeOverTheOperatorSocket(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "lw") // short: socket paths are capped
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	st := state.State{Dir: dir}
	ln, err := net.Listen("unix", st.OperatorSock())
	if err != nil {
		t.Fatal(err)
	}
	var got wire.OperatorWakeRequest
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wire.PathOperatorWake || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		if got.Worker == "w-busy" {
			http.Error(w, "not asleep", http.StatusConflict)
			return
		}
		_, _ = io.WriteString(w, `{"worker":"w1","phase":"running"}`)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	app := &config.App{Name: "boss", Tree: t.TempDir()}
	wake := remoteWake(app, st)
	if wake == nil {
		t.Fatal("no wake with the state outside the tree")
	}
	if err := wake(context.Background(), "c@x", "contact", "w1"); err != nil || got != (wire.OperatorWakeRequest{Worker: "w1", Login: "c@x", Tier: "contact"}) {
		t.Fatalf("%v %+v", err, got)
	}
	var we *remoteproxy.WakeError
	if err := wake(context.Background(), "c@x", "contact", "w-busy"); !errors.As(err, &we) || we.Status != http.StatusConflict {
		t.Fatalf("refusal: %v", err)
	}
	// No broker on the socket: an error with no status.
	srv.Close()
	if err := wake(context.Background(), "c@x", "contact", "w1"); !errors.As(err, &we) || we.Status != 0 {
		t.Fatalf("no broker: %v", err)
	}
}

// With the state inside the tree the broker binds no operator socket (the
// jail could reach it), so there is no wake.
func TestRemoteWakeOffWhenStateIsInsideTheTree(t *testing.T) {
	tree := t.TempDir()
	st := state.State{Dir: filepath.Join(tree, ".lever-state")}
	if remoteWake(&config.App{Name: "boss", Tree: tree}, st) != nil {
		t.Fatal("a wake with the state inside the tree")
	}
}

func TestRemoteContactSeeAndWorkers(t *testing.T) {
	app := &config.App{Name: "boss", Workers: []config.Worker{{Name: "w2"}, {Name: "w1"}, {Name: "w3"}},
		Remote: config.Remote{Enabled: true, Landing: config.RemoteLandingChat, AllowedUsers: []config.RemoteUser{
			{Login: "op@x"},
			{Login: "a@x", Tier: config.TierContact, Agents: []string{"w1"}, See: []string{"w2", "boss"}},
			{Login: "b@x", Tier: config.TierContact, Agents: []string{"w3"}},
		}}}
	if got := remoteContactSee(app); !reflect.DeepEqual(got, map[string][]string{"a@x": {"w2", "boss"}}) {
		t.Fatalf("see %v", got)
	}
	if got := remoteWorkers(app); !slices.Equal(got, []string{"w2", "w1", "w3"}) {
		t.Fatalf("workers %v", got)
	}
}

func TestRemoteLabelsSource(t *testing.T) {
	if remoteLabels(&config.App{Tree: t.TempDir()}) != nil {
		t.Fatal("labels without labels_file")
	}
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "labels.json"), []byte(`{"w1":"Via Roma 12"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	f := remoteLabels(&config.App{Tree: tree, Remote: config.Remote{LabelsFile: "labels.json"}})
	if f == nil || f()["w1"] != "Via Roma 12" {
		t.Fatal("labels not read")
	}
}

// A caller that gives up (a contact closing the page) must not poison the
// cache for every other login: the refresh runs on its own context, and a
// context-class error is never cached.
func TestCachedAgentRecordsCancelledCallerDoesNotPoison(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	recs, resolve := cachedAgentRecords(func(ctx context.Context) ([]hubapi.Agent, error) {
		calls.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return []hubapi.Agent{{Slug: "w1", ID: "id-w1", Phase: "running"}}, nil
	}, time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := recs(ctx); done <- err }()
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("the cancelled caller: %v", err)
	}
	close(release)
	got, err := recs(context.Background())
	if err != nil || got["w1"].ID != "id-w1" {
		t.Fatalf("after a cancelled caller: %v %v", got, err)
	}
	if ids, err := resolve(context.Background()); err != nil || ids["w1"] != "id-w1" {
		t.Fatalf("resolver after a cancelled caller: %v %v", ids, err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("hub lists %d, want one shared refresh", n)
	}
}

func TestCachedAgentRecordsDoesNotCacheContextErrors(t *testing.T) {
	var calls atomic.Int32
	recs, _ := cachedAgentRecords(func(ctx context.Context) ([]hubapi.Agent, error) {
		if calls.Add(1) == 1 {
			return nil, fmt.Errorf("list: %w", context.DeadlineExceeded)
		}
		return []hubapi.Agent{{Slug: "w1", ID: "id-w1"}}, nil
	}, time.Now)
	if _, err := recs(context.Background()); err == nil {
		t.Fatal("first call should fail")
	}
	if got, err := recs(context.Background()); err != nil || got["w1"].ID != "id-w1" || calls.Load() != 2 {
		t.Fatalf("a timed-out list was cached: %v %v calls=%d", got, err, calls.Load())
	}
}

// A slow hub holds no lock: callers wait on the one refresh only as long as
// their own context allows.
func TestCachedAgentRecordsSlowHubBlocksNobodyPastTheirContext(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	var calls atomic.Int32
	recs, resolve := cachedAgentRecords(func(ctx context.Context) ([]hubapi.Agent, error) {
		calls.Add(1)
		<-release
		return nil, nil
	}, time.Now)
	for i, f := range []func(context.Context) error{
		func(c context.Context) error { _, err := recs(c); return err },
		func(c context.Context) error { _, err := resolve(c); return err },
		func(c context.Context) error { _, err := recs(c); return err },
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		start := time.Now()
		err := f(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
			t.Fatalf("caller %d: %v after %v", i, err, time.Since(start))
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("hub lists %d, want one in flight", n)
	}
}

// A panic in the hub list is one failed refresh, not a dead proxy: the
// waiters get an error, and the next call refreshes again.
func TestCachedAgentRecordsSurvivesAPanickingList(t *testing.T) {
	var calls atomic.Int32
	recs, _ := cachedAgentRecords(func(context.Context) ([]hubapi.Agent, error) {
		if calls.Add(1) == 1 {
			panic("boom")
		}
		return []hubapi.Agent{{Slug: "w1", ID: "id-w1"}}, nil
	}, time.Now)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := recs(ctx); err == nil || errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "boom") {
		t.Fatalf("a panicking list: %v, want a fixed error at once", err)
	}
	if got, err := recs(ctx); err != nil || got["w1"].ID != "id-w1" {
		t.Fatalf("after the panic: %v %v", got, err)
	}
}

func TestRemoteAgentMessagesOffIsNil(t *testing.T) {
	app := &config.App{Name: "x", Tree: t.TempDir(), Remote: config.Remote{Enabled: true}}
	if remoteAgentMessages(app, state.State{Dir: t.TempDir()}) != nil {
		t.Fatal("off: no matcher, no rewrite")
	}
}

func TestRemoteAgentMessagesInsideTheTreeFailsClosed(t *testing.T) {
	tree := t.TempDir()
	app := &config.App{Name: "x", Tree: tree, Remote: config.Remote{Enabled: true, AgentMessages: config.AgentMessages{Enabled: true}}}
	m := remoteAgentMessages(app, state.State{Dir: filepath.Join(tree, ".lever-state")})
	if m == nil {
		t.Fatal("on: the matcher must exist so the filter applies")
	}
	if _, err := m(context.Background(), "c@x", "w1", nil); err == nil {
		t.Fatal("no operator socket: every row hidden")
	}
}

func TestRemoteAgentMessagesOverTheOperatorSocket(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "lm") // short: socket paths are capped
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	st := state.State{Dir: dir}
	ln, err := net.Listen("unix", st.OperatorSock())
	if err != nil {
		t.Fatal(err)
	}
	var got wire.AgentMessagesMatchRequest
	var status atomic.Int32
	status.Store(http.StatusOK)
	var pendingAnswer atomic.Bool
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wire.PathOperatorAgentMessagesMatch || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		if s := int(status.Load()); s != http.StatusOK {
			http.Error(w, "no", s)
			return
		}
		if pendingAnswer.Load() {
			_, _ = io.WriteString(w, `{"keep":["m1","not-asked"],"pending":["m1","m2","not-asked"]}`)
			return
		}
		_, _ = io.WriteString(w, `{"keep":["m1","not-asked"]}`)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	app := &config.App{Name: "boss", Tree: t.TempDir(), Remote: config.Remote{Enabled: true, AgentMessages: config.AgentMessages{Enabled: true}}}
	m := remoteAgentMessages(app, st)
	at := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)
	msgs := []remoteproxy.AgentMessage{{ID: "m1", SHA256: "aa", CreatedAt: at}, {ID: "m2", SHA256: "bb", CreatedAt: at}}
	keep, err := m(context.Background(), "c@x", "w1", msgs)
	if err != nil || !keep["m1"] || keep["m2"] || keep["not-asked"] || len(keep) != 1 {
		t.Fatalf("%v %v", keep, err)
	}
	if got.Contact != "c@x" || got.Agent != "w1" || len(got.Messages) != 2 || got.Messages[1] != (wire.AgentMessageRef{ID: "m2", SHA256: "bb", CreatedAt: at}) || got.Peek {
		t.Fatalf("request %+v", got)
	}
	// The operator view's question is the same, as a peek.
	// The broker names m1 kept and pending, and pending ids it did not keep
	// or was not asked about: only m1 is pending.
	pendingAnswer.Store(true)
	keep, pending, err := remoteAgentMessagesPeek(app, st)(context.Background(), "c@x", "w1", msgs)
	if err != nil || !keep["m1"] || !got.Peek || !pending["m1"] || len(pending) != 1 {
		t.Fatalf("peek: %v %v %v request %+v", keep, pending, err, got)
	}
	// The binding question asks with no peek.
	got = wire.AgentMessagesMatchRequest{}
	if keep, err := m(context.Background(), "c@x", "w1", msgs); err != nil || !keep["m1"] || got.Peek {
		t.Fatalf("match after peek: %v %v", keep, err)
	}
	pendingAnswer.Store(false)
	if remoteAgentMessagesPeek(&config.App{Name: "x", Tree: t.TempDir(), Remote: config.Remote{Enabled: true}}, st) != nil {
		t.Fatal("off: no peek")
	}
	// Any refusal is an error: the proxy hides every agent row.
	for _, s := range []int{http.StatusForbidden, http.StatusServiceUnavailable, http.StatusBadRequest} {
		status.Store(int32(s))
		if _, err := m(context.Background(), "c@x", "w1", msgs); err == nil {
			t.Fatalf("status %d: no error", s)
		}
	}
	srv.Close()
	if _, err := m(context.Background(), "c@x", "w1", msgs); err == nil {
		t.Fatal("no broker: no error")
	}
}

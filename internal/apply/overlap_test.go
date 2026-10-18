package apply

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/progress"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/testutil"
)

// overlapTimeout bounds a test's wait for the other side of an overlap, so a
// run that is not concurrent fails instead of hanging.
const overlapTimeout = 5 * time.Second

var testImage = helloApp("").ManagerImage()

// idleTicker's stop closes the channel, as a real one does.
func idleTicker(time.Duration) (<-chan time.Time, func()) {
	ch := make(chan time.Time)
	var once sync.Once
	return ch, func() { once.Do(func() { close(ch) }) }
}

func runWithProgress(t *testing.T, deps Deps) (string, error) {
	t.Helper()
	return runWithProgressCtx(context.Background(), t, deps, &syncOut{})
}

type syncOut struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (o *syncOut) Write(p []byte) (int, error) { o.mu.Lock(); defer o.mu.Unlock(); return o.b.Write(p) }
func (o *syncOut) String() string              { o.mu.Lock(); defer o.mu.Unlock(); return o.b.String() }

func (o *syncOut) waitFor(want string) bool {
	deadline := time.Now().Add(overlapTimeout)
	for time.Now().Before(deadline) {
		if strings.Contains(o.String(), want) {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

func runWithProgressCtx(ctx context.Context, t *testing.T, deps Deps, out *syncOut) (string, error) {
	t.Helper()
	app, f := newObserveFirstApp(t)
	if deps.Scion == nil {
		deps.Scion = scion.New(&agentLifecycleRunner{FakeRunner: f, slug: "hello"}, scion.Options{})
	}
	rep := progress.New(progress.Options{Mode: progress.ModePlain, Out: out, Ticker: idleTicker})
	err := Run(progress.With(ctx, rep), app, fillDeps(deps), PlanOpts{})
	rep.Finish(err)
	return out.String(), err
}

func TestLoadImageRunsAlongsideTheHubSteps(t *testing.T) {
	identityRan := make(chan struct{})
	var loadSawIdentity atomic.Bool
	out, err := runWithProgress(t, Deps{
		LoadImage: func(ctx context.Context, _ string) error {
			select {
			case <-identityRan:
				loadSawIdentity.Store(true)
				return nil
			case <-time.After(overlapTimeout):
				return errors.New("hub-identity never ran while the image loaded")
			}
		},
		EnsureHubIdentity: func(context.Context) error { close(identityRan); return nil },
	})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out)
	}
	if !loadSawIdentity.Load() {
		t.Fatal("load-image did not overlap hub-identity")
	}
	if !strings.Contains(out, "load-image "+testImage+" ... (alongside the next steps)") || !strings.Contains(out, "load-image "+testImage+" ok") {
		t.Fatalf("progress list:\n%s", out)
	}
}

// The hub kept running and its fake runtime broker never reports online, so
// start-manager makes its own runtime-broker wait and that call shows when
// it began.
func TestStartManagerWaitsForTheImagesAndTheBroker(t *testing.T) {
	var warmed, healthy atomic.Bool
	var mu sync.Mutex
	var ready []bool // per WaitBrokerReady call; start-manager's is the last
	_, f := newObserveFirstApp(t)
	keptHub(f, helloApp(""))
	_, err := runWithProgress(t, Deps{
		Scion:      scion.New(&agentLifecycleRunner{FakeRunner: f, slug: "hello"}, scion.Options{}),
		HubAuthEnv: testHubAuthEnv(),
		WarmImage: func(context.Context, string) error {
			time.Sleep(20 * time.Millisecond)
			warmed.Store(true)
			return nil
		},
		BrokerHealthy: func(context.Context) error {
			time.Sleep(20 * time.Millisecond)
			healthy.Store(true)
			return nil
		},
		WaitBrokerReady: func(context.Context, string) (bool, error) {
			mu.Lock()
			ready = append(ready, warmed.Load() && healthy.Load())
			mu.Unlock()
			return false, nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(ready) != 2 || !ready[1] {
		t.Fatalf("runtime-broker waits (lanes done at each) = %v; want two, start-manager's after the image warm-up and the broker health check", ready)
	}
}

// With the lanes running, the scion-server step's online answer still
// carries to start-manager (no lane step restarts the hub), so the runtime
// broker is asked about only once.
func TestRuntimeBrokerSeenOnlineSkipsStartManagersWait(t *testing.T) {
	var mu sync.Mutex
	var projects []string
	_, err := runWithProgress(t, Deps{
		WarmImage: func(context.Context, string) error {
			time.Sleep(20 * time.Millisecond)
			return nil
		},
		WaitBrokerReady: func(_ context.Context, project string) (bool, error) {
			mu.Lock()
			projects = append(projects, project)
			mu.Unlock()
			return true, nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(projects) != 0 {
		t.Fatalf("WaitBrokerReady calls = %q; want none after the scion-server step's hard wait", projects)
	}
}

func TestMintWaitsForTheBroker(t *testing.T) {
	var healthy, ok atomic.Bool
	_, err := runWithProgress(t, Deps{
		BrokerHealthy: func(context.Context) error {
			time.Sleep(20 * time.Millisecond)
			healthy.Store(true)
			return nil
		},
		MintManagerBootstrap: func(context.Context) (BootstrapMaterial, error) {
			ok.Store(healthy.Load())
			return BootstrapMaterial{Ticket: "t"}, nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !ok.Load() {
		t.Fatal("mint ran before the broker was healthy")
	}
}

func TestBackgroundFailureStopsTheRunAfterTheRunningStep(t *testing.T) {
	identityStarted := make(chan struct{})
	var identityDone, laterRan atomic.Bool
	out := &syncOut{}
	_, err := runWithProgressCtx(context.Background(), t, Deps{
		LoadImage: func(context.Context, string) error {
			select {
			case <-identityStarted:
			case <-time.After(overlapTimeout):
			}
			return errBoom
		},
		EnsureHubIdentity: func(context.Context) error {
			close(identityStarted)
			// The step in front ends only once the lane step is listed
			// as failed.
			out.waitFor(testImage + " FAILED")
			identityDone.Store(true)
			return nil
		},
		EnsureAgentTemplate: func(context.Context, string) (bool, error) {
			laterRan.Store(true)
			return false, nil
		},
	}, out)
	testutil.WantErrIs(t, err, errBoom)
	if !strings.Contains(err.Error(), "step load-image") {
		t.Fatalf("error does not name the failed step: %v", err)
	}
	if !identityDone.Load() {
		t.Fatal("the running foreground step was not left to finish")
	}
	if laterRan.Load() {
		t.Fatal("a step after the background failure ran")
	}
	if got := out.String(); !strings.Contains(got, "load-image "+testImage+" FAILED") || !strings.Contains(got, "hub-identity ok") {
		t.Fatalf("progress list:\n%s", got)
	}
}

// Mint waits for the broker lane; the image lane failing during that wait
// stops the run before mint.
func TestLaneFailureStopsAStepWaitingForAnotherLane(t *testing.T) {
	brokerRelease, templateDone := make(chan struct{}), make(chan struct{})
	var minted atomic.Bool
	out := &syncOut{}
	_, err := runWithProgressCtx(context.Background(), t, Deps{
		BrokerHealthy: func(context.Context) error {
			<-brokerRelease
			return nil
		},
		// The step before mint: once it ran, mint is about to wait for
		// the broker lane.
		EnsureAgentTemplate: func(context.Context, string) (bool, error) {
			close(templateDone)
			return false, nil
		},
		LoadImage: func(context.Context, string) error {
			<-templateDone
			time.Sleep(20 * time.Millisecond) // mint is waiting by now
			go func() {
				out.waitFor(testImage + " FAILED")
				close(brokerRelease)
			}()
			return errBoom
		},
		MintManagerBootstrap: func(context.Context) (BootstrapMaterial, error) {
			minted.Store(true)
			return BootstrapMaterial{Ticket: "t"}, nil
		},
	}, out)
	testutil.WantErrIs(t, err, errBoom)
	if minted.Load() {
		t.Fatal("mint ran after the image lane failed")
	}
}

func TestInterruptListsLaneStepsAsStopped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &syncOut{}
	_, err := runWithProgressCtx(ctx, t, Deps{
		LoadImage: func(lctx context.Context, _ string) error {
			<-lctx.Done()
			return lctx.Err()
		},
		EnsureHubIdentity: func(c context.Context) error {
			cancel()
			<-c.Done()
			return c.Err()
		},
	}, out)
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "load-image") {
		t.Fatalf("error: %v", err)
	}
	got := out.String()
	if strings.Contains(got, testImage+" FAILED") || !strings.Contains(got, "stopped: the run was interrupted") {
		t.Fatalf("progress list:\n%s", got)
	}
}

func TestForegroundFailureStopsTheLanes(t *testing.T) {
	var cancelled atomic.Bool
	out, err := runWithProgress(t, Deps{
		LoadImage: func(ctx context.Context, _ string) error {
			select {
			case <-ctx.Done():
				cancelled.Store(true)
				return ctx.Err()
			case <-time.After(overlapTimeout):
				return nil
			}
		},
		EnsureHubIdentity: func(context.Context) error { return errBoom },
	})
	testutil.WantErrIs(t, err, errBoom)
	if strings.Contains(err.Error(), "load-image") || errors.Is(err, context.Canceled) {
		t.Fatalf("a stopped lane leaked into the error: %v", err)
	}
	if !cancelled.Load() {
		t.Fatal("the image load was not cancelled")
	}
	if !strings.Contains(out, "load-image "+testImage+" skipped") || !strings.Contains(out, "stopped: an earlier step failed") {
		t.Fatalf("progress list:\n%s", out)
	}
	if strings.Contains(out, "load-image "+testImage+" FAILED") {
		t.Fatalf("a stopped lane step listed as failed:\n%s", out)
	}
}

func TestBothFailuresAreReported(t *testing.T) {
	identityStarted, brokerFailed := make(chan struct{}), make(chan struct{})
	errBroker := errors.New("broker did not become healthy")
	_, err := runWithProgress(t, Deps{
		BrokerHealthy: func(context.Context) error {
			defer close(brokerFailed)
			<-identityStarted
			return errBroker
		},
		EnsureHubIdentity: func(context.Context) error {
			close(identityStarted)
			<-brokerFailed
			return errBoom
		},
	})
	testutil.WantErrIs(t, err, errBoom)
	testutil.WantErrIs(t, err, errBroker)
	if !strings.Contains(err.Error(), "step broker-up") || !strings.Contains(err.Error(), "step hub-identity") {
		t.Fatalf("error: %v", err)
	}
}

func TestBrokerOnlyPlanMintsAfterTheBroker(t *testing.T) {
	app, f := newObserveFirstApp(t)
	var started, ok atomic.Bool
	d := fillDeps(Deps{
		Scion: hubScion(f, app),
		StartBroker: func(context.Context) error {
			time.Sleep(20 * time.Millisecond)
			started.Store(true)
			return nil
		},
		MintManagerBootstrap: func(context.Context) (BootstrapMaterial, error) {
			ok.Store(started.Load())
			return BootstrapMaterial{Ticket: "t"}, nil
		},
		// The broker-only VM has no scion to answer the role check.
		VerifyAgentRole: func(context.Context, string, string) error { return errors.New("no scion in the jail") },
	})
	if err := Run(context.Background(), app, d, PlanOpts{BrokerOnly: true}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !ok.Load() {
		t.Fatal("mint ran before the broker started")
	}
}

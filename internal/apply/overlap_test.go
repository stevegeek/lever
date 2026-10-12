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

// testImage is helloApp's manager image as the plan names it.
var testImage = helloApp("").ManagerImage()

// idleTicker never ticks; its stop closes the channel, as a real one does.
func idleTicker(time.Duration) (<-chan time.Time, func()) {
	ch := make(chan time.Time)
	var once sync.Once
	return ch, func() { once.Do(func() { close(ch) }) }
}

// runWithProgress runs the full plan with a plain progress list and returns
// the list's text.
func runWithProgress(t *testing.T, deps Deps) (string, error) {
	t.Helper()
	app, f := newObserveFirstApp(t)
	if deps.Scion == nil {
		deps.Scion = scion.New(&agentLifecycleRunner{FakeRunner: f, slug: "hello"}, scion.Options{})
	}
	var out bytes.Buffer
	rep := progress.New(progress.Options{Mode: progress.ModePlain, Out: &out, Ticker: idleTicker})
	err := Run(progress.With(context.Background(), rep), app, fillDeps(deps), PlanOpts{})
	rep.Finish(err)
	return out.String(), err
}

// TestLoadImageRunsAlongsideTheHubSteps (lever#189): the image load is
// still running when bootstrap-token (a later, independent step) runs.
func TestLoadImageRunsAlongsideTheHubSteps(t *testing.T) {
	patRan := make(chan struct{})
	var loadSawPAT atomic.Bool
	out, err := runWithProgress(t, Deps{
		LoadImage: func(ctx context.Context, _ string) error {
			select {
			case <-patRan:
				loadSawPAT.Store(true)
				return nil
			case <-time.After(overlapTimeout):
				return errors.New("bootstrap-token never ran while the image loaded")
			}
		},
		EnsureControllerPAT: func(context.Context) error { close(patRan); return nil },
	})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out)
	}
	if !loadSawPAT.Load() {
		t.Fatal("load-image did not overlap bootstrap-token")
	}
	if !strings.Contains(out, "load-image "+testImage+" ... (alongside the next steps)") || !strings.Contains(out, "load-image "+testImage+" ok") {
		t.Fatalf("progress list:\n%s", out)
	}
}

// TestStartManagerWaitsForTheImagesAndTheBroker: start-manager begins only
// once every image is loaded and warm and the broker is healthy.
func TestStartManagerWaitsForTheImagesAndTheBroker(t *testing.T) {
	var warmed, healthy, ok atomic.Bool
	_, err := runWithProgress(t, Deps{
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
		WaitBrokerReady: func(context.Context, string) error {
			ok.Store(warmed.Load() && healthy.Load())
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !ok.Load() {
		t.Fatal("start-manager started before the image warm-up or the broker health check finished")
	}
}

// TestMintWaitsForTheBroker: the enrol ticket is minted only from a healthy
// broker.
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

// TestBackgroundFailureStopsTheRunAfterTheRunningStep: a failed image load
// lets the foreground step that is running finish, starts no further step,
// and is the error Run returns.
func TestBackgroundFailureStopsTheRunAfterTheRunningStep(t *testing.T) {
	patStarted, loadFailed := make(chan struct{}), make(chan struct{})
	var patDone, serverRan atomic.Bool
	out, err := runWithProgress(t, Deps{
		LoadImage: func(context.Context, string) error {
			defer close(loadFailed)
			select {
			case <-patStarted:
			case <-time.After(overlapTimeout):
			}
			return errBoom
		},
		EnsureControllerPAT: func(context.Context) error {
			close(patStarted)
			select {
			case <-loadFailed:
			case <-time.After(overlapTimeout):
			}
			patDone.Store(true)
			return nil
		},
		EnsureScionTelemetry: func(context.Context) (bool, error) {
			serverRan.Store(true)
			return false, nil
		},
	})
	testutil.WantErrIs(t, err, errBoom)
	if !strings.Contains(err.Error(), "step load-image") {
		t.Fatalf("error does not name the failed step: %v", err)
	}
	if !patDone.Load() {
		t.Fatal("the running foreground step was not left to finish")
	}
	if serverRan.Load() {
		t.Fatal("a step after the background failure ran")
	}
	if !strings.Contains(out, "load-image "+testImage+" FAILED") || !strings.Contains(out, "bootstrap-token ok") {
		t.Fatalf("progress list:\n%s", out)
	}
}

// TestForegroundFailureStopsTheLanes: a failed foreground step cancels the
// image load still running; the load is listed as stopped, not failed, and
// only the real failure is returned.
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
		EnsureControllerPAT: func(context.Context) error { return errBoom },
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

// TestBothFailuresAreReported: a lane step and the foreground step that
// both fail on their own are both in the error.
func TestBothFailuresAreReported(t *testing.T) {
	patStarted, brokerFailed := make(chan struct{}), make(chan struct{})
	errBroker := errors.New("broker did not become healthy")
	_, err := runWithProgress(t, Deps{
		BrokerHealthy: func(context.Context) error {
			defer close(brokerFailed)
			<-patStarted
			return errBroker
		},
		EnsureControllerPAT: func(context.Context) error {
			close(patStarted)
			<-brokerFailed
			return errBoom
		},
	})
	testutil.WantErrIs(t, err, errBoom)
	testutil.WantErrIs(t, err, errBroker)
	if !strings.Contains(err.Error(), "step broker-up") || !strings.Contains(err.Error(), "step bootstrap-token") {
		t.Fatalf("error: %v", err)
	}
}

// TestBrokerOnlyPlanMintsAfterTheBroker: the acceptance plan (broker-up,
// mint) still mints only from a started broker.
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
	})
	if err := Run(context.Background(), app, d, PlanOpts{BrokerOnly: true}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !ok.Load() {
		t.Fatal("mint ran before the broker started")
	}
}

package scionbin

import (
	"context"
	"sync"

	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/progress"
)

// Prebuild is a Resolve started before the guest exists (lever#189): the
// host's scion build (a cold one took over three minutes) runs while the VM
// boots and the guest is provisioned, instead of after. It builds for an
// assumed guest architecture (the host's: Lima and OrbStack run the host's
// native architecture), and the scion step takes its result only once the
// guest has confirmed that architecture (Result); otherwise it builds again.
//
// A nil *Prebuild is valid: Result reports no result and Stop does nothing.
type Prebuild struct {
	spec            Spec
	goarch, machine string
	cancel          context.CancelFunc
	done            chan struct{}
	step            *progress.Step

	path string // valid after done
	err  error

	mu      sync.Mutex
	stopped bool
}

// StartPrebuild starts Resolve(spec, goarch, machine) in the background, or
// returns nil when there is nothing to build early: a prebuilt binary
// (scion.binary needs no build) or a spec Validate refuses (the scion step
// reports that, as before). The build is listed as its own background step.
// The caller must call Stop once the bring-up no longer needs it.
func StartPrebuild(ctx context.Context, r proc.Runner, spec Spec, goarch, machine string) *Prebuild {
	if spec.Binary != "" || spec.Validate() != nil {
		return nil
	}
	pctx, cancel := context.WithCancel(ctx)
	p := &Prebuild{spec: spec, goarch: goarch, machine: machine, cancel: cancel, done: make(chan struct{})}
	p.step = progress.From(ctx).Background("build scion on the host")
	go func() {
		defer close(p.done)
		p.path, p.err = Resolve(progress.WithStep(pctx, p.step), r, spec, goarch, machine)
		p.mu.Lock()
		stopped := p.stopped
		p.mu.Unlock()
		if p.err != nil && stopped {
			p.step.Skip("stopped: the bring-up ended before the scion step")
			return
		}
		p.step.Done(p.err)
	}()
	return p
}

// Result waits for the build and returns its binary and error when it was
// built from spec for goarch and machine. ok is false when p is nil or
// built something else; the caller then builds itself. Result waits even
// then, since a second build would write the same output path. A ctx that
// ends first returns its error with ok true.
func (p *Prebuild) Result(ctx context.Context, spec Spec, goarch, machine string) (path string, err error, ok bool) {
	if p == nil {
		return "", nil, false
	}
	select {
	case <-p.done:
	default:
		progress.From(ctx).Detail("waiting for the host build of scion")
		select {
		case <-p.done:
		case <-ctx.Done():
			return "", ctx.Err(), true
		}
	}
	if p.spec.Source != spec.Source || p.spec.Version != spec.Version || p.goarch != goarch || p.machine != machine {
		return "", nil, false
	}
	return p.path, p.err, true
}

// Stop cancels a build still running and waits for it to end.
func (p *Prebuild) Stop() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
	p.cancel()
	<-p.done
}

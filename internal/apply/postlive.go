package apply

import (
	"context"
	"fmt"
	"sync"
)

// postLive is start-manager's post-live work: the workers' session heal
// and the three warnings about the manager (its read-only mounts, its
// /dev/kvm, its claude settings). Each is one or two guest calls, a few
// seconds together, and none of them changes the manager; they used to run
// after the settle window and now run alongside it, from the moment the
// gate has seen claude (lever#188).
//
// Their lines are held back and logged once the gate has decided, in the
// order the probes ran before: a start that fails reports the failure, not
// warnings about a manager that is not up. The heal's lines are logged
// either way (it acts on workers, not on the manager); the warnings only
// when the manager came up.
type postLive struct {
	r   *run
	ctx context.Context
	jp  string

	once       sync.Once
	started    bool
	done       chan struct{}
	cancelWarn context.CancelFunc
	heal, warn lineBuffer
}

func (r *run) startPostLive(ctx context.Context, jp string) *postLive {
	return &postLive{r: r, ctx: ctx, jp: jp}
}

// start runs the probes in the background; later calls do nothing. The
// gate calls it as its settle window starts.
func (p *postLive) start() {
	p.once.Do(func() {
		p.started = true
		p.done = make(chan struct{})
		wctx, cancel := context.WithCancel(p.ctx)
		p.cancelWarn = cancel
		heal := p.r.d
		if heal.Log != nil {
			heal.Log = p.heal.logf
		}
		warn := *p.r
		warn.d.Log = p.warn.logf
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			healNamedSessions(p.ctx, heal, p.jp, workerNames(p.r.app))
		}()
		go func() {
			defer wg.Done()
			if !warn.managerCreated {
				warn.warnManagerTreeMounts(wctx, p.jp)
			}
			warn.warnManagerNestedVirt(wctx, p.jp)
			warn.warnManagerClaude(wctx, p.jp)
		}()
		go func() {
			wg.Wait()
			close(p.done)
		}()
	})
}

// wait waits for probes the gate started, without logging their lines;
// probes it never started stay unstarted. finish or abandon follows.
func (p *postLive) wait() {
	if p.started {
		<-p.done
	}
}

// finish runs the probes if the gate did not start them, waits for them,
// and logs every line they held back.
func (p *postLive) finish() {
	p.start()
	<-p.done
	p.cancelWarn()
	p.heal.flush(p.r.d.Log)
	p.warn.flush(p.r.d.Log)
}

// abandon is finish for a manager that did not come up: the warnings are
// cancelled and dropped, the heal is waited for and its lines logged.
// Probes the gate never started are not run.
func (p *postLive) abandon() {
	if !p.started {
		return
	}
	p.cancelWarn()
	<-p.done
	p.heal.flush(p.r.d.Log)
}

// lineBuffer holds log lines for later; safe for concurrent use.
type lineBuffer struct {
	mu    sync.Mutex
	lines []string
}

func (b *lineBuffer) logf(format string, args ...any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = append(b.lines, fmt.Sprintf(format, args...))
}

func (b *lineBuffer) flush(log func(string, ...any)) {
	b.mu.Lock()
	lines := b.lines
	b.lines = nil
	b.mu.Unlock()
	if log == nil {
		return
	}
	for _, l := range lines {
		log("%s", l)
	}
}

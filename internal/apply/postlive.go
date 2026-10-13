package apply

import (
	"context"
	"fmt"
	"sync"
)

// postLive is start-manager's post-live work (the workers' session heal and
// the warnings about the manager), run alongside the gate's settle window.
// None of it changes the manager.
//
// Their lines are held back until the gate has decided, so a start that fails
// reports the failure, not warnings about a manager that is not up. The
// heal's lines are logged either way (it acts on workers); the warnings only
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

// start runs the probes in the background; later calls do nothing.
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

// wait does not log the probes' lines or start probes; finish or abandon
// must follow.
func (p *postLive) wait() {
	if p.started {
		<-p.done
	}
}

func (p *postLive) finish() {
	p.start()
	<-p.done
	p.cancelWarn()
	p.heal.flush(p.r.d.Log)
	p.warn.flush(p.r.d.Log)
}

// abandon is finish for a manager that did not come up: the warnings are
// dropped and probes the gate never started are not run.
func (p *postLive) abandon() {
	if !p.started {
		return
	}
	p.cancelWarn()
	<-p.done
	p.heal.flush(p.r.d.Log)
}

// lineBuffer is safe for concurrent use.
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

package remoteproxy

import (
	"context"
	"sync"
	"time"
)

// revokePoll is how often an open request or stream re-asks
// Config.LoginCurrent: a revocation must also end an /events stream or a
// terminal websocket the login opened before it, which no new request would
// reach.
const revokePoll = time.Second

// liveRequests cancels every open request of a login once LoginCurrent stops
// admitting it. One poller runs while any request is open.
type liveRequests struct {
	mu      sync.Mutex
	open    map[string]map[*context.CancelFunc]struct{}
	polling bool
}

// track runs cancel when login is revoked while the request is open; the
// returned func ends the tracking.
func (g *gate) track(login string, cancel context.CancelFunc) func() {
	if g.cfg.LoginCurrent == nil {
		return func() {}
	}
	l := &g.live
	key := &cancel
	l.mu.Lock()
	if l.open == nil {
		l.open = map[string]map[*context.CancelFunc]struct{}{}
	}
	if l.open[login] == nil {
		l.open[login] = map[*context.CancelFunc]struct{}{}
	}
	l.open[login][key] = struct{}{}
	if !l.polling {
		l.polling = true
		go g.pollRevocations()
	}
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		delete(l.open[login], key)
		if len(l.open[login]) == 0 {
			delete(l.open, login)
		}
		l.mu.Unlock()
	}
}

func (g *gate) pollRevocations() {
	l := &g.live
	every := revokePoll
	if g.revokeEvery > 0 {
		every = g.revokeEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for range t.C {
		l.mu.Lock()
		if len(l.open) == 0 {
			l.polling = false
			l.mu.Unlock()
			return
		}
		logins := make([]string, 0, len(l.open))
		for login := range l.open {
			logins = append(logins, login)
		}
		l.mu.Unlock()
		for _, login := range logins {
			if g.loginCurrent(login) {
				continue
			}
			l.mu.Lock()
			for c := range l.open[login] {
				(*c)()
			}
			l.mu.Unlock()
		}
	}
}

package broker

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/sentledger"
	"github.com/stevegeek/lever/internal/servicehost"
)

// The service-host watch tells the manager when a service host it could not
// reach is back, so an agent that queued work for that host's tools (a
// laptop that sleeps) knows when to try again. The broker probes each host
// once a minute, and a failed tool call probes its host at once (a call can
// fail for the caller's own reasons, so the failure alone decides nothing). Only a
// host seen down and then up again is announced: the first probe after a
// broker start announces nothing, and a host that flaps is announced at most
// once per hostBackGap.

const (
	hostWatchEvery = time.Minute
	hostBackGap    = 10 * time.Minute
	hostProbeLimit = 15 * time.Second
)

// serviceHostMarker starts the notice; the ref after it is what
// message_verify looks the notice up by.
const serviceHostMarker = "[lever: service host back]"

// hostTarget is how the watch reaches one service host.
type hostTarget struct {
	name      string
	infoURL   string
	transport http.RoundTripper
	tools     []string
}

type hostState struct {
	known, up  bool
	lastNotice time.Time
}

type hostWatch struct {
	mu       sync.Mutex
	state    map[string]*hostState
	checking map[string]bool // a probe after a failed call is under way
}

func (w *hostWatch) get(host string) *hostState {
	if w.state == nil {
		w.state = map[string]*hostState{}
	}
	s, ok := w.state[host]
	if !ok {
		s = &hostState{}
		w.state[host] = s
	}
	return s
}

// markDown records that host could not be reached.
func (w *hostWatch) markDown(host string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.get(host)
	s.known, s.up = true, false
}

// markUp records that host answered and reports whether that is news worth
// telling the manager: the host was seen down before, and no notice went out
// in the last hostBackGap.
func (w *hostWatch) markUp(host string, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.get(host)
	back := s.known && !s.up
	s.known, s.up = true, true
	if !back || (!s.lastNotice.IsZero() && now.Sub(s.lastNotice) < hostBackGap) {
		return false
	}
	s.lastNotice = now
	return true
}

// unsent puts a notice that could not be sent back: the host counts as down
// again with no notice sent, so the next probe that finds it up retries.
func (w *hostWatch) unsent(host string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.get(host)
	s.up, s.lastNotice = false, time.Time{}
}

// startCheck claims the one probe-after-failure slot for host.
func (w *hostWatch) startCheck(host string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.checking == nil {
		w.checking = map[string]bool{}
	}
	if w.checking[host] {
		return false
	}
	w.checking[host] = true
	return true
}

func (w *hostWatch) endCheck(host string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.checking, host)
}

// checkHostAfterFailure probes host now, once at a time, after a gateway call
// to it failed, and marks it down only if the probe fails too: an agent can
// make its own call fail (a stalled body), and that must not stage a "back"
// notice. A short outage between two scheduled probes is still seen.
func (b *Broker) checkHostAfterFailure(host string) {
	if !b.hostWatchEnabled() || !b.hostWatch.startCheck(host) {
		return
	}
	go func() {
		defer b.hostWatch.endCheck(host)
		probe := b.hostProbe
		if probe == nil {
			probe = probeHost
		}
		for _, t := range b.serviceHostTargets() {
			if t.name == host && !probe(context.Background(), t) {
				b.hostWatch.markDown(host)
			}
		}
	}()
}

// serviceHostTargets groups the service-host tools by host.
func (b *Broker) serviceHostTargets() []hostTarget {
	byHost := map[string]*hostTarget{}
	for tool, route := range b.serviceHosts {
		t, ok := b.reg.Lookup(tool)
		if !ok {
			continue
		}
		u, err := url.Parse(t.Backend)
		if err != nil || u.Host == "" {
			continue
		}
		ht, ok := byHost[route.Host]
		if !ok {
			ht = &hostTarget{name: route.Host, infoURL: "https://" + u.Host + servicehost.InfoPath, transport: route.Transport}
			byHost[route.Host] = ht
		}
		ht.tools = append(ht.tools, tool)
	}
	out := make([]hostTarget, 0, len(byHost))
	for _, ht := range byHost {
		sort.Strings(ht.tools)
		out = append(out, *ht)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// probeHost asks the host for its info page over the gateway's pinned
// transport. Any 200 is up: the host answered with the broker's key.
func probeHost(ctx context.Context, t hostTarget) bool {
	ctx, cancel := context.WithTimeout(ctx, hostProbeLimit)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.infoURL, nil)
	if err != nil {
		return false
	}
	c := &http.Client{Transport: t.transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (b *Broker) hostWatchEnabled() bool {
	return len(b.serviceHosts) > 0 && b.runtime != nil && b.manager != ""
}

// runHostWatch probes every service host once at once, then once per
// interval, until ctx ends.
func (b *Broker) runHostWatch(ctx context.Context) {
	every := b.hostWatchEvery
	if every <= 0 {
		every = hostWatchEvery
	}
	b.watchHostsOnce(ctx)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.watchHostsOnce(ctx)
		}
	}
}

func (b *Broker) watchHostsOnce(ctx context.Context) {
	probe := b.hostProbe
	if probe == nil {
		probe = probeHost
	}
	for _, t := range b.serviceHostTargets() {
		if ctx.Err() != nil {
			return
		}
		if !probe(ctx, t) {
			b.hostWatch.markDown(t.name)
			continue
		}
		if b.hostWatch.markUp(t.name, time.Now()) {
			b.announceHostBack(ctx, t)
		}
	}
}

// announceHostBack sends the manager the notice. A manager that is not
// running gets nothing: it lists the host's tools afresh when it starts.
func (b *Broker) announceHostBack(ctx context.Context, t hostTarget) {
	cn, slug, _, ok := b.identity(b.manager)
	if !ok {
		return
	}
	if refusal, _, _ := b.notRunningCheck(ctx, "lever", slug, true); refusal != "" {
		b.audit("msg", cn, "deny", "service host "+t.name+" back: not sent: "+refusal)
		return
	}
	text := fmt.Sprintf("Service host %s is reachable again. Its tools: %s. Retry any work you held back for them.",
		t.name, strings.Join(t.tools, ", "))
	ref, err := b.sendRecorded(ctx, cn, slug, sentledger.KindServiceHostNotice,
		func(ref string) string { return markedBody(serviceHostMarker, ref, text) },
		scion.MsgOpts{To: "agent:" + slug, Project: b.instanceProject})
	if err != nil {
		b.hostWatch.unsent(t.name)
		b.audit("msg", cn, "error", "service host "+t.name+" back: "+err.Error()+" (retried at the next check)", "ref", ref)
		return
	}
	b.audit("msg", cn, "allow", "service host "+t.name+" back", "ref", ref)
}

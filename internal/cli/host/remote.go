package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/backend/common"
	"github.com/stevegeek/lever/internal/backend/registry"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/chatledger"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/httpjson"
	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/hubauth"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/remoteproxy"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/voice"
	"github.com/stevegeek/lever/internal/webpush"
	"github.com/stevegeek/lever/internal/wire"
)

func newRemoteCmd(bf BackendFactory) *cobra.Command {
	c := &cobra.Command{Use: "remote", Short: "Run / inspect the remote-access proxy (behind tailscale serve or another authenticating front)"}
	c.AddCommand(newRemoteServeCmd(bf), newRemoteStatusCmd())
	return c
}

func newRemoteServeCmd(bf BackendFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "serve [CONFIG]",
		Args:  cobra.MaximumNArgs(1),
		Short: "Run the remote-access proxy (foreground)",
		RunE: func(cmd *cobra.Command, args []string) error {
			daemonLogging(cmd, "remote-proxy")
			path, app, err := loadRemoteApp(args)
			if err != nil {
				return err
			}
			st := stateFor(path)
			// The proxy is what lets a contact in: the same gate as apply.
			if err := checkContactGate(app, st); err != nil {
				return err
			}
			printContactSessionWarnings(cmd, app, st)
			auditFn, auditCloser, err := remoteproxy.OpenAudit(st.RemoteAudit())
			if err != nil {
				return err
			}
			defer auditCloser.Close()

			// One jail route for proxied traffic, the login and the clock read: the login is
			// hub traffic and must reach this instance's jail.
			prefix := jailPrefixFn(bf, app.Backend, machineName(app.Name), cmd.ErrOrStderr())
			dial := remoteproxy.JailDial(prefix)
			hubLogin, err := loadRemoteHubLogin(app, st, func() hubauth.Clock {
				return guestClock(cmd.Context(), prefix, proc.RealRunner{}, time.Now, cmd.ErrOrStderr())
			})
			if err != nil {
				return err
			}
			handler, push, err := buildRemoteHandler(app, st, dial, hubLogin, auditFn, cmd.ErrOrStderr())
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			// The push watchers share the proxy's context, and the serve waits for every
			// hub stream to close.
			pushDone := make(chan struct{})
			go func() {
				defer close(pushDone)
				if push != nil {
					push.Run(ctx)
				}
			}()
			printRemoteWarnings(cmd, app)
			cmd.Printf("remote proxy %q serving on %s, identity header %s\n",
				app.Name, app.RemoteListenAddr(), app.EffectiveRemoteIdentityHeader())
			if app.VoiceOn() {
				cmd.Printf("dictation on: through lever-tool-whisper's socket %s\n", app.Remote.Voice.Socket)
				if app.EffectiveReadAloud() == config.ReadAloudServer {
					cmd.Printf("read-aloud with server voices: through lever-tool-tts's socket %s (the device's voices when it is down)\n", app.Remote.Voice.SpeakSocket)
				}
			}
			err = serveRemote(ctx, app, st, handler)
			stop()
			<-pushDone
			return err
		},
	}
}

// loadRemoteApp loads the config for `remote serve` and applies the two gates
// every proxy start must pass.
func loadRemoteApp(args []string) (string, *config.App, error) {
	path, err := resolveConfigPath(argOrEmpty(args))
	if err != nil {
		return "", nil, err
	}
	app, err := config.Load(path)
	if err != nil {
		return "", nil, err
	}
	if !app.RemoteEnabled() {
		return "", nil, errRemoteDisabled
	}
	return path, app, nil
}

// remoteHubLogin is what the login driver needs to turn a verified login into
// a hub session. A zero value refuses every login.
type remoteHubLogin struct {
	sign      func(email string) (string, error)
	hubUserID func(email string) (string, bool)
}

// loadRemoteHubLogin reads the assertion key, the instance id and the
// recorded hub users once: apply restarts the proxy after changing any of
// them. clock is read only once those are in place.
func loadRemoteHubLogin(app *config.App, st state.State, clock func() hubauth.Clock) (remoteHubLogin, error) {
	key, err := hubauth.LoadKey(st.HubAuthKey())
	if err != nil {
		return remoteHubLogin{}, fmt.Errorf("remote proxy: no usable hub assertion key (%v); run `lever apply`", err)
	}
	id, err := st.ReadInstanceID()
	if err != nil {
		return remoteHubLogin{}, fmt.Errorf("remote proxy: no instance id (%v); run `lever apply`", err)
	}
	rec, found, err := st.LoadHubIdentity()
	if err != nil {
		return remoteHubLogin{}, fmt.Errorf("remote proxy: %v; run `lever apply`", err)
	}
	if !found {
		return remoteHubLogin{}, fmt.Errorf("remote proxy: no hub identity record (%s); run `lever apply`", st.HubIdentityFile())
	}
	emails := remoteproxy.HubUserEmails(app.Remote.Logins())
	for i, e := range emails {
		emails[i] = strings.ToLower(e)
	}
	signer := hubauth.NewRemoteSigner(hubauth.NewSigner(key, id, clock()), emails)
	users := make(map[string]string, len(rec.Users))
	for email, uid := range rec.Users {
		users[strings.ToLower(email)] = uid
	}
	return remoteHubLogin{
		sign: signer.Sign,
		hubUserID: func(email string) (string, bool) {
			uid, ok := users[strings.ToLower(email)]
			return uid, ok && uid != ""
		},
	}, nil
}

// guestClockTimeout bounds the startup clock read, so a wedged jail delays
// the proxy by at most this.
const guestClockTimeout = 15 * time.Second

// guestClock reads the guest's clock once. The hub checks an assertion's iat
// and exp against its own clock; when the read fails the proxy signs on the
// host clock, which the assertion's backdate and lifetime absorb for a small
// skew.
func guestClock(ctx context.Context, prefixFn func() []string, run proc.Runner, now func() time.Time, warn io.Writer) hubauth.Clock {
	fallback := func(why string) hubauth.Clock {
		fmt.Fprintf(warn, "lever: warning: remote proxy could not read the guest clock (%s): signing hub assertions on the host clock\n", why)
		return hubauth.Clock{}
	}
	prefix := prefixFn()
	if len(prefix) == 0 {
		return fallback("the jail is not reachable")
	}
	ctx, cancel := context.WithTimeout(ctx, guestClockTimeout)
	defer cancel()
	ctx = proc.WithOutputLimit(ctx, 64)
	before := now()
	res, err := run.Run(ctx, nil, prefix[0], append(slices.Clone(prefix[1:]), "date", "-u", "+%s")...)
	after := now()
	if err != nil {
		return fallback(firstLine(err.Error()))
	}
	epoch, err := strconv.ParseInt(strings.TrimSpace(res.Stdout), 10, 64)
	if err != nil {
		return fallback("unexpected `date` output")
	}
	return hubauth.ClockFromGuestEpoch(epoch, before.Add(after.Sub(before)/2))
}

func buildRemoteHandler(app *config.App, st state.State, dial func(ctx context.Context, network, addr string) (net.Conn, error), hubLogin remoteHubLogin, auditFn func(remoteproxy.AuditLine), warn io.Writer) (http.Handler, *remoteproxy.Push, error) {
	// The hub's address inside the guest, where the dialer lands, so also the
	// right Host header. It is deliberately not host-reachable (see
	// scion.DefaultHubEndpoint).
	target, err := url.Parse(scion.DefaultHubEndpoint)
	if err != nil {
		return nil, nil, err
	}
	push, err := remotePush(app, st, auditFn, warn)
	if err != nil {
		return nil, nil, err
	}

	login := remoteproxy.NewLoginDriver(remoteproxy.LoginConfig{
		Hub:         target,
		DialContext: dial,
		Sign:        hubLogin.sign,
		HubUserID:   hubLogin.hubUserID,
		Audit:       auditFn,
	})

	records, resolve := remoteHubAgents(st, target, dial)
	handler := remoteproxy.NewHandler(remoteproxy.Config{
		Target:      target,
		DialContext: dial,
		ServeHost:   remoteServeHost(app.Remote.BaseURL),
		// Lets the Host gate admit `lever doctor`'s loopback /healthz probe on this
		// one port.
		ListenPort:         app.EffectiveRemotePort(),
		AllowedUsers:       app.Remote.Logins(),
		IdentityHeader:     app.EffectiveRemoteIdentityHeader(),
		TrustForwardedHost: app.Remote.TrustForwardedHost,
		BindHost:           remoteBindHost(app),
		Session:            login,
		Audit:              auditFn,
		// Verified web chat needs allowed_users: without it no login is verified.
		ChatLedger: remoteChatLedger(app, st),
		// Contacts chat only with their agents (remoteproxy/contact.go). Agent names
		// resolve to hub ids with the remote PAT, for GET agent lists only.
		Contacts:      remoteContacts(app),
		ResolveAgents: resolve,
		// A contact's post reaches an agent only while its session started fresh with
		// the current skill on disk (contactSession).
		ContactSession:     func(agent string) error { return contactSession(app, st, agent) },
		ChatAgent:          remoteChatAgent(app),
		Workers:            remoteWorkers(app),
		ContactSee:         remoteContactSee(app),
		AgentRecords:       records,
		Labels:             remoteLabels(app),
		Wake:               remoteWake(app, st),
		MatchAgentMessages: remoteAgentMessages(app, st),
		// The operator view of contact conversations reads only for a contact apply
		// bound to a hub user (opview.go).
		ContactUser:       remoteContactUser(st),
		PeekAgentMessages: remoteAgentMessagesPeek(app, st),
		// Named as doctor names it, so the denial text stays byte-identical.
		LogPath: stateRel(st, st.RemoteLog()),
		Push:    push,
		Files:   remoteFiles(app, st),
		Voice:   remoteVoiceConfig(app),
	})
	return handler, push, nil
}

// remoteVoiceConfig sends each checked clip to lever-tool-whisper's socket and,
// with read_aloud: server, each spoken part to lever-tool-tts's socket. Nil
// when off.
func remoteVoiceConfig(app *config.App) *remoteproxy.VoiceConfig {
	if !app.VoiceOn() {
		return nil
	}
	c := &voice.DictateClient{Socket: app.Remote.Voice.Socket}
	vc := &remoteproxy.VoiceConfig{MaxSeconds: app.EffectiveVoiceMaxSeconds(), Excluded: app.VoiceExcludedLogins(),
		ReadAloud: app.EffectiveReadAloud() != config.ReadAloudOff,
		NotSent:   func(err error) bool { return errors.Is(err, voice.ErrNotSent) },
		Available: c.Available, Transcribe: c.Transcribe}
	if app.EffectiveReadAloud() == config.ReadAloudServer {
		s := &voice.SpeakClient{Socket: app.Remote.Voice.SpeakSocket}
		vc.Speech = &remoteproxy.SpeechConfig{Available: s.Available, Speak: s.Speak,
			NotSent: func(err error) bool { return errors.Is(err, voice.ErrSpeakNotSent) }}
	}
	return vc
}

// pushTestHosts is a test-only exception: remote.push.test_hosts and
// LEVER_PUSH_TEST_HOSTS must name the same 127.0.0.1 addresses. Either alone is
// an error, so neither a stray shell variable nor a copied config key widens
// where the proxy may connect.
func pushTestHosts(app *config.App, env string) (webpush.TestHosts, error) {
	fromEnv, err := webpush.ParseTestHosts(env)
	if err != nil {
		return nil, err
	}
	fromCfg, err := webpush.ParseTestHosts(strings.Join(app.Remote.Push.TestHosts, ","))
	if err != nil {
		return nil, err
	}
	if !maps.Equal(fromEnv, fromCfg) {
		return nil, fmt.Errorf("remote.push: the test push hosts need both remote.push.test_hosts and %s, naming the same "+
			"127.0.0.1:<port> addresses (config %q, environment %q); TEST ONLY — for a real instance set neither",
			webpush.TestHostsEnv, strings.Join(app.Remote.Push.TestHosts, ","), env)
	}
	return fromEnv, nil
}

// remotePush builds the Web Push service, or nil when off. A malformed
// LEVER_PUSH_TEST_HOSTS stops the serve; a fault in the push files keeps the
// proxy serving with push off, and says so.
func remotePush(app *config.App, st state.State, auditFn func(remoteproxy.AuditLine), warn io.Writer) (*remoteproxy.Push, error) {
	if !app.PushOn() {
		return nil, nil
	}
	if brokerctl.StateInsideTree(app, st) {
		fmt.Fprintf(warn, "lever: warning: remote.push is on, but the state directory is inside the tree, where agents could read "+
			"the push key and subscriptions: push stays off\n")
		return nil, nil
	}
	test, err := pushTestHosts(app, os.Getenv(webpush.TestHostsEnv))
	if err != nil {
		return nil, err
	}
	if len(test) > 0 {
		fmt.Fprintf(warn, "lever: warning: %s=%s: TEST ONLY — this proxy may push over plain http to those loopback addresses\n",
			webpush.TestHostsEnv, os.Getenv(webpush.TestHostsEnv))
	}
	p, err := remoteproxy.NewPush(remoteproxy.PushOptions{Dir: st.PushDir(), Subject: app.Remote.Push.Subject,
		TestHosts: test, Logins: app.Remote.Logins(), Audit: auditFn})
	if err != nil {
		fix := ""
		if errors.Is(err, webpush.ErrBadKey) || errors.Is(err, remoteproxy.ErrBadPushStore) {
			fix = "; remove that file and restart the proxy"
		}
		fmt.Fprintf(warn, "lever: warning: remote.push: %v: push stays off%s (see `lever doctor`)\n", err, fix)
		return nil, nil
	}
	return p, nil
}

// remoteChatAgent is the manager, whose agent name is the instance name; empty
// when the chat page is off.
func remoteChatAgent(app *config.App) string {
	if !app.RemoteLandingChat() {
		return ""
	}
	return app.Name
}

func remoteContacts(app *config.App) map[string][]string {
	out := map[string][]string{}
	for _, u := range app.Remote.AllowedUsers {
		if u.EffectiveTier() == config.TierContact {
			out[u.Login] = u.Agents
		}
	}
	return out
}

func remoteContactSee(app *config.App) map[string][]string {
	out := map[string][]string{}
	for _, u := range app.Remote.AllowedUsers {
		if u.EffectiveTier() == config.TierContact && len(u.See) > 0 {
			out[u.Login] = u.See
		}
	}
	return out
}

func remoteWorkers(app *config.App) []string {
	out := make([]string, len(app.Workers))
	for i, w := range app.Workers {
		out[i] = w.Name
	}
	return out
}

// remoteFiles is remote.files, or nil when off. The ledger stays off when the
// state directory is inside the tree, where an agent could write a record.
func remoteFiles(app *config.App, st state.State) *remoteproxy.FilesConfig {
	if !app.FilesOn() {
		return nil
	}
	c := &remoteproxy.FilesConfig{Tree: app.Tree, Workspaces: app.AgentWorkspaces(),
		MaxBytes: app.EffectiveFilesMaxBytes(), Extensions: app.EffectiveFilesExtensions(),
		NoUploads: !app.FilesUploadsOn(), NoShares: !app.FilesSharesOn(), Excluded: app.FilesExcludedLogins()}
	if !brokerctl.StateInsideTree(app, st) {
		c.LedgerDir = st.FilesLedger()
	}
	return c
}

func remoteLabels(app *config.App) func() map[string]string {
	if app.Remote.LabelsFile == "" {
		return nil
	}
	return (&remoteproxy.LabelSource{Tree: app.Tree, Rel: app.Remote.LabelsFile}).Labels
}

// remoteHubAgents lists the project's agent records with the remote PAT (GET
// only, through HubDoer). The chat page and the contact fence read the same
// cached list.
func remoteHubAgents(st state.State, target *url.URL, dial func(ctx context.Context, network, addr string) (net.Conn, error)) (
	func(context.Context) (map[string]remoteproxy.AgentRecord, error), func(context.Context) (map[string]string, error)) {
	hc := &hubapi.Client{T: &remoteproxy.HubDoer{Target: target, DialContext: dial, Token: func() (string, error) {
		tok, err := st.LoadRemotePAT()
		if err == nil && tok == "" {
			err = errors.New("no remote PAT on disk; run `lever apply`")
		}
		return tok, err
	}}}
	return cachedAgentRecords(func(ctx context.Context) ([]hubapi.Agent, error) {
		return hc.Agents(ctx, filepath.Base(common.MountDest), scion.DefaultHubEndpoint)
	}, time.Now)
}

// agentRecordsTTL bounds how stale the chat page's states may be; the page
// polls every few seconds during a wake.
const agentRecordsTTL = 3 * time.Second

// agentRecordsRefresh bounds one hub list, which runs on its own context.
const agentRecordsRefresh = 10 * time.Second

// errAgentListPanicked is not cached, so the next caller refreshes again.
var errAgentListPanicked = errors.New("the hub agent list failed unexpectedly")

type agentFlight struct {
	done chan struct{}
	recs map[string]remoteproxy.AgentRecord
	err  error
}

// cachedAgentRecords wraps the hub agent list in a short cache keyed by slug
// and derives the name-to-id resolver from it. Every login and the contact
// fence share it, so no caller may spoil it for the others:
//
//   - one refresh at a time, on its own context, so a caller that gives up
//     does not cancel the list others wait on;
//   - a caller waits only as long as its own context allows, and no lock is
//     held across the hub call;
//   - a hub error is cached for the TTL, but never a context-class error,
//     which says nothing about the hub.
func cachedAgentRecords(list func(context.Context) ([]hubapi.Agent, error), now func() time.Time) (
	func(context.Context) (map[string]remoteproxy.AgentRecord, error), func(context.Context) (map[string]string, error)) {
	var (
		mu     sync.Mutex
		at     time.Time
		recs   map[string]remoteproxy.AgentRecord
		lerr   error
		flight *agentFlight
	)
	refresh := func(f *agentFlight, ctx context.Context) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentRecordsRefresh)
		defer cancel()
		agents, err := func() (agents []hubapi.Agent, err error) {
			// A panic here would end the proxy and leave the flight open for good.
			defer func() {
				if recover() != nil {
					agents, err = nil, errAgentListPanicked
				}
			}()
			return list(ctx)
		}()
		if err == nil {
			f.recs = map[string]remoteproxy.AgentRecord{}
			for _, a := range agents {
				if a.Slug != "" && a.ID != "" {
					f.recs[a.Slug] = agentRecordOf(a)
				}
			}
		}
		f.err = err
		mu.Lock()
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, errAgentListPanicked) {
			recs, lerr, at = f.recs, err, now()
		}
		flight = nil
		mu.Unlock()
		close(f.done)
	}
	records := func(ctx context.Context) (map[string]remoteproxy.AgentRecord, error) {
		mu.Lock()
		if !at.IsZero() && now().Sub(at) < agentRecordsTTL {
			r, e := recs, lerr
			mu.Unlock()
			if e != nil {
				return nil, e
			}
			return maps.Clone(r), nil
		}
		f := flight
		if f == nil {
			f = &agentFlight{done: make(chan struct{})}
			flight = f
			go refresh(f, ctx)
		}
		mu.Unlock()
		select {
		case <-f.done:
			if f.err != nil {
				return nil, f.err
			}
			return maps.Clone(f.recs), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	resolve := func(ctx context.Context) (map[string]string, error) {
		recs, err := records(ctx)
		if err != nil {
			return nil, err
		}
		out := make(map[string]string, len(recs))
		for name, r := range recs {
			out[name] = r.ID
		}
		return out, nil
	}
	return records, resolve
}

// agentRecordOf reduces a hub record to what the chat page may show. Phase,
// activity and container status are agent-controlled text, so only scion's
// known words pass (scion.PhaseLabel, ActivityLabel).
func agentRecordOf(a hubapi.Agent) remoteproxy.AgentRecord {
	phase := scion.PhaseLabel(a.Phase)
	return remoteproxy.AgentRecord{ID: a.ID, Phase: phase, Activity: scion.ActivityLabel(a.Activity),
		ContainerDown: scion.RunningContainerDown(phase, a.ContainerStatus)}
}

// remoteWake POSTs /operator/wake on the broker's 0600 operator socket, the
// host user's own channel. Nil when the state directory is inside the tree:
// the broker then binds no operator socket.
func remoteWake(app *config.App, st state.State) func(ctx context.Context, login, tier, worker string) error {
	if brokerctl.StateInsideTree(app, st) {
		return nil
	}
	client := udsClient(st.OperatorSock())
	return func(ctx context.Context, login, tier, worker string) error {
		err := httpjson.Post(ctx, client, udsURL+wire.PathOperatorWake, wire.OperatorWakeRequest{Worker: worker, Login: login, Tier: tier}, nil)
		if err != nil {
			return &remoteproxy.WakeError{Status: httpjson.Status(err), Err: err}
		}
		return nil
	}
}

// agentMessagesTimeout bounds one match question; the proxy holds the
// contact's history answer meanwhile.
const agentMessagesTimeout = 10 * time.Second

// remoteAgentMessages asks the broker which agent rows of a contact's DM its
// agent ledger recorded. Nil when off. With the state directory inside the
// tree there is no operator socket, so every call fails and the proxy hides
// every agent row. Any answer but 200 is an error.
func remoteAgentMessages(app *config.App, st state.State) func(ctx context.Context, contact, agent string, msgs []remoteproxy.AgentMessage) (map[string]bool, error) {
	ask := remoteAgentMatcher(app, st, false)
	if ask == nil {
		return nil
	}
	return func(ctx context.Context, contact, agent string, msgs []remoteproxy.AgentMessage) (map[string]bool, error) {
		keep, _, err := ask(ctx, contact, agent, msgs)
		return keep, err
	}
}

// remoteAgentMessagesPeek is the operator view's question, which binds and
// writes nothing. It also answers which kept rows are pending.
func remoteAgentMessagesPeek(app *config.App, st state.State) func(ctx context.Context, contact, agent string, msgs []remoteproxy.AgentMessage) (keep, pending map[string]bool, err error) {
	return remoteAgentMatcher(app, st, true)
}

func remoteAgentMatcher(app *config.App, st state.State, peek bool) func(ctx context.Context, contact, agent string, msgs []remoteproxy.AgentMessage) (keep, pending map[string]bool, err error) {
	if !app.AgentMessagesOn() {
		return nil
	}
	if brokerctl.StateInsideTree(app, st) {
		return func(context.Context, string, string, []remoteproxy.AgentMessage) (map[string]bool, map[string]bool, error) {
			return nil, nil, errors.New("no operator socket: the state directory is inside the tree")
		}
	}
	client := udsClient(st.OperatorSock())
	return func(ctx context.Context, contact, agent string, msgs []remoteproxy.AgentMessage) (map[string]bool, map[string]bool, error) {
		ctx, cancel := context.WithTimeout(ctx, agentMessagesTimeout)
		defer cancel()
		req := wire.AgentMessagesMatchRequest{Contact: contact, Agent: agent, Peek: peek, Messages: make([]wire.AgentMessageRef, len(msgs))}
		for i, m := range msgs {
			req.Messages[i] = wire.AgentMessageRef{ID: m.ID, SHA256: m.SHA256, CreatedAt: m.CreatedAt}
		}
		var out wire.AgentMessagesMatchResponse
		if err := httpjson.Post(ctx, client, udsURL+wire.PathOperatorAgentMessagesMatch, req, &out); err != nil {
			return nil, nil, err
		}
		// Only ids this question named: an id the broker adds is ignored.
		asked := make(map[string]bool, len(msgs))
		for _, m := range msgs {
			asked[m.ID] = true
		}
		keep := make(map[string]bool, len(out.Keep))
		for _, id := range out.Keep {
			if asked[id] {
				keep[id] = true
			}
		}
		pending := map[string]bool{}
		for _, id := range out.Pending {
			if peek && keep[id] {
				pending[id] = true
			}
		}
		return keep, pending, nil
	}
}

// remoteChatLedger is nil when verified chat is off; brokerctl.ChatLedgerPath
// makes the same decision for the broker.
func remoteChatLedger(app *config.App, st state.State) func(chatledger.Entry) error {
	p := brokerctl.ChatLedgerPath(app, st)
	if p == "" {
		return nil
	}
	removeOldChatLedger(st)
	return chatledger.NewWriter(p).Append
}

// removeOldChatLedger deletes the legacy single-file ledger and its rotated
// copy, which only hold old chat text.
func removeOldChatLedger(st state.State) {
	for _, name := range []string{"chat-ledger.jsonl", "chat-ledger.jsonl.1"} {
		p := filepath.Join(st.Dir, name)
		if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() {
			_ = os.Remove(p)
		}
	}
}

// serveRemote stamps the config this process actually loaded. apply reuses a
// live proxy only when the stamp matches, and a proxy can be started by hand,
// so stamping here keeps a hand-started proxy from inheriting the record of
// an apply-started one. See ServeConfig.Stamp.
func serveRemote(ctx context.Context, app *config.App, st state.State, handler http.Handler) error {
	return remoteproxy.Serve(ctx, remoteproxy.ServeConfig{
		Port: app.EffectiveRemotePort(),
		Bind: app.EffectiveRemoteBind(),
		// config.validateRemoteBind already refused every address the jail could reach
		// and an unacknowledged wildcard.
		AllowNonLoopback: !app.RemoteBindLoopback(),
		Handler:          handler,
		PIDPath:          st.RemotePID(),
		Stamp: func() error {
			return st.WriteRemoteStamp(cli.VersionString(), brokerctl.RemoteConfigHash(app, st))
		},
	})
}

// jailResolveTimeout bounds one read of the jail's run user: a wedged machine
// must fail the dial rather than hold a request open.
const jailResolveTimeout = 15 * time.Second

// jailPrefixFn resolves the argv prefix into this instance's jail for
// remoteproxy.JailDial; on OrbStack it embeds the machine's run user.
//
// Resolution happens on the first dial, not at startup, so a stopped jail does
// not make a dead proxy. A failed resolve returns nil (JailDial renders an
// actionable error) and the next request retries.
//
// Success is cached: resolving costs three commands into the machine, and the
// run user cannot change under a running machine. A rebuilt jail with another
// run user needs `lever stop` + `lever up`: apply's reuse stamp
// (brokerctl.RemoteConfigHash) does not change on a jail rebuild.
//
// Concurrent dials share one attempt, so a wedged machine does not hold the
// Nth caller for N timeouts. warn gets resolve failures deduplicated: the
// proxy's stderr is remote.log.
func jailPrefixFn(bf BackendFactory, backendName, machine string, warn io.Writer) func() []string {
	var (
		mu       sync.Mutex
		cached   []string
		inflight chan struct{} // non-nil while one attempt is running
		lastWarn string
	)
	report := func(err error) { // call with mu held
		if warn == nil || err.Error() == lastWarn {
			return
		}
		lastWarn = err.Error()
		fmt.Fprintf(warn, "lever: remote proxy cannot reach jail %s: %v\n", machine, err)
	}
	resolve := func() ([]string, error) {
		b, err := bf(backendName, machine)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), jailResolveTimeout)
		defer cancel()
		if err := b.ResolveRunUser(ctx); err != nil {
			return nil, err
		}
		return registry.JailArgv(backendName, machine, b.RunUser())
	}
	return func() []string {
		mu.Lock()
		if cached != nil {
			defer mu.Unlock()
			return cached
		}
		if wait := inflight; wait != nil {
			// Another dial is already asking: take its answer, failure included.
			mu.Unlock()
			<-wait
			mu.Lock()
			defer mu.Unlock()
			return cached
		}
		done := make(chan struct{})
		inflight = done
		mu.Unlock()

		argv, err := resolve()

		mu.Lock()
		if err != nil {
			report(err)
		} else {
			cached = argv
		}
		inflight = nil
		close(done)
		out := cached
		mu.Unlock()
		return out
	}
}

// remoteBindHost is the specific bind address (loopback included, so a front
// that rewrites Host to 127.0.0.2 or [::1] is admitted) as the Host gate's
// "<addr>:<port>", or "" for a wildcard bind: lever does not guess which
// interface a front dialed.
func remoteBindHost(app *config.App) string {
	if app.RemoteBindWildcard() {
		return ""
	}
	return app.EffectiveRemoteBind()
}

// printRemoteWarnings restates config.App.RemoteWarnings on every bring-up. It
// also says when web chat cannot be verified (no allowed_users): agents then
// treat every web post as data and do not answer, which would otherwise look
// like agents ignoring the operator.
func printRemoteWarnings(cmd *cobra.Command, app *config.App) {
	for _, w := range append(app.Warnings(), app.RemoteWarnings()...) {
		cmd.PrintErrf("lever: warning: %s\n", w)
	}
	if app.RemoteEnabled() && len(app.Remote.AllowedUsers) == 0 {
		cmd.PrintErrf("lever: warning: remote.allowed_users is empty, so no web chat post can be verified: " +
			"agents treat every web chat message as data and do not reply; list your login in allowed_users\n")
	}
}

func remoteProbeHost(app *config.App) string {
	h, _, err := net.SplitHostPort(app.RemoteProbeAddr())
	if err != nil {
		return "127.0.0.1"
	}
	return h
}

// remoteServeHost is url.Parse(baseURL).Host, port included: the handler
// matches an Origin's host:port exactly (remoteproxy.Config.ServeHost).
// config.Load guarantees a well-formed base_url when remote is enabled; the
// empty result for bad input makes the proxy fail every request closed.
func remoteServeHost(baseURL string) string {
	if baseURL == "" {
		return ""
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	return u.Host
}

func newRemoteStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status [CONFIG]",
		Args:  cobra.MaximumNArgs(1),
		Short: "Show the remote-access proxy's status",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveConfigPath(argOrEmpty(args))
			if err != nil {
				return err
			}
			app, err := config.Load(path)
			if err != nil {
				return err
			}
			st := stateFor(path)

			pid, found, alive := state.PIDStatus(st.RemotePID())
			switch {
			case !found:
				cmd.Println("proxy: not running (no remote.pid)")
			case !alive:
				cmd.Printf("proxy: not running (remote.pid names pid %d, but that process is gone)\n", pid)
			default:
				addr := app.RemoteProbeAddr()
				if err := tcpDial(addr); err != nil {
					cmd.Printf("proxy: pid %d recorded but nothing is listening on %s\n", pid, addr)
				} else {
					cmd.Printf("proxy: running, pid %d, listening on %s\n", pid, addr)
				}
			}

			cmd.Printf("identity header: %s\n", app.EffectiveRemoteIdentityHeader())
			if app.RemoteBindLoopback() {
				cmd.Printf("tailscale command: tailscale serve --bg --https=443 http://%s\n", app.RemoteListenAddr())
				if app.EffectiveRemoteBind() != config.DefaultRemoteBind {
					// Checked against tailscale 1.102.4: any IP with an explicit scheme is
					// accepted. Older releases refuse every target but localhost/127.0.0.1.
					cmd.Println("  note: this target is not 127.0.0.1; older tailscale releases refuse it (\"only localhost or " +
						"127.0.0.1 proxies are currently supported\") — use a current tailscale, or the default bind")
				}
			}
			for _, w := range app.RemoteWarnings() {
				cmd.Printf("lever: warning: %s\n", w)
			}

			if app.Remote.BaseURL != "" {
				cmd.Printf("serve URL: %s\n", app.Remote.BaseURL)
			} else {
				// Reachable only with remote disabled: an enabled block without base_url is
				// rejected at config load.
				cmd.Println("base_url not set — remote access needs both `remote.enabled: true` and `remote.base_url` (the front's public https origin) set in lever.yaml; set both, then `lever apply`")
			}

			if _, err := os.Stat(st.RemotePAT()); err == nil {
				cmd.Println("remote PAT: present")
			} else {
				cmd.Println("remote PAT: absent — run `lever apply` to mint it")
			}
			return nil
		},
	}
}

// remoteContactUser returns the hub user id apply bound a contact to
// (remote-role.json), read per call because apply rewrites it while the proxy
// runs. An unbound contact has none, so the proxy never logs in for it.
func remoteContactUser(st state.State) func(login string) (string, bool) {
	return func(login string) (string, bool) {
		rec, found, err := st.LoadRemoteRoleRecord()
		if err != nil || !found {
			return "", false
		}
		email := config.HubEmailFor(login)
		if !slices.Contains(rec.Contacts, email) {
			return "", false
		}
		id := rec.Bound[email]
		return id, id != ""
	}
}

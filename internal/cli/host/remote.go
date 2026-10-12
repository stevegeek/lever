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

			// One dialler for both the proxied traffic and the login
			// handshake: the handshake IS hub traffic, and must travel the
			// same route into this instance's own jail.
			dial := remoteproxy.JailDial(jailPrefixFn(bf, app.Backend, machineName(app.Name), cmd.ErrOrStderr()))
			provider, handler, push, err := buildRemoteHandler(app, st, dial, auditFn, cmd.ErrOrStderr())
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			// The push watchers live as long as the proxy: the same context
			// ends both, and the serve waits for every hub stream to close.
			pushDone := make(chan struct{})
			go func() {
				defer close(pushDone)
				if push != nil {
					push.Run(ctx)
				}
			}()
			printRemoteWarnings(cmd, app)
			cmd.Printf("remote proxy %q serving on %s, identity header %s (login provider on 127.0.0.1:%d, issuer %s)\n",
				app.Name, app.RemoteListenAddr(), app.EffectiveRemoteIdentityHeader(), provider.Port(), provider.IssuerURL())
			if app.VoiceOn() {
				cmd.Printf("dictation on: through lever-tool-whisper's socket %s\n", app.Remote.Voice.Socket)
			}
			err = serveRemote(ctx, app, st, provider, handler)
			stop()
			<-pushDone
			return err
		},
	}
}

// loadRemoteApp resolves and loads the config for `remote serve`, applying the
// two gates every proxy start must pass.
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
	// Both backends: the proxy dials the hub through the jail
	// (remoteproxy.JailDial), which is backend-agnostic, and
	// config.validateRemote says why the login path is too.
	return path, app, nil
}

// buildRemoteHandler assembles the local OIDC provider, the login driver over
// it, and the proxy handler that fronts the hub.
func buildRemoteHandler(app *config.App, st state.State, dial func(ctx context.Context, network, addr string) (net.Conn, error), auditFn func(remoteproxy.AuditLine), warn io.Writer) (*remoteproxy.Provider, http.Handler, *remoteproxy.Push, error) {
	// The hub's address INSIDE the guest, which is where the dialer
	// lands — so this is also the correct Host header. It is
	// deliberately not a host-reachable address: see
	// scion.DefaultHubEndpoint.
	target, err := url.Parse(scion.DefaultHubEndpoint)
	if err != nil {
		return nil, nil, nil, err
	}
	// remote.push: nil when off. Built before the handler, which serves its
	// routes and worker only when it is non-nil.
	push, err := remotePush(app, st, auditFn, warn)
	if err != nil {
		return nil, nil, nil, err
	}

	// The local OIDC provider, and the driver that logs in with it.
	// Both live in THIS process because an authorization code must be
	// mintable only by an in-process call — see the provider's own
	// documentation for what that property is holding up. The guest
	// reaches this listener through the forwarder `lever apply`
	// installed (internal/backend/guest.EnsureHubLogin).
	provider := remoteproxy.NewProvider(remoteproxy.ProviderConfig{
		Port: app.EffectiveRemoteLoginPort(),
		// The hub dials the GUEST port; the forwarder carries it here.
		// Two numbers on purpose — see config.GuestLoginIssuerPort.
		IssuerPort: config.GuestLoginIssuerPort,
		Audit:      auditFn,
	})
	login := remoteproxy.NewLoginDriver(remoteproxy.LoginConfig{
		Hub:         target,
		DialContext: dial,
		Provider:    provider,
		Audit:       auditFn,
	})

	records, resolve := remoteHubAgents(st, target, dial)
	handler := remoteproxy.NewHandler(remoteproxy.Config{
		Target:      target,
		DialContext: dial,
		ServeHost:   remoteServeHost(app.Remote.BaseURL),
		// So the Host gate admits `lever doctor`'s loopback /healthz
		// probe without widening the allowlist beyond this one port.
		ListenPort:         app.EffectiveRemotePort(),
		AllowedUsers:       app.Remote.Logins(),
		IdentityHeader:     app.EffectiveRemoteIdentityHeader(),
		TrustForwardedHost: app.Remote.TrustForwardedHost,
		BindHost:           remoteBindHost(app),
		Session:            login,
		Audit:              auditFn,
		// Verified web chat. Only with allowed_users: without it no login
		// is verified, so there is nothing to vouch for.
		ChatLedger: remoteChatLedger(app, st),
		// Contacts: chat only, and only with their agents (see
		// remoteproxy/contact.go). Agent names resolve to hub ids with the
		// remote PAT, used only for GET agent lists.
		Contacts:      remoteContacts(app),
		ResolveAgents: resolve,
		// A contact's post reaches an agent only while that agent's session
		// started fresh with the skill on disk now (contactSession).
		ContactSession: func(agent string) error { return contactSession(app, st, agent) },
		// lever's chat page (remote.landing: chat): the manager, the agent
		// list per login (the same hub records, labels from the tree) and
		// the wake of a sleeping worker over the broker's operator socket.
		ChatAgent:    remoteChatAgent(app),
		Workers:      remoteWorkers(app),
		ContactSee:   remoteContactSee(app),
		AgentRecords: records,
		Labels:       remoteLabels(app),
		Wake:         remoteWake(app, st),
		// remote.agent_messages: which agent rows a contact is shown, asked
		// of the broker over its operator socket. Nil when off.
		MatchAgentMessages: remoteAgentMessages(app, st),
		// The operator's read-only view of contact conversations reads only
		// for a contact apply bound to a hub user (opview.go).
		ContactUser:       remoteContactUser(st),
		PeekAgentMessages: remoteAgentMessagesPeek(app, st),
		// The proxy's own log, named the way doctor names it (relative to
		// the instance root) so the denial text stays byte-identical.
		LogPath: stateRel(st, st.RemoteLog()),
		// remote.push: the page's notifications (nil when off).
		Push: push,
		// remote.files: uploads into each agent's .lever-files/in/, and
		// downloads of the files it shares. Nil when off.
		Files: remoteFiles(app, st),
		// remote.voice: dictation through lever-tool-whisper's socket.
		// Nil when off.
		Voice: remoteVoiceConfig(app),
	})
	return provider, handler, push, nil
}

// remoteVoiceConfig is dictation (remote.voice) as the proxy runs it: each
// checked clip goes to lever-tool-whisper's dictation socket. Nil when off.
func remoteVoiceConfig(app *config.App) *remoteproxy.VoiceConfig {
	if !app.VoiceOn() {
		return nil
	}
	c := &voice.DictateClient{Socket: app.Remote.Voice.Socket}
	return &remoteproxy.VoiceConfig{MaxSeconds: app.EffectiveVoiceMaxSeconds(), Excluded: app.VoiceExcludedLogins(),
		ReadAloud: app.Remote.Voice.ReadAloud == nil || *app.Remote.Voice.ReadAloud,
		NotSent:   func(err error) bool { return errors.Is(err, voice.ErrNotSent) },
		Available: c.Available, Transcribe: c.Transcribe}
}

// pushTestHosts is the TEST ONLY exception: both remote.push.test_hosts and
// LEVER_PUSH_TEST_HOSTS must name the same 127.0.0.1 addresses. Either one
// alone is an error, so neither a stray shell variable of whoever runs
// `lever apply` nor a config key copied into a real lever.yaml widens where
// the proxy may connect by itself.
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

// remotePush builds the Web Push service (remote.push), or nil when off.
// A malformed LEVER_PUSH_TEST_HOSTS stops the serve: never a silent
// widening of where the proxy may connect. A fault in the push files keeps
// the proxy serving with push off, and says so.
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

// remoteChatAgent is the agent lever's chat page talks to: the manager, whose
// agent name is the instance name. Empty when the page is off.
func remoteChatAgent(app *config.App) string {
	if !app.RemoteLandingChat() {
		return ""
	}
	return app.Name
}

// remoteContacts maps each contact-tier login to the agents it may chat with.
func remoteContacts(app *config.App) map[string][]string {
	out := map[string][]string{}
	for _, u := range app.Remote.AllowedUsers {
		if u.EffectiveTier() == config.TierContact {
			out[u.Login] = u.Agents
		}
	}
	return out
}

// remoteContactSee maps each contact-tier login with a see list to it.
func remoteContactSee(app *config.App) map[string][]string {
	out := map[string][]string{}
	for _, u := range app.Remote.AllowedUsers {
		if u.EffectiveTier() == config.TierContact && len(u.See) > 0 {
			out[u.Login] = u.See
		}
	}
	return out
}

// remoteWorkers is every configured worker name, in config order.
func remoteWorkers(app *config.App) []string {
	out := make([]string, len(app.Workers))
	for i, w := range app.Workers {
		out[i] = w.Name
	}
	return out
}

// remoteFiles is the chat page's file exchange (remote.files), or nil when
// off. The ledger stays off ("") when the state directory is inside the
// tree, where an agent could write a record.
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

// remoteLabels is the chat page's labels source, or nil when
// remote.labels_file is unset.
func remoteLabels(app *config.App) func() map[string]string {
	if app.Remote.LabelsFile == "" {
		return nil
	}
	return (&remoteproxy.LabelSource{Tree: app.Tree, Rel: app.Remote.LabelsFile}).Labels
}

// remoteHubAgents lists the instance project's agent records with the remote
// PAT (GET only, through HubDoer): the chat page's records and the contact
// fence's name → hub id resolver read the same cached list.
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
// polls every few seconds while it waits for a wake.
const agentRecordsTTL = 3 * time.Second

// agentRecordsRefresh bounds one hub list: it runs on its own context, not
// on any caller's.
const agentRecordsRefresh = 10 * time.Second

// errAgentListPanicked is the fixed error of a hub list that panicked; it
// is not cached, so the next caller refreshes again.
var errAgentListPanicked = errors.New("the hub agent list failed unexpectedly")

// agentFlight is one hub list in progress; done closes when recs and err
// are set.
type agentFlight struct {
	done chan struct{}
	recs map[string]remoteproxy.AgentRecord
	err  error
}

// cachedAgentRecords wraps one hub agent list in a short cache, keyed by
// slug, and derives the name → id resolver from it. The cache is shared by
// every login and by the contact fence, so no caller may spoil it for the
// others:
//
//   - one refresh at a time, on a context of its own (agentRecordsRefresh),
//     so a caller that gives up does not cancel the list the others wait on;
//   - a caller waits for that refresh only as long as its own context lets
//     it, and no lock is held across the hub call;
//   - a hub error is cached for the TTL (a down hub is not asked on every
//     poll), but never a context-class error: that says nothing about the
//     hub's answer.
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
			// A panic in the list is one failed refresh: on this goroutine
			// it would end the proxy and leave the flight open for good.
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
// activity and container status are text an agent can post about itself,
// so only scion's known words pass (scion.PhaseLabel, ActivityLabel).
func agentRecordOf(a hubapi.Agent) remoteproxy.AgentRecord {
	phase := scion.PhaseLabel(a.Phase)
	return remoteproxy.AgentRecord{ID: a.ID, Phase: phase, Activity: scion.ActivityLabel(a.Activity),
		ContainerDown: scion.RunningContainerDown(phase, a.ContainerStatus)}
}

// remoteWake is the chat page's wake: POST /operator/wake on the broker's
// 0600 operator socket, the host user's own channel. Nil when the state
// directory is inside the tree: then the broker binds no operator socket
// (brokerctl.bindListeners), and the page answers every wake "unavailable".
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

// agentMessagesTimeout bounds one match question: the proxy holds the
// contact's history answer while it waits.
const agentMessagesTimeout = 10 * time.Second

// remoteAgentMessages asks the broker which agent rows of a contact's DM its
// agent ledger recorded. Nil when agent messages are off. With the state
// directory inside the tree there is no operator socket: every call fails,
// and the proxy hides every agent row. Any answer but 200 is an error.
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

// remoteAgentMessagesPeek is remoteAgentMessages without binding (the
// request's peek): the operator view's question, which writes nothing. It
// also answers which kept rows are pending (no contact read bound them).
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
		// Pending only for a peek, and only among the kept ids.
		pending := map[string]bool{}
		for _, id := range out.Pending {
			if peek && keep[id] {
				pending[id] = true
			}
		}
		return keep, pending, nil
	}
}

// remoteChatLedger is the proxy's ledger writer, or nil when verified chat is
// off. brokerctl.ChatLedgerPath makes the same decision for the broker.
func remoteChatLedger(app *config.App, st state.State) func(chatledger.Entry) error {
	p := brokerctl.ChatLedgerPath(app, st)
	if p == "" {
		return nil
	}
	removeOldChatLedger(st)
	return chatledger.NewWriter(p).Append
}

// removeOldChatLedger deletes lever 0.27's single-file ledger (and its
// rotated copy): 0.28 keeps one file per login in a directory, and the old
// file only holds old chat text.
func removeOldChatLedger(st state.State) {
	for _, name := range []string{"chat-ledger.jsonl", "chat-ledger.jsonl.1"} {
		p := filepath.Join(st.Dir, name)
		if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() {
			_ = os.Remove(p)
		}
	}
}

// serveRemote runs the proxy until ctx ends, stamping the config THIS process
// actually loaded, not the one whoever started it believes is running. `lever
// apply` reuses a live proxy only when this record matches the config it is
// applying, and apply is not the only thing that starts proxies — this
// command is reachable by hand, and the pid file apply reads is written by
// every serve. Stamping here is what stops a hand-started proxy from
// inheriting the record of an apply-started one. See ServeConfig.Stamp.
func serveRemote(ctx context.Context, app *config.App, st state.State, provider *remoteproxy.Provider, handler http.Handler) error {
	return remoteproxy.Serve(ctx, remoteproxy.ServeConfig{
		Port: app.EffectiveRemotePort(),
		Bind: app.EffectiveRemoteBind(),
		// config.validateRemoteBind already refused every non-loopback
		// address the jail could reach, and the wildcard without its own
		// acknowledgement, so a config that loaded may bind what it names.
		AllowNonLoopback: !app.RemoteBindLoopback(),
		Handler:          handler,
		PIDPath:          st.RemotePID(),
		Provider:         provider,
		Stamp: func() error {
			return st.WriteRemoteStamp(cli.VersionString(), brokerctl.RemoteConfigHash(app))
		},
	})
}

// jailResolveTimeout bounds one attempt to read the jail's run user. The
// probe is three short commands into a running machine; a machine that is
// wedged must fail the dial rather than hold a request open.
const jailResolveTimeout = 15 * time.Second

// jailPrefixFn resolves the argv prefix that reaches inside THIS instance's
// jail, for remoteproxy.JailDial. On OrbStack that prefix embeds the machine's
// run user, so resolving it means talking to the machine.
//
// Resolution is deferred to the first dial, not done at startup: the proxy is
// a long-lived process started alongside the rest of the instance, and
// demanding a running jail before it will serve would make startup order
// load-bearing and turn a stopped jail into a dead proxy. A failed resolve
// returns nil — JailDial renders that as an actionable dial error — and the
// next request tries again.
//
// Success is cached because resolving costs three commands into the machine
// (`orb list`, `whoami`, `id -u`), which is real latency to pay per
// connection, and the run user cannot change under a running machine. A
// rebuilt jail with a different run user therefore needs a proxy restart — and
// `lever apply` does NOT give you one: its reuse check compares the lever
// version, the `remote:` block, the instance name and the backend
// (brokerctl.RemoteConfigHash), none of which a jail rebuild changes, so a
// matching stamp keeps the old process and its stale prefix. Use `lever stop`
// + `lever up` after rebuilding the jail. (Verified against
// remoteController.Start; the comment used to claim the opposite.)
//
// Concurrent dials share one attempt rather than queueing behind each other:
// a browser opens several connections at once, and a wedged machine would
// otherwise hold the second caller for two timeouts, the third for three, and
// so on — the opposite of failing the dial promptly.
//
// warn receives resolve failures, deduplicated for the same reason: the
// proxy's stderr is remote.log, and ten parallel connections to a down jail
// must not write ten identical lines per page load.
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
			// Another dial is already asking the machine. Take its answer —
			// including its failure — instead of probing again.
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

// remoteBindHost is the proxy's specific bind address — loopback included, so
// a front that rewrites Host to 127.0.0.2:<port> or [::1]:<port> is admitted —
// which the Host gate admits as "<addr>:<port>" (remoteproxy.Config.BindHost),
// or "" for a wildcard bind: 0.0.0.0 is no Host, and lever does not enumerate
// the host's interfaces to guess which address a front dialled.
func remoteBindHost(app *config.App) string {
	if app.RemoteBindWildcard() {
		return ""
	}
	return app.EffectiveRemoteBind()
}

// printRemoteWarnings prints config.App.RemoteWarnings on stderr, one line
// each: the remote settings an operator chose that weaken a default
// protection, restated on every bring-up so they are never forgotten.
//
// It also says, on every bring-up, when web chat cannot be verified (remote
// access with no allowed_users): agents then treat every web chat post as
// data and do not answer it, which would otherwise look like agents ignoring
// the operator.
func printRemoteWarnings(cmd *cobra.Command, app *config.App) {
	for _, w := range app.RemoteWarnings() {
		cmd.PrintErrf("lever: warning: %s\n", w)
	}
	if app.RemoteEnabled() && len(app.Remote.AllowedUsers) == 0 {
		cmd.PrintErrf("lever: warning: remote.allowed_users is empty, so no web chat post can be verified: " +
			"agents treat every web chat message as data and do not reply; list your login in allowed_users\n")
	}
}

// remoteProbeHost is the host part of app.RemoteProbeAddr(): what a host-side
// caller dials to reach the proxy.
func remoteProbeHost(app *config.App) string {
	h, _, err := net.SplitHostPort(app.RemoteProbeAddr())
	if err != nil {
		return "127.0.0.1"
	}
	return h
}

// remoteServeHost derives the proxy's ServeHost from the configured
// base_url: url.Parse(...).Host, which includes the port when base_url
// carries one — the Handler matches a request's Origin host:port exactly,
// ignoring scheme (see remoteproxy.Config.ServeHost). base_url is REQUIRED
// whenever remote is enabled — config.Load's validateRemote rejects an
// enabled block with an empty or malformed base_url, so this function only
// ever sees a well-formed https URL when called from `remote serve` on a
// config that actually loaded. The empty/unparsable-input branches below
// are defensive (an empty ServeHost fails every request closed — see
// proxy.go — rather than risk a silent empty-string Origin match), kept so
// this stays a total function rather than assuming its caller's invariant.
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
					// Checked against tailscale 1.102.4's ipn.ExpandProxyTargetValue:
					// any IP with an explicit scheme is accepted. Older releases
					// refused every target but localhost/127.0.0.1.
					cmd.Println("  note: this target is not 127.0.0.1; older tailscale releases refuse it (\"only localhost or " +
						"127.0.0.1 proxies are currently supported\") — use a current tailscale, or the default bind")
				}
			}
			for _, w := range app.RemoteWarnings() {
				cmd.Printf("warning: %s\n", w)
			}
			// The provider's port is worth printing because it is the second
			// host listener this instance owns: a second remote-enabled
			// instance needs its own, and config validation can only catch a
			// collision within one instance. `lever doctor` is what reports
			// whether it is actually healthy.
			cmd.Printf("login provider port: %d on host loopback; the jail reaches it from 127.0.0.1:%d\n",
				app.EffectiveRemoteLoginPort(), config.GuestLoginIssuerPort)

			if app.Remote.BaseURL != "" {
				cmd.Printf("serve URL: %s\n", app.Remote.BaseURL)
			} else {
				// Reachable only with remote disabled (or unconfigured): an
				// enabled block with no base_url is rejected at config load
				// (validateRemote), so this can no longer describe a proxy
				// that's up and 403ing everything — remote access simply
				// isn't turned on yet.
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

// remoteContactUser is the operator view's source of a contact's hub user
// id: the one `lever apply` bound it to, as a contact (remote-role.json).
// Read on each call: apply rewrites the file while the proxy runs, and the
// operator view is read rarely. A contact apply did not bind (it had not
// signed in) has none, so the proxy never logs in for it.
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

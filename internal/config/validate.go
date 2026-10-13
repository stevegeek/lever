package config

import (
	"fmt"
	"net"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/chatfiles"
	"github.com/stevegeek/lever/internal/egress"
	"github.com/stevegeek/lever/internal/fsutil"
	"github.com/stevegeek/lever/internal/hubauth"
	"github.com/stevegeek/lever/internal/opsig"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/voice"
	"github.com/stevegeek/lever/internal/webpush"
	"github.com/stevegeek/lever/internal/wire"
)

// validate checks one broker.tools entry's shape, failing closed. Whether a
// supervised command is spawnable is a host probe (Tool.checkHost).
func (t Tool) validate() error {
	if !t.Gate.valid() {
		return fmt.Errorf("config: broker tool %q gate %q invalid (want fine|coarse)", t.Name, t.Gate)
	}
	for _, o := range t.Operations {
		if o.Name == "" {
			return fmt.Errorf("config: broker tool %q has an operation with an empty name", t.Name)
		}
		if o.Name == wire.WildcardOp {
			return fmt.Errorf("config: broker tool %q declares operation %q, which is reserved for gate: coarse", t.Name, wire.WildcardOp)
		}
	}
	if !t.External {
		if t.Gate != "" {
			return fmt.Errorf("config: broker tool %q sets gate but is not external (gate applies to external tools only)", t.Name)
		}
		if t.AllowNonLoopback {
			return fmt.Errorf("config: broker tool %q sets allow_non_loopback but is not external (applies to external tools only)", t.Name)
		}
		if len(t.Command) == 0 {
			return fmt.Errorf("config: broker tool %q has no command (a supervised tool needs one; did you mean external: true?)", t.Name)
		}
		return nil
	}
	if len(t.Command) != 0 {
		return fmt.Errorf("config: external broker tool %q must not set command (the broker fronts external servers, it does not spawn them)", t.Name)
	}
	if t.Backend == "" {
		return fmt.Errorf("config: external broker tool %q needs backend (the server's own listen address, host:port[/path])", t.Name)
	}
	if err := t.validateExternalBackend(); err != nil {
		return err
	}
	if t.EffectiveGate() == GateCoarse {
		if len(t.Operations) != 0 {
			return fmt.Errorf("config: external broker tool %q is gate: coarse and must not declare operations (coarse admits the whole surface; use gate: fine to enumerate)", t.Name)
		}
		if len(t.AllowedValues) != 0 {
			return fmt.Errorf("config: external broker tool %q is gate: coarse and must not declare allowed_values (there are no per-operation params to pin)", t.Name)
		}
		return nil
	}
	if len(t.Operations) == 0 {
		return fmt.Errorf("config: external broker tool %q is gate: fine and needs operations (or set gate: coarse to admit the whole surface)", t.Name)
	}
	return nil
}

// validateExternalBackend confines an external backend to a literal loopback
// IP unless allow_non_loopback is set: the broker proxies host-side, so a
// non-loopback backend would let the jailed agent reach other hosts through
// it. Hostnames (even "localhost") are rejected: only a literal IP can be
// loopback-checked without trusting a resolver.
func (t Tool) validateExternalBackend() error {
	b := t.Backend
	if strings.Contains(b, "://") {
		return fmt.Errorf("config: external broker tool %q backend %q must not carry a scheme (want host:port[/path])", t.Name, b)
	}
	hostport := b
	if i := strings.IndexByte(b, '/'); i >= 0 {
		hostport = b[:i]
	}
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return fmt.Errorf("config: external broker tool %q backend %q: %v (want host:port[/path])", t.Name, b, err)
	}
	if t.AllowNonLoopback {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("config: external broker tool %q backend %q is not a literal loopback IP — the broker proxies host-side, so a non-loopback backend would let the jailed agent reach other hosts through the broker; set allow_non_loopback: true only if that is exactly what you intend", t.Name, b)
	}
	return nil
}

// validateBackend refuses an unknown backend so nothing is ever silently
// swapped for the default.
func validateBackend(name string) error {
	if name == "" {
		return fmt.Errorf("config: backend is required (valid: %s)", strings.Join(backendNames(), ", "))
	}
	if !knownBackend(name) {
		return fmt.Errorf("config: unknown backend %q (valid: %s)", name, strings.Join(backendNames(), ", "))
	}
	return nil
}

// nameRE: an instance or worker name becomes a jail machine name and a shell
// token in the scion-install path.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

var diskRE = regexp.MustCompile(`^[0-9]+(MiB|GiB|MB|GB)$`)
var sizeRE = regexp.MustCompile(`^([0-9]+)(MiB|GiB|MB|GB)$`)

func validateDisk(d string) error {
	if d == "" {
		return nil
	}
	if !diskRE.MatchString(d) {
		return fmt.Errorf("config: disk %q invalid (want e.g. 24GiB, 40GiB)", d)
	}
	return nil
}

func sizeBytes(s string) (int64, bool) {
	m := sizeRE.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, false
	}
	unit := map[string]int64{"MiB": 1 << 20, "GiB": 1 << 30, "MB": 1e6, "GB": 1e9}[m[2]]
	return n * unit, true
}

// validateLimaKeys: nested_virt needs the qemu driver, which lever uses on
// Linux hosts only (vz on darwin).
func validateLimaKeys(a *App, goos string) error {
	if a.Backend != BackendLima {
		for _, k := range []struct {
			key string
			set bool
		}{{"nested_virt", a.NestedVirt}, {"cpus", a.CPUs != 0}, {"memory", a.Memory != ""}, {"guest_kernel", a.GuestKernel != ""}, {"guest_profile", a.GuestProfile != ""}} {
			if k.set {
				return fmt.Errorf("config: %s is Lima-only; OrbStack manages its own machine resources", k.key)
			}
		}
		return nil
	}
	if a.NestedVirt && goos == "darwin" {
		return fmt.Errorf("config: nested_virt needs the qemu driver on a Linux host; Lima's vz nestedVirtualization is not supported yet")
	}
	switch a.GuestKernel {
	case "", GuestKernelHWE, GuestKernelGA:
	default:
		return fmt.Errorf("config: guest_kernel %q invalid (want %q or %q, or unset: %q with nested_virt, else %q)",
			a.GuestKernel, GuestKernelHWE, GuestKernelGA, GuestKernelHWE, GuestKernelGA)
	}
	switch a.GuestProfile {
	case "", GuestProfileStandard, GuestProfileSmall:
	default:
		return fmt.Errorf("config: guest_profile %q invalid (want %q or %q, or unset: %q)",
			a.GuestProfile, GuestProfileStandard, GuestProfileSmall, GuestProfileStandard)
	}
	if a.CPUs < 0 || a.CPUs > 256 {
		return fmt.Errorf("config: cpus %d out of range (1-256, or unset for the Lima default)", a.CPUs)
	}
	if a.Memory != "" {
		n, ok := sizeBytes(a.Memory)
		if !ok {
			return fmt.Errorf("config: memory %q invalid (want e.g. 8GiB, 24GiB)", a.Memory)
		}
		if n < 2<<30 {
			return fmt.Errorf("config: memory %q too small: at least 2GiB", a.Memory)
		}
	}
	return nil
}

// toolNameRE: a tool name becomes the /mcp/<name>/ route and a `claude mcp
// add` token, so it must be safe in a URL path and never collide with the
// reserved wildcard op. Underscores are admitted because MCP server names use
// them.
var toolNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// imageRE admits no whitespace or shell metacharacters.
var imageRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/@-]*$`)

var digestRE = regexp.MustCompile(`@[a-z0-9]+:[0-9a-fA-F]{32,}$`)

func (s Security) validateImage(field, ref string) error {
	if !imageRE.MatchString(ref) {
		return fmt.Errorf("config: %s %q has invalid characters", field, ref)
	}
	if len(s.AllowedImageRegistries) > 0 && !registryAllowed(ref, s.AllowedImageRegistries) {
		return fmt.Errorf("config: %s %q is not from an allowed registry (allowed: %s)", field, ref, strings.Join(s.AllowedImageRegistries, ", "))
	}
	if s.RequireImageDigest && !digestRE.MatchString(ref) {
		return fmt.Errorf("config: %s %q must be pinned by digest (…@sha256:<hex>); a mutable tag is not allowed", field, ref)
	}
	return nil
}

// registryAllowed matches whole path components, so "scionlocal" does not
// allow "scionlocalevil/x".
func registryAllowed(ref string, prefixes []string) bool {
	for _, p := range prefixes {
		if ref == p || strings.HasPrefix(ref, p+"/") {
			return true
		}
	}
	return false
}

// Validate checks the config's shape without touching the host; CheckHost
// does the host probes.
func (a *App) Validate() error {
	if a.Name == "" {
		return fmt.Errorf("config: name is required")
	}
	if !nameRE.MatchString(a.Name) {
		return fmt.Errorf("config: name %q must match %s (it becomes the jail machine name and a shell token)", a.Name, nameRE)
	}
	if err := validateBackend(a.Backend); err != nil {
		return err
	}
	if a.ManagerCN() == operatorName {
		return fmt.Errorf("config: broker.manager_identity %q is reserved — it is the name the broker's audit log gives the host operator", operatorName)
	}
	if !a.Broker.AutoReenrol.valid() {
		return fmt.Errorf("config: broker.auto_reenrol %q must be one of all|manager|off (or unset = all)", a.Broker.AutoReenrol)
	}
	if !a.Scion.Telemetry.valid() {
		return fmt.Errorf("config: scion.telemetry %q must be one of off|scion-default (or unset = off)", a.Scion.Telemetry)
	}
	if err := validateDisk(a.Disk); err != nil {
		return err
	}
	if err := validateLimaKeys(a, runtime.GOOS); err != nil {
		return err
	}
	if a.Tree == "" {
		return fmt.Errorf("config: tree is required")
	}
	if a.Manager.Image != "" {
		if err := a.Security.validateImage("manager.image", a.Manager.Image); err != nil {
			return err
		}
	}
	// manager.allow_ports opens a host-loopback port to the jailed agent. The
	// broker's admin port is unauthenticated and the egress allowlist is the
	// only thing isolating it from the guest, so it must never be listed.
	adminPort := a.EffectiveAdminPort()
	for _, p := range a.Manager.AllowPorts {
		if p == adminPort {
			return fmt.Errorf("config: manager.allow_ports must not include the broker admin port (%d) — this would hand the jailed agent a direct, unauthenticated path to /bootstrap, /revoke, /bump-epoch (the egress allowlist is the only thing isolating the host-loopback admin API from the guest)", adminPort)
		}
	}
	// Nor a first-party tool's backend port: the tool trusts the
	// X-Lever-Caller header the broker sets, so an agent dialling the port
	// directly could name any caller and skip mTLS binding, revocation and
	// audit. The per-boot tool secret is defence in depth, not a reason to
	// open the port.
	for _, t := range a.Broker.Tools {
		if t.External {
			continue
		}
		port, ok := backendPort(t.Backend)
		if !ok {
			continue
		}
		if slices.Contains(a.Manager.AllowPorts, port) {
			return fmt.Errorf("config: manager.allow_ports must not include %d, the backend port of first-party broker tool %q — the jailed agent could dial the tool directly and bypass the broker (mTLS caller binding, per-agent revocation, audit); agents reach a first-party tool only through the broker's /mcp/%s/ route", port, t.Name, t.Name)
		}
	}
	if a.Manager.PromptFile != "" && !confinedRel(a.Manager.PromptFile) {
		return fmt.Errorf("config: manager.prompt_file %q must be a relative path inside the instance root (no \"..\", not absolute)", a.Manager.PromptFile)
	}
	if a.Manager.InstructionsFile != "" && !confinedRel(a.Manager.InstructionsFile) {
		return fmt.Errorf("config: manager.instructions_file %q must be a relative path inside the instance root (no \"..\", not absolute)", a.Manager.InstructionsFile)
	}
	if err := validateImageTar("manager", a.Manager.Image, a.Manager.ImageTar); err != nil {
		return err
	}
	if err := validateClaude("manager", a.Manager.ClaudeSettings, a.Manager.AfterCompactNote); err != nil {
		return err
	}
	if err := a.validateResources("manager", a.Manager.Resources); err != nil {
		return err
	}
	if err := a.validateWorkerCap(); err != nil {
		return err
	}
	for _, g := range a.Workers {
		if err := a.validateWorker(g); err != nil {
			return err
		}
	}
	if err := a.validateWorkerDirsDisjoint(); err != nil {
		return err
	}
	if err := a.validateManagerReadOnly(); err != nil {
		return err
	}
	if err := a.validateSharedFolders(); err != nil {
		return err
	}
	if err := a.validateImageTarsDistinct(); err != nil {
		return err
	}
	if err := a.validateBroker(); err != nil {
		return err
	}
	if err := a.validateWhisperTools(); err != nil {
		return err
	}
	if err := a.validateTTSTools(); err != nil {
		return err
	}
	if err := a.validateLeverFilesTools(); err != nil {
		return err
	}
	if err := a.validateOperator(); err != nil {
		return err
	}
	if err := a.validateRemote(); err != nil {
		return err
	}
	return nil
}

// backendPort accepts the shapes broker.backendURL accepts. An unparsable
// value is not port-checked; the shape rules live elsewhere.
func backendPort(backend string) (int, bool) {
	if i := strings.Index(backend, "://"); i >= 0 {
		backend = backend[i+3:]
	}
	if i := strings.IndexByte(backend, '/'); i >= 0 {
		backend = backend[:i]
	}
	_, portStr, err := net.SplitHostPort(backend)
	if err != nil {
		return 0, false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		return 0, false
	}
	return port, true
}

// managerAlias is reserved: the operator-directive channel resolves it to the
// manager whatever broker.manager_identity says (broker.resolveDirectiveAgent).
const managerAlias = "manager"

// operatorName is the actor the broker's audit log gives the host operator.
// No agent may carry it: its audit lines would read as the operator's.
const operatorName = "operator"

// validateWorkerDirsDisjoint: sibling isolation rests on each worker
// mounting only its own subtree, so an overlap would let the outer worker
// read and write the inner one's whole workspace.
func (a *App) validateWorkerDirsDisjoint() error {
	for i, outer := range a.Workers {
		for j, inner := range a.Workers {
			if i >= j {
				continue
			}
			if pathOverlaps(outer.Dir, inner.Dir) {
				return fmt.Errorf("config: workers %q (dir %q) and %q (dir %q) overlap — one is the other, or contains it; "+
					"each worker must mount a subtree no other worker can reach, or it can read and write its sibling's workspace",
					outer.Name, outer.Dir, inner.Name, inner.Dir)
			}
		}
	}
	return nil
}

// pathOverlaps compares components, so "workers/x" does not contain
// "workers/xy".
func pathOverlaps(a, b string) bool {
	ca, cb := filepath.Clean(a), filepath.Clean(b)
	if ca == cb {
		return true
	}
	sep := string(filepath.Separator)
	return strings.HasPrefix(ca, cb+sep) || strings.HasPrefix(cb, ca+sep)
}

func (a *App) validateWorker(g Worker) error {
	if g.Name == "" || g.Dir == "" {
		return fmt.Errorf("config: worker needs name + dir (got %+v)", g)
	}
	if err := validateImageTar(fmt.Sprintf("workers[%s]", g.Name), g.Image, g.ImageTar); err != nil {
		return err
	}
	if !nameRE.MatchString(g.Name) {
		return fmt.Errorf("config: worker name %q must match %s", g.Name, nameRE)
	}
	if g.Name == a.ManagerCN() {
		return fmt.Errorf("config: worker name %q collides with the manager identity — a worker must not share the manager's CN", g.Name)
	}
	if g.Name == managerAlias {
		// An operator directive aimed at such a worker would reach the
		// manager instead. The ManagerCN check does not cover it when a
		// custom manager_identity moves the CN off this word.
		return fmt.Errorf("config: worker name %q is reserved — it is the alias the operator-directive channel resolves to the manager, so a worker of that name could never be addressed", g.Name)
	}
	if g.Name == operatorName {
		return fmt.Errorf("config: worker name %q is reserved — it is the name the broker's audit log gives the host operator", g.Name)
	}
	if g.Name == a.Name {
		// The manager's scion agent slug is the app name and the broker
		// routes by slug, so messages to such a worker would reach the
		// manager.
		return fmt.Errorf("config: worker name %q collides with the manager agent (the app name) — rename the worker or the app", g.Name)
	}
	if filepath.IsAbs(g.Dir) || strings.HasPrefix(filepath.Clean(g.Dir), "..") {
		return fmt.Errorf("config: worker dir %q must be relative and inside the tree", g.Dir)
	}
	if filepath.Clean(g.Dir) == "." {
		// A "." dir would mount the whole tree into the worker, defeating
		// sibling isolation.
		return fmt.Errorf("config: worker %q dir must be a subdir of the tree, not %q (which collides with the manager's mount root)", g.Name, g.Dir)
	}
	if g.Image != "" {
		if err := a.Security.validateImage(fmt.Sprintf("worker %q image", g.Name), g.Image); err != nil {
			return err
		}
	}
	if g.InstructionsFile != "" && !confinedRel(g.InstructionsFile) {
		return fmt.Errorf("config: worker %q instructions_file %q must be a relative path inside the instance root (no \"..\", not absolute)", g.Name, g.InstructionsFile)
	}
	if err := validateClaude(fmt.Sprintf("worker %q", g.Name), g.ClaudeSettings, g.AfterCompactNote); err != nil {
		return err
	}
	return a.validateResources(fmt.Sprintf("worker %q", g.Name), g.Resources)
}

// validateOperator checks the expiry pair even with the channel disabled, so
// a bad pair is caught at load rather than when allowed_signers is later set.
// opsig.MaxExpiry is the ceiling the directive-signing protocol assumes.
func (a *App) validateOperator() error {
	if a.Operator.AllowedSigners != "" && !confinedRel(a.Operator.AllowedSigners) {
		return fmt.Errorf("config: operator: allowed_signers %q must be a relative path inside the instance root (no \"..\", not absolute)", a.Operator.AllowedSigners)
	}
	if a.Operator.DirectiveExpiry < 0 {
		return fmt.Errorf("config: operator: directive_expiry %s must not be negative", a.Operator.DirectiveExpiry)
	}
	if a.Operator.DirectiveExpiryMax < 0 {
		return fmt.Errorf("config: operator: directive_expiry_max %s must not be negative", a.Operator.DirectiveExpiryMax)
	}
	if a.EffectiveDirectiveExpiryMax() > opsig.MaxExpiry {
		return fmt.Errorf("config: operator: directive_expiry_max %s exceeds the 24h hard ceiling", a.EffectiveDirectiveExpiryMax())
	}
	if a.EffectiveDirectiveExpiry() > a.EffectiveDirectiveExpiryMax() {
		return fmt.Errorf("config: operator: directive_expiry %s exceeds directive_expiry_max %s", a.EffectiveDirectiveExpiry(), a.EffectiveDirectiveExpiryMax())
	}
	return nil
}

// validateRemote rejects a remote block that could not serve safely. It is
// skipped while remote is disabled, so stale settings do not block loading.
// The GuestLoginIssuerPort checks matter on OrbStack only (lever's Lima
// template mirrors no guest port onto the host) and are harmless on Lima.
func (a *App) validateRemote() error {
	if !a.Remote.Enabled {
		return nil
	}
	rp := a.EffectiveRemotePort()
	if rp == a.EffectiveJailPort() || rp == a.EffectiveAdminPort() {
		return fmt.Errorf("config: remote: port %d collides with a broker listener", rp)
	}
	if rp == GuestLoginIssuerPort {
		// OrbStack mirrors the jail's login forwarder onto this host port.
		return fmt.Errorf("config: remote: port %d is the port the jail's login forwarder is mirrored onto "+
			"by the container runtime, so the proxy cannot bind it — pick another", rp)
	}
	if slices.Contains(a.Manager.AllowPorts, rp) {
		// A trust boundary, not a bind collision: the proxy believes the
		// identity header only because just the authenticating front reaches
		// it. A port in manager.allow_ports gets an egress ACCEPT from the
		// jail, so any agent could set the header itself and ride the
		// operator's hub session (agent.attach on every agent).
		return fmt.Errorf("config: remote: manager.allow_ports lists %d, which is the remote proxy's own port — "+
			"that grant lets any jailed agent reach the proxy directly and forge the %s header "+
			"it trusts, riding the operator's hub session (agent.attach on every agent in the project); remove %d from "+
			"manager.allow_ports, or move the proxy with remote.port", rp, a.EffectiveRemoteIdentityHeader(), rp)
	}
	if a.Remote.BaseURL == "" {
		return fmt.Errorf("config: remote: base_url is required when remote is enabled (without it the proxy refuses all requests)")
	}
	u, err := url.Parse(a.Remote.BaseURL)
	if err != nil || !u.IsAbs() || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("config: remote: base_url %q must be an absolute https URL", a.Remote.BaseURL)
	}
	if err := validRemoteIdentityHeader(a.EffectiveRemoteIdentityHeader()); err != nil {
		return err
	}
	if err := a.validateRemoteBind(); err != nil {
		return err
	}
	seen := map[string]string{}
	for _, u := range a.Remote.AllowedUsers {
		au := u.Login
		if err := validRemoteLogin(au); err != nil {
			return err
		}
		if err := a.validRemoteUserTier(u); err != nil {
			return err
		}
		// scion lowercases emails, so two entries that differ only in case
		// would be one hub user while the proxy treats them as two logins.
		key := strings.ToLower(au)
		if prev, dup := seen[key]; dup {
			return fmt.Errorf("config: remote: allowed_users lists %q and %q, which the hub treats as the same user (it lowercases emails); keep one", prev, au)
		}
		seen[key] = au
	}
	switch a.Remote.Landing {
	case "", RemoteLandingConsole:
	case RemoteLandingChat:
		if len(a.Remote.LoginsWithTier(TierOperator)) == 0 {
			return fmt.Errorf("config: remote: landing: chat needs an operator login in allowed_users (without one no message from the page is verified)")
		}
	default:
		return fmt.Errorf("config: remote: landing %q; use %s or %s", a.Remote.Landing, RemoteLandingConsole, RemoteLandingChat)
	}
	am := a.Remote.AgentMessages
	if am.FollowUpAfter != 0 && (am.FollowUpAfter < time.Hour || am.FollowUpAfter > 720*time.Hour) {
		return fmt.Errorf("config: remote: agent_messages.follow_up_after %v; use 1h to 720h (or leave it out for %v)", am.FollowUpAfter, DefaultAgentFollowUpAfter)
	}
	if am.MaxChars < 0 || am.MaxChars > MaxAgentMaxChars {
		return fmt.Errorf("config: remote: agent_messages.max_chars %d; use 1 to %d (or leave it out for %d)", am.MaxChars, MaxAgentMaxChars, DefaultAgentMaxChars)
	}
	if am.Enabled && len(a.Remote.LoginsWithTier(TierContact)) == 0 {
		return fmt.Errorf("config: remote: agent_messages is enabled but allowed_users has no tier: contact entry; agents message only contacts")
	}
	if am.Enabled && a.ScionVersionPredatesAgentMessages() {
		return fmt.Errorf("config: remote: agent_messages needs scion %s (2026-08-28) or later, where the hub sets a message's sender itself; scion.version %s is older, so an agent could post as its contact", AgentMessagesScionFloor[:8], a.Scion.Version)
	}
	if lf := a.Remote.LabelsFile; lf != "" {
		if strings.Contains(lf, `\`) || path.Clean(lf) != lf || lf == "." || !filepath.IsLocal(filepath.FromSlash(lf)) {
			return fmt.Errorf("config: remote: labels_file %q must be a clean path relative to tree (no leading /, no ..)", lf)
		}
		if err := a.validLabelsFileOwner(lf); err != nil {
			return err
		}
	}
	if a.Remote.Push.Enabled {
		if a.Remote.Landing != RemoteLandingChat {
			return fmt.Errorf("config: remote: push needs landing: chat (a notification opens the chat page)")
		}
		if err := validPushSubject(a.Remote.Push.Subject); err != nil {
			return err
		}
	}
	if th := a.Remote.Push.TestHosts; len(th) > 0 {
		if !a.Remote.Push.Enabled {
			return fmt.Errorf("config: remote: push.test_hosts is set but push is off — drop it")
		}
		if _, err := webpush.ParseTestHosts(strings.Join(th, ",")); err != nil {
			return fmt.Errorf("config: remote: push.test_hosts: each entry must be 127.0.0.1:<port> (TEST ONLY): %v", err)
		}
	}
	fl := a.Remote.Files
	if fl.MaxBytes < 0 || fl.MaxBytes > MaxFilesMaxBytes {
		return fmt.Errorf("config: remote: files.max_bytes %d; use 1 to %d (or leave it out for %d)", fl.MaxBytes, MaxFilesMaxBytes, DefaultFilesMaxBytes)
	}
	seenExt := map[string]bool{}
	for _, e := range fl.Extensions {
		if !filesExtRE.MatchString(e) {
			return fmt.Errorf("config: remote: files.extensions %q: lowercase letters and digits only, 1 to 10, no dot", e)
		}
		if slices.Contains(filesActiveExts, e) {
			return fmt.Errorf("config: remote: files.extensions %q is active content a browser runs; share it inside a zip", e)
		}
		if seenExt[e] {
			return fmt.Errorf("config: remote: files.extensions lists %q twice", e)
		}
		seenExt[e] = true
	}
	if fl.Enabled {
		if a.Remote.Landing != RemoteLandingChat {
			return fmt.Errorf("config: remote: files needs landing: chat (the upload and download routes are the chat page's)")
		}
		// A worker mounted inside the manager's exchange would read and write
		// the manager's files. Case-folded: the tree may be on a
		// case-insensitive filesystem.
		for _, w := range a.Workers {
			if fsutil.RelOverlapFold(w.Dir, chatfiles.Dir) {
				return fmt.Errorf("config: remote: files is on, and worker %q dir %q overlaps %s, the manager's file exchange; move the worker", w.Name, w.Dir, chatfiles.Dir)
			}
		}
		for _, e := range a.Manager.ReadOnly {
			if fsutil.RelOverlapFold(e, chatfiles.Dir) {
				return fmt.Errorf("config: remote: files is on, and manager.read_only %q covers %s: the manager could not write the files it shares", e, chatfiles.Dir)
			}
		}
	}
	return a.validateVoice()
}

// validateVoice: the socket must match a whisper tool in broker.tools only
// when there is one, since the tool may also be run by hand.
func (a *App) validateVoice() error {
	v := a.Remote.Voice
	if v.MaxSeconds < 0 || v.MaxSeconds > MaxVoiceMaxSeconds {
		return fmt.Errorf("config: remote: voice.max_seconds %d; use 1 to %d (or leave it out for %d)", v.MaxSeconds, MaxVoiceMaxSeconds, DefaultVoiceMaxSeconds)
	}
	if !v.ReadAloud.valid() {
		return fmt.Errorf("config: remote: voice.read_aloud %q; use device, server or off", v.ReadAloud)
	}
	if !v.Enabled {
		return nil
	}
	if err := a.validateSpeech(); err != nil {
		return err
	}
	if a.Remote.Landing != RemoteLandingChat {
		return fmt.Errorf("config: remote: voice needs landing: chat (the mic is on the chat page)")
	}
	if v.Socket == "" {
		return fmt.Errorf("config: remote: voice.socket is required when voice is on: the -dictate-socket of the lever-tool-whisper entry in broker.tools")
	}
	if err := voice.CheckSocketPath(v.Socket); err != nil {
		return fmt.Errorf("config: remote: voice.socket: %w", err)
	}
	tools := a.WhisperTools()
	if len(tools) == 0 {
		return nil
	}
	w, ok := a.VoiceTool()
	if !ok {
		var names []string
		for _, t := range tools {
			names = append(names, fmt.Sprintf("%s (-dictate-socket %s)", t.Name, t.Socket))
		}
		return fmt.Errorf("config: remote: voice.socket %s is not the -dictate-socket of any whisper tool in broker.tools: %s; set them to the same path",
			v.Socket, strings.Join(names, ", "))
	}
	if a.EffectiveVoiceMaxSeconds() > w.EffectiveMaxSeconds() {
		return fmt.Errorf("config: remote: voice.max_seconds %d is more than broker tool %q's -max-seconds %d, which would refuse such a clip",
			a.EffectiveVoiceMaxSeconds(), w.Name, w.EffectiveMaxSeconds())
	}
	return nil
}

// validateSpeech: the socket must match a tts tool in broker.tools only when
// there is one, since the tool may also be run by hand.
func (a *App) validateSpeech() error {
	v := a.Remote.Voice
	if a.EffectiveReadAloud() != ReadAloudServer {
		return nil
	}
	if v.SpeakSocket == "" {
		return fmt.Errorf("config: remote: voice.speak_socket is required with read_aloud: server: the -speak-socket of the lever-tool-tts entry in broker.tools")
	}
	if err := voice.CheckSocketPath(v.SpeakSocket); err != nil {
		return fmt.Errorf("config: remote: voice.speak_socket: %w", err)
	}
	if v.SpeakSocket == v.Socket {
		return fmt.Errorf("config: remote: voice.speak_socket is voice.socket, the dictation socket; give lever-tool-tts its own")
	}
	tools := a.TTSTools()
	if len(tools) == 0 {
		return nil
	}
	if _, ok := a.SpeechTool(); !ok {
		var names []string
		for _, t := range tools {
			names = append(names, fmt.Sprintf("%s (-speak-socket %s)", t.Name, t.Socket))
		}
		return fmt.Errorf("config: remote: voice.speak_socket %s is not the -speak-socket of any tts tool in broker.tools: %s; set them to the same path",
			v.SpeakSocket, strings.Join(names, ", "))
	}
	return nil
}

func validPushSubject(s string) error {
	bad := fmt.Errorf("config: remote: push.subject %q must be mailto:<address> or an https URL (push services use it to contact this sender)", s)
	if s == "" || len(s) > 200 {
		return bad
	}
	for _, c := range []byte(s) {
		if c <= ' ' || c >= 0x7f {
			return bad
		}
	}
	if addr, ok := strings.CutPrefix(s, "mailto:"); ok {
		local, domain, found := strings.Cut(addr, "@")
		if !found || local == "" || domain == "" || strings.ContainsAny(addr, ",;?<>\"") || strings.Contains(domain, "@") {
			return bad
		}
		return nil
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return bad
	}
	return nil
}

var filesExtRE = regexp.MustCompile(`^[a-z0-9]{1,10}$`)

// filesActiveExts are types a browser renders or runs. Downloads are
// attachments with nosniff and a sandbox CSP; refusing these too keeps one
// mistake in those headers from serving a page on lever's origin.
var filesActiveExts = []string{"html", "htm", "xhtml", "shtml", "svg", "js", "mjs", "xml"}

func (a *App) validRemoteUserTier(u RemoteUser) error {
	switch u.EffectiveTier() {
	case TierOperator:
		if len(u.Agents) > 0 {
			return fmt.Errorf("config: remote: allowed_users %q is an operator, which reaches every agent; remove its agents list, or set tier: contact", u.Login)
		}
		if len(u.See) > 0 {
			return fmt.Errorf("config: remote: allowed_users %q is an operator, which sees every agent; remove its see list", u.Login)
		}
		return nil
	case TierContact:
	default:
		return fmt.Errorf("config: remote: allowed_users %q has tier %q; use operator or contact", u.Login, u.Tier)
	}
	if len(u.Agents) == 0 {
		return fmt.Errorf("config: remote: allowed_users %q is a contact and must list its agents (agents: [<worker>, ...])", u.Login)
	}
	seen := map[string]bool{}
	for _, name := range u.Agents {
		if !a.knownAgent(name) {
			return fmt.Errorf("config: remote: allowed_users %q lists agent %q, which is not a declared worker or the manager (%s)", u.Login, name, a.Name)
		}
		if seen[name] {
			return fmt.Errorf("config: remote: allowed_users %q lists agent %q twice", u.Login, name)
		}
		seen[name] = true
	}
	seenSee := map[string]bool{}
	for _, name := range u.See {
		if !a.knownAgent(name) {
			return fmt.Errorf("config: remote: allowed_users %q lists agent %q in see, which is not a declared worker or the manager (%s)", u.Login, name, a.Name)
		}
		if slices.Contains(u.Agents, name) {
			return fmt.Errorf("config: remote: allowed_users %q lists agent %q in both agents and see; keep it in one", u.Login, name)
		}
		if seenSee[name] {
			return fmt.Errorf("config: remote: allowed_users %q lists agent %q twice in see", u.Login, name)
		}
		seenSee[name] = true
	}
	return nil
}

// validLabelsFileOwner refuses a labels file someone other than the manager
// would write: whoever writes it sets the text contacts see beside every
// agent's name. It must not be inside a worker's directory or the host's own
// directories (.lever, the state directory), which hold host-written secrets.
// Compared without case for case-insensitive filesystems. Not
// Unicode-normalised: both sides are operator-written config, not agent
// input.
func (a *App) validLabelsFileOwner(lf string) error {
	inside := func(p, dir string) bool {
		p, dir = strings.ToLower(p), strings.ToLower(dir)
		return p == dir || strings.HasPrefix(p, dir+"/")
	}
	for _, w := range a.Workers {
		if d := filepath.ToSlash(filepath.Clean(w.Dir)); d != "." && inside(lf, d) {
			return fmt.Errorf("config: remote: labels_file %q is inside worker %q's dir %q; that worker would write the labels contacts see — keep it outside every worker dir", lf, w.Name, w.Dir)
		}
	}
	for _, part := range strings.Split(lf, "/") {
		if strings.EqualFold(part, ".lever") || strings.EqualFold(part, state.DirName) {
			return fmt.Errorf("config: remote: labels_file %q is inside %s, a host directory; keep it in the manager's part of the tree", lf, part)
		}
	}
	if a.dir != "" {
		stateDir := filepath.ToSlash(filepath.Join(a.dir, state.DirName))
		if inside(filepath.ToSlash(filepath.Join(a.Tree, lf)), stateDir) {
			return fmt.Errorf("config: remote: labels_file %q is inside the state directory %s, a host directory", lf, stateDir)
		}
	}
	return nil
}

func (a *App) knownAgent(name string) bool {
	return name == a.Name || slices.ContainsFunc(a.Workers, func(w Worker) bool { return w.Name == name })
}

// validateRemoteBind accepts the proxy's listen address only where the jail
// cannot reach it: the proxy believes whatever identity header arrives, so
// anything that can connect is, to the proxy, the front.
//
// Loopback is reached from the jail only through the host alias, and
// validateRemote refuses the proxy port in manager.allow_ports. A
// non-loopback address must be one the jail's egress chain drops in every
// posture (egress.DroppedForJail); under open egress any agent could dial a
// public one and forge the header. The egress chain is not persisted in the
// guest, but apply and up apply it before starting any agent. Everyone else
// is the operator's host firewall, which lever cannot see.
//
// The wildcard is refused unless allow_wildcard_bind acknowledges it. A
// hostname is refused: the address it names can change after the check.
func (a *App) validateRemoteBind() error {
	if a.Remote.AllowWildcardBind && !a.RemoteBindWildcard() {
		return fmt.Errorf("config: remote: allow_wildcard_bind is set, but bind is %q, not 0.0.0.0 or \"::\" — drop it", a.EffectiveRemoteBind())
	}
	b := a.EffectiveRemoteBind()
	ip := net.ParseIP(b)
	switch {
	case ip == nil:
		return fmt.Errorf("config: remote: bind %q must be an IP address (no hostname: the address a name resolves to can change after this check)", b)
	case ip.IsLoopback():
		return nil
	case ip.IsUnspecified():
		if a.EffectiveRemoteIdentityHeader() == DefaultRemoteIdentityHeader {
			// The wildcard listens on the tailnet address too, and no
			// acknowledgement can make a forgeable Tailscale-User-Login safe.
			return fmt.Errorf("config: remote: bind %q listens on every host address, this host's tailnet address included, and "+
				"identity_header is %s: any tailnet peer could connect directly, bypassing `tailscale serve`, and set that header "+
				"to any login. allow_wildcard_bind cannot accept that; keep the loopback bind and use `tailscale serve`", b, DefaultRemoteIdentityHeader)
		}
		if !a.Remote.AllowWildcardBind {
			return fmt.Errorf("config: remote: bind %q listens on every host address, public ones included — anything that reaches "+
				"any of them can set %s to any login, and a public address is reachable from the jail under open egress. Bind "+
				"the one private address the front dials, or set allow_wildcard_bind: true to accept this", b, a.EffectiveRemoteIdentityHeader())
		}
		return nil
	case !egress.DroppedForJail(ip):
		return fmt.Errorf("config: remote: bind %s is not an address the jail's egress rules drop, so a jailed agent could dial "+
			"the proxy directly and set %s to any login; bind a private (RFC 1918, CGNAT, link-local or ULA) address "+
			"the front dials, or 127.0.0.1", b, a.EffectiveRemoteIdentityHeader())
	case ip.To4() == nil && ip.IsLinkLocalUnicast():
		// fe80::/10 is only listenable with a zone ("fe80::1%eth0"), which
		// ParseIP (and so this check) does not accept.
		return fmt.Errorf("config: remote: bind %s is an IPv6 link-local address, which cannot be listened on without an "+
			"interface zone; bind a ULA, RFC 1918 or CGNAT address instead", b)
	case a.EffectiveRemoteIdentityHeader() == DefaultRemoteIdentityHeader && inTailnetRange(ip):
		// Any tailnet peer could connect directly, not through `tailscale
		// serve`, which is what sets and overwrites the header.
		return fmt.Errorf("config: remote: bind %s is a tailnet address, and identity_header is %s: any tailnet peer could connect "+
			"to it directly and set that header to any login. Keep the default loopback bind and publish it with "+
			"`tailscale serve`, which sets the header", b, DefaultRemoteIdentityHeader)
	}
	return nil
}

// tailnetRanges are the ranges Tailscale assigns node addresses in.
var tailnetRanges = []string{"100.64.0.0/10", "fd7a:115c:a1e0::/48"}

func inTailnetRange(ip net.IP) bool {
	for _, c := range tailnetRanges {
		if _, n, err := net.ParseCIDR(c); err == nil && n.Contains(ip) {
			return true
		}
	}
	return false
}

// refusedIdentityHeaders are headers the proxy must never take a login from:
// each is set by the browser or by any hop rather than overwritten by the
// front alone, or is one scion's own auth reads, so trusting it would let a
// browser or an appending hop choose the identity.
var refusedIdentityHeaders = []string{
	// Tailscale's display name and avatar: set by `tailscale serve`, but
	// chosen by the user and not unique, so never an identity.
	"Tailscale-User-Name", "Tailscale-User-Profile-Pic",
	"Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie",
	"Host", "Origin", "Referer", "Forwarded", "Via",
	"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Forwarded-Port", "X-Real-Ip",
	"Connection", "Keep-Alive", "Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
	"Content-Length", "Content-Type", "User-Agent",
	"X-Api-Key", "X-Goog-Iap-Jwt-Assertion",
}

// refusedIdentityHeaderPrefixes: Sec-* is browser fetch metadata; X-Scion-*
// and X-Forwarded-User-* are scion's identity headers.
var refusedIdentityHeaderPrefixes = []string{"Sec-", "X-Scion-", "X-Forwarded-User-", "Access-Control-"}

// validRemoteIdentityHeader takes h in canonical form.
func validRemoteIdentityHeader(h string) error {
	if h == "" {
		return fmt.Errorf("config: remote: identity_header is empty")
	}
	for i := 0; i < len(h); i++ {
		if !isHeaderTokenChar(h[i]) {
			return fmt.Errorf("config: remote: identity_header %q is not a valid HTTP header name", h)
		}
	}
	if strings.Contains(h, "_") {
		// Some proxies map "_" to "-", so a client could send X_Login and
		// have a hop deliver it as X-Login next to the front's.
		return fmt.Errorf("config: remote: identity_header %q contains \"_\"; proxies drop or rewrite such headers inconsistently, "+
			"so a client-sent variant could reach the proxy as the front's header — use \"-\"", h)
	}
	for _, r := range refusedIdentityHeaders {
		if strings.EqualFold(h, r) {
			return fmt.Errorf("config: remote: identity_header %q is refused: it carries a browser credential, routing, a "+
				"user-chosen display value, or an identity the proxy strips — not a unique login only the front sets", h)
		}
	}
	for _, p := range refusedIdentityHeaderPrefixes {
		if len(h) >= len(p) && strings.EqualFold(h[:len(p)], p) {
			return fmt.Errorf("config: remote: identity_header %q is refused: %s* headers are set by the browser or read by "+
				"scion as an identity, never a login only the front sets", h, p)
		}
	}
	return nil
}

// isHeaderTokenChar: RFC 9110 tchar.
func isHeaderTokenChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}

// remoteIDEmailDomain is the domain remoteproxy.identityFor gives a login
// with no "@" so that it becomes a hub email; remoteproxy_test pins that the
// constants agree.
const (
	remoteIDEmailDomain      = "id.lever.local"
	remoteUnnamedOperatorKey = "lever-operator@lever.local"
)

// userIDChars are what a login without "@" may carry: it becomes the local
// part of a synthesized hub email, and must stay one without quoting.
var userIDChars = regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)

// validRemoteLogin: an entry is compared exactly with the front's header
// value and becomes a hub user's email, so it must be one login. The proxy
// refuses a comma-joined header value outright, so such an entry could never
// match.
func validRemoteLogin(l string) error {
	if strings.TrimSpace(l) == "" {
		return fmt.Errorf("config: remote: allowed_users contains an empty entry")
	}
	for _, r := range l {
		if r == ',' || r <= ' ' || r == 0x7f {
			return fmt.Errorf("config: remote: allowed_users entry %q must be a single login: no commas, whitespace or control characters", l)
		}
	}
	local, domain, hasAt := strings.Cut(l, "@")
	switch {
	case !hasAt && !userIDChars.MatchString(local):
		return fmt.Errorf("config: remote: allowed_users entry %q has no \"@\", so it becomes the hub email %q; "+
			"use only letters, digits and . _ + - in it", l, l+"@"+remoteIDEmailDomain)
	case hasAt && (strings.EqualFold(domain, remoteIDEmailDomain) || strings.EqualFold(l, remoteUnnamedOperatorKey)):
		// Listing one of lever's synthesized addresses would make two
		// different logins the same hub user.
		return fmt.Errorf("config: remote: allowed_users entry %q is an address lever synthesizes for other logins; list the login itself", l)
	case hubauth.IsReserved(HubEmailFor(l)):
		// A login that became one of these hub users would act as lever
		// itself (admin, or the controller every message is sent as) or as
		// scion's dev user.
		return fmt.Errorf("config: remote: allowed_users entry %q is reserved for lever's own hub identities "+
			"(%s, %s) or scion's dev user (any @localhost address); list another login", l, hubauth.AdminEmail, hubauth.ControllerEmail)
	}
	return nil
}

// validateBroker fails closed on capability-config mistakes. A worker with no
// grants is fine: default deny.
func (a *App) validateBroker() error {
	if !a.Broker.LLMAuth.valid() {
		return fmt.Errorf("config: broker.llm_auth %q invalid (want subscription|api-key)", a.Broker.LLMAuth)
	}
	if !a.Manager.LLMAuth.valid() {
		return fmt.Errorf("config: manager.llm_auth %q invalid (want subscription|api-key)", a.Manager.LLMAuth)
	}
	for _, g := range a.Workers {
		if !g.LLMAuth.valid() {
			return fmt.Errorf("config: worker %s llm_auth %q invalid (want subscription|api-key)", g.Name, g.LLMAuth)
		}
	}
	// Mixed instances are unsupported: the OAuth credential is one jail-wide
	// hub secret and egress is jail-wide, so a subscription agent would put
	// the real token and open egress into the api-key agents' containers.
	// Resolved modes are compared, so a default that disagrees with a worker
	// override is caught too.
	if a.mixedLLMAuth() {
		return fmt.Errorf("config: llm_auth is mixed across the instance (both api-key and subscription agents) — this is unsupported; an instance must be uniformly api-key or uniformly subscription (see security-model-credentials.md §6.1)")
	}
	if !a.Egress.valid() {
		return fmt.Errorf("config: egress %q invalid (want open|closed)", a.Egress)
	}
	if a.Egress == EgressClosed {
		if _, anySub := a.llmAuthModes(); anySub {
			return fmt.Errorf("config: egress: closed requires every agent to be api-key (a subscription agent needs direct internet to reach Anthropic)")
		}
	}
	if a.AnyAPIKeyAgent() {
		if a.Broker.APIKeyFile == "" {
			return fmt.Errorf("config: broker.api_key_file is required when llm_auth is api-key")
		}
	}
	return a.validateBrokerGrants()
}

func (a *App) validateBrokerGrants() error {
	toolOps := map[string]map[string]bool{}
	// llm is a reserved pseudo-tool: the broker's /llm proxy.
	toolOps[wire.ReservedLLMTool] = map[string]bool{wire.ReservedLLMOp: true}
	for _, t := range a.Broker.Tools {
		if t.Name == "" {
			return fmt.Errorf("config: broker.tools entry has empty name")
		}
		if !toolNameRE.MatchString(t.Name) {
			return fmt.Errorf("config: broker tool name %q must match %s (it becomes the /mcp/%s/ tool route and a claude mcp add token)", t.Name, toolNameRE, t.Name)
		}
		if _, dup := toolOps[t.Name]; dup {
			return fmt.Errorf("config: duplicate broker tool %q", t.Name)
		}
		if err := t.validate(); err != nil {
			return err
		}
		ops := map[string]bool{}
		if t.External && t.EffectiveGate() == GateCoarse {
			ops[wire.WildcardOp] = true
		}
		for _, o := range t.Operations {
			ops[o.Name] = true
		}
		toolOps[t.Name] = ops
	}
	agents := map[string]bool{a.ManagerCN(): true}
	for _, g := range a.Workers {
		agents[g.Name] = true
	}
	checkCap := func(who, tool, op string) error {
		ops, ok := toolOps[tool]
		if !ok {
			return fmt.Errorf("config: %s grants tool %q which is not a declared broker.tool", who, tool)
		}
		if !ops[op] {
			if op == wire.WildcardOp {
				return fmt.Errorf("config: %s grants op %q on tool %q, but the wildcard is honored only for a gate: coarse external tool", who, op, tool)
			}
			return fmt.Errorf("config: %s grants %q on tool %q which has no such operation", who, op, tool)
		}
		return nil
	}
	checkAgentGrants := func(who string, obtain []Grant, delegate []DelegateGrant) error {
		for _, g := range obtain {
			if err := checkCap(who+".obtain", g.Tool, g.Op); err != nil {
				return err
			}
		}
		for _, d := range delegate {
			if err := checkCap(who+".delegate", d.Tool, d.Op); err != nil {
				return err
			}
			for _, to := range d.To {
				if !agents[to] {
					return fmt.Errorf("config: %s delegates to %q which is not a declared agent", who, to)
				}
			}
		}
		return nil
	}
	if err := checkAgentGrants("manager", a.Manager.Obtain, a.Manager.Delegate); err != nil {
		return err
	}
	for _, g := range a.Workers {
		if err := checkAgentGrants("worker "+g.Name, g.Obtain, g.Delegate); err != nil {
			return err
		}
	}
	return nil
}

// validateImageTar: the archive is host-only boot material, root-confined
// like prompt_file, and is matched by tag, so it needs an image that is not
// digest-pinned.
func validateImageTar(who, image, tar string) error {
	if tar == "" {
		return nil
	}
	if !confinedRel(tar) {
		return fmt.Errorf("config: %s.image_tar %q must be a relative path inside the instance root (no \"..\", not absolute)", who, tar)
	}
	if image == "" {
		return fmt.Errorf("config: %s.image_tar %q needs %s.image — the archive is matched against the image's tag (a worker with no image inherits the manager's image and its tar)", who, tar, who)
	}
	if strings.Contains(image, "@") {
		return fmt.Errorf("config: %s.image_tar %q cannot ship a digest-pinned image (%s): a docker archive is matched by tag, not by manifest digest", who, tar, image)
	}
	return nil
}

// validateImageTarsDistinct: bring-up loads each distinct ref once, so a
// second archive for the same ref would be silently ignored.
func (a *App) validateImageTarsDistinct() error {
	src := map[string]string{}
	check := func(ref, tar string) error {
		if ref == "" || tar == "" {
			return nil
		}
		if prev, ok := src[ref]; ok && prev != tar {
			return fmt.Errorf("config: image %q is shipped from two archives (%q and %q); one image ref must come from one image_tar", ref, prev, tar)
		}
		src[ref] = tar
		return nil
	}
	if err := check(a.ManagerImage(), a.Manager.ImageTar); err != nil {
		return err
	}
	for _, g := range a.Workers {
		if g.Image == "" {
			continue
		}
		if err := check(a.WorkerImage(g), g.ImageTar); err != nil {
			return err
		}
	}
	return nil
}

package scion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/retry"
)

// ContainerLive reports whether a scion containerStatus value describes a live
// container. For a running container scion passes through the podman status
// text ("Up 6 seconds"), not a canonical token; non-live values include
// "stopped" and "Exited (1) 4 minutes ago".
func ContainerLive(status string) bool {
	return status == "running" || strings.HasPrefix(status, "Up")
}

// RunningContainerDown reports a record the hub still calls running while
// its container is not live: until the hub's own sweep marks it error, a
// message to it reaches nothing. A blank status ("cannot tell") and
// "created", which scion reports briefly for a running record, are not down.
func RunningContainerDown(phase, containerStatus string) bool {
	return phase == PhaseRunning && containerStatus != "" && !ContainerLive(containerStatus) &&
		!strings.EqualFold(containerStatus, "created")
}

// LiveBudget bounds WaitAgentLive. Attempts × Interval is how long a record may
// take to become live at all; Settle is how long it must then stay live (0
// returns on the first live observation).
type LiveBudget struct {
	Attempts int
	Interval time.Duration
	Settle   time.Duration
}

// WaitAgentLive polls list until the agent named slug shows both
// Phase=="running" and a live container, or attempts run out; then, when the
// budget has a Settle window, requires that pair to hold for the whole
// window. It backstops scion's false successes (a 409 "already exists", a
// "resumed" whose harness dies moments later): only the observed record is
// trusted, never CLI exit codes or wording.
//
// The settle window exists because both signals appear before the harness has
// run a line, so a harness that dies seconds in would pass a first-observation
// gate. It is still probabilistic: a death just after Settle passes.
//
// A mid-poll list error is consumed within the same budget and polling
// continues; it is surfaced only if the budget exhausts. In the settle window
// errors are skipped too, but a window with no successful observation is
// reported (ErrAgentUnverified), not passed.
//
// On exhaustion it returns an ErrAgentNotLive error with no subject prefix
// (the caller adds one). A record that stops being live during the window
// returns ErrAgentDied. Context cancellation returns ctx.Err() unwrapped.
// Attempts <= 0 is an exhausted budget, not "poll forever".
func WaitAgentLive(ctx context.Context, list func(context.Context) ([]Agent, error), slug string, b LiveBudget) error {
	return waitAgentLive(ctx, list, slug, b, wallClock)
}

// waitAgentLive is WaitAgentLive with the settle window's clock injected.
func waitAgentLive(ctx context.Context, list func(context.Context) ([]Agent, error), slug string, b LiveBudget, clk settleClock) error {
	if err := waitLiveOnce(ctx, list, slug, b.Attempts, b.Interval); err != nil {
		return err
	}
	if b.Settle <= 0 {
		return nil
	}
	return holdLive(ctx, list, slug, b.Interval, b.Settle, clk)
}

// settleClock lets tests substitute a clock on which every wait returns at
// once and advances by exactly the interval, so an outcome never depends on
// scheduler latency.
type settleClock struct {
	now   func() time.Time
	after func(time.Duration) <-chan time.Time
}

var wallClock = settleClock{now: time.Now, after: time.After}

// HoldAgentLive is WaitAgentLive's settle phase on its own, for a caller that
// puts its own check between becoming live and the hold (WaitAgentLive with a
// zero Settle, its check, then this). settle <= 0 returns nil at once.
func HoldAgentLive(ctx context.Context, list func(context.Context) ([]Agent, error), slug string, interval, settle time.Duration) error {
	if settle <= 0 {
		return nil
	}
	return holdLive(ctx, list, slug, interval, settle, wallClock)
}

func waitLiveOnce(ctx context.Context, list func(context.Context) ([]Agent, error), slug string, attempts int, interval time.Duration) error {
	var lastPhase, lastContainer string
	var lastErr error
	if attempts <= 0 {
		return notLiveErr(lastPhase, lastContainer)
	}
	err := retry.Until(ctx, attempts, interval, func() (bool, error) {
		agents, err := list(ctx)
		if err != nil {
			lastErr = err
			return false, nil
		}
		lastErr = nil
		// Reset first so a record that vanished mid-poll reports "", not a
		// stale earlier phase.
		lastPhase, lastContainer = "", ""
		if a := FindAgent(agents, slug); a != nil {
			lastPhase, lastContainer = a.Phase, a.ContainerStatus
		}
		return lastPhase == "running" && ContainerLive(lastContainer), nil
	})
	if !errors.Is(err, retry.ErrExhausted) {
		return err
	}
	if lastErr != nil {
		// %v, not %w: a list error may carry an inner context deadline while
		// our ctx stayed live, and wrapping it would make exhaustion read as
		// cancellation.
		return fmt.Errorf("%w (last error observing agents: %v)", ErrAgentNotLive, lastErr)
	}
	return notLiveErr(lastPhase, lastContainer)
}

// holdLive observes every interval until settle has elapsed since the record
// was first seen live.
//
// Two strikes, not one: the hub's container column is refreshed by the
// runtime broker's heartbeat, so a single reading can be stale. A non-live
// observation counts only when the next successful one agrees; a real death
// is stable across it. A pending strike keeps the window open for a bounded
// few polls past its deadline; if no observation succeeds (a dying VM), the
// window ends as unverified. A list error between two non-live readings does
// not break the chain.
func holdLive(ctx context.Context, list func(context.Context) ([]Agent, error), slug string, interval, settle time.Duration, clk settleClock) error {
	if interval <= 0 {
		// Never spin on a zero interval.
		interval = time.Second
	}
	// strikeGrace bounds how many polls past the deadline a pending strike may
	// wait for a deciding observation.
	const strikeGrace = 3
	start := clk.now()
	var lastErr error
	var lastPhase, lastContainer string
	observed := false
	strikes, graceLeft := 0, strikeGrace
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-clk.after(interval):
		}
		agents, err := list(ctx)
		if err != nil {
			lastErr = err
		} else {
			lastErr, observed = nil, true
			lastPhase, lastContainer = "", ""
			if a := FindAgent(agents, slug); a != nil {
				lastPhase, lastContainer = a.Phase, a.ContainerStatus
			}
			switch {
			case lastPhase == "running" && ContainerLive(lastContainer):
				strikes = 0
			case lastPhase == "running" && lastContainer == "":
				// A blank container column is "cannot tell": strikes stay as
				// they are, like a list error.
			default:
				if strikes++; strikes >= 2 {
					return fmt.Errorf("%w after %s (%s) — scion reported success and the record was live, then the harness died; its container log in the guest holds the last output",
						ErrAgentDied, clk.now().Sub(start).Round(100*time.Millisecond), describeReading(lastPhase, lastContainer))
				}
			}
		}
		if clk.now().Sub(start) >= settle {
			if strikes == 0 {
				break
			}
			if graceLeft--; graceLeft <= 0 {
				return fmt.Errorf("%w during the %s settle window (a non-live reading — %s — was never confirmed or refuted; last error observing agents: %v); run `lever doctor` for the manager row",
					ErrAgentUnverified, settle, describeReading(lastPhase, lastContainer), lastErr)
			}
		}
	}
	if !observed {
		return fmt.Errorf("%w during the %s settle window (no observation succeeded; last error observing agents: %v); run `lever doctor` for the manager row", ErrAgentUnverified, settle, lastErr)
	}
	return nil
}

func describeReading(phase, container string) string {
	if phase == "" && container == "" {
		return "record gone from the listing"
	}
	return fmt.Sprintf("phase %q, container %q", PhaseLabel(phase), ContainerLabel(container))
}

// ErrAgentNotLive is wrapped by WaitAgentLive when its budget exhausts without
// observing a live record; the message carries the last observation.
var ErrAgentNotLive = errors.New("did not come up")

// ErrAgentDied is wrapped by WaitAgentLive when a record that was live stops
// being so inside the settle window. Distinct from ErrAgentNotLive: this case
// has a container log worth reading.
var ErrAgentDied = errors.New("came up, then died")

// ErrAgentUnverified is wrapped by WaitAgentLive when the record was seen
// live once but no observation succeeded for the rest of the settle window:
// nothing contradicted it, and nothing confirmed it either. Not a pass.
var ErrAgentUnverified = errors.New("could not be verified live")

// NotLiveError is WaitAgentLive's exhaustion error when the record was
// observed but never live, carrying the last observation. errors.Is(err,
// ErrAgentNotLive) holds for it.
type NotLiveError struct {
	Phase, Container string
}

func (e *NotLiveError) Error() string {
	// Labels, not the raw strings: this error reaches the manager, and both
	// fields are text an agent can set for itself on the hub.
	return fmt.Sprintf("%v (last phase %q, container %q) — scion reported success but the harness is not live", ErrAgentNotLive, PhaseLabel(e.Phase), ContainerLabel(e.Container))
}

func (e *NotLiveError) Is(target error) bool { return target == ErrAgentNotLive }

func notLiveErr(lastPhase, lastContainer string) error {
	return &NotLiveError{Phase: lastPhase, Container: lastContainer}
}

// ObserveAgent returns the record for slug as scion lists it now, retrying a
// failed list up to attempts times. Absent is (nil, nil). It judges nothing.
// Exhaustion reports the last list error unwrapped (see waitLiveOnce); ctx
// errors pass through.
func ObserveAgent(ctx context.Context, list func(context.Context) ([]Agent, error), slug string, attempts int, interval time.Duration) (*Agent, error) {
	if attempts <= 0 {
		attempts = 1
	}
	var lastErr error
	var found *Agent
	err := retry.Until(ctx, attempts, interval, func() (bool, error) {
		agents, err := list(ctx)
		if err != nil {
			lastErr = err
			return false, nil
		}
		if a := FindAgent(agents, slug); a != nil {
			copied := *a
			found = &copied
		}
		return true, nil
	})
	if errors.Is(err, retry.ErrExhausted) {
		return nil, fmt.Errorf("observing agents: %v", lastErr)
	}
	if err != nil {
		return nil, err
	}
	return found, nil
}

// FindAgent returns a pointer to the first agent whose Slug matches, or nil.
// The pointer aliases the slice element.
func FindAgent(agents []Agent, slug string) *Agent {
	for i := range agents {
		if agents[i].Slug == slug {
			return &agents[i]
		}
	}
	return nil
}

type Agent struct {
	Slug     string `json:"slug"`
	Phase    string `json:"phase"`
	Activity string `json:"activity"`
	// ContainerStatus backstops liveness checks: scion's "resumed" or exit 0
	// can lie, so Phase alone is not trusted.
	ContainerStatus string `json:"containerStatus"`
	// Image is the container image the record was created with; a resume
	// redispatches it unchanged, so it can drift from the config.
	Image string `json:"image"`
	// ContainerID is "" when no container is running.
	ContainerID string `json:"containerId"`
	// LastActivityEvent is when Activity last changed (zero when the hub has
	// not reported one).
	LastActivityEvent time.Time `json:"lastActivityEvent,omitempty"`
}

// Activity values for Agent.Activity, the harness-level state scion's Claude
// Code hooks report to the hub (pkg/agent/state/state.go). The hub's sweeper
// sets stalled when no activity event arrives within its stalled_threshold,
// both for a harness that cannot reach the model and for one idle at its
// prompt, so stalled alone does not tell the two apart.
const (
	ActivityWorking         = "working"
	ActivityWaitingForInput = "waiting_for_input"
	ActivityCompleted       = "completed"
	ActivityStalled         = "stalled"
	ActivityOffline         = "offline"
	ActivityCrashed         = "crashed"
)

// ActivityDead reports an activity the hub uses for a harness that has
// stopped turning: stalled, crashed or offline.
func ActivityDead(activity string) bool {
	switch activity {
	case ActivityStalled, ActivityCrashed, ActivityOffline:
		return true
	}
	return false
}

// LabelUnrecognised stands in for a phase or an activity lever does not know.
const LabelUnrecognised = "unrecognised"

// PhaseLabel is phase when it is one of scion's phases (or the CLI's interim
// "resumed"), "" when empty, else LabelUnrecognised. An agent can post any
// text as its own phase, so everything that prints a phase to another agent
// or acts on it by name must go through here; what no label matches is never
// echoed.
func PhaseLabel(phase string) string {
	switch phase {
	case "", "created", "provisioning", "cloning", "starting", PhaseRunning,
		PhaseSuspended, "stopping", PhaseStopped, PhaseError, "resumed":
		return phase
	}
	return LabelUnrecognised
}

// containerAge and containerStatusRE match only the container status texts
// podman and scion produce. The age is a number and a unit, never free words:
// an agent can post this field about itself, so no slot may hold a sentence.
const containerAge = `(\d{1,4} (second|minute|hour|day|week|month|year)s?|About an? (minute|hour)|Less than a second)`

var containerStatusRE = regexp.MustCompile(`^(running|stopped|created|Created|paused|Paused|Up ` + containerAge + `|Exited \(\d{1,3}\) ` + containerAge + ` ago)$`)

// ContainerLabel is status when it has one of the shapes a runtime reports,
// "" when empty, else LabelUnrecognised (or "up" for a text that only starts
// like a live status). An agent can post its own containerStatus, so the text
// is shown only when it is plainly a status. For display only: ContainerLive
// decides liveness from the raw text.
func ContainerLabel(status string) string {
	switch {
	case status == "", containerStatusRE.MatchString(status):
		return status
	case ContainerLive(status):
		return "up"
	}
	return LabelUnrecognised
}

// BoundedQuote renders a hub-reported string for the OPERATOR: quoted, and cut
// at 48 bytes. The operator may see an odd value (it helps a diagnosis), but
// never a wall of agent-chosen text inside a lever message.
func BoundedQuote(s string) string {
	const limit = 48
	if len(s) > limit {
		s = strings.ToValidUTF8(s[:limit], "") + "…"
	}
	return strconv.Quote(s)
}

// ActivityLabel is the same for an activity (also agent-reported).
func ActivityLabel(activity string) string {
	switch activity {
	case "", ActivityWorking, "thinking", "executing", ActivityWaitingForInput, "blocked",
		ActivityCompleted, "limits_exceeded", ActivityStalled, ActivityOffline, ActivityCrashed:
		return activity
	}
	return LabelUnrecognised
}

// Phase values for Agent.Phase, mirroring scion's agent-state vocabulary
// (pkg/agent/state/state.go). Only the phases lever compares against are
// listed.
const (
	PhaseRunning   = "running"
	PhaseSuspended = "suspended"
	PhaseStopped   = "stopped"
	PhaseError     = "error"
)

// agentRoleBaseline is the role lever wants for every agent: heartbeat and
// self-token-refresh, with no agent create/lifecycle or secret scope. Worker
// dispatch runs host-side under the controller PAT, so nothing needs more.
const agentRoleBaseline = "baseline"

// EffectiveAgentRole is the role lever stamps on every agent it creates and
// the ceiling it asks the hub to enforce on the project: the configured
// scion.agent_role, else baseline. The stamp and the ceiling must not
// disagree.
func EffectiveAgentRole(configured string) string {
	if configured != "" {
		return configured
	}
	return agentRoleBaseline
}

// RolesSupported reports whether the installed scion accepts start --role.
// hubapi.VerifyAgentRole also needs it to judge an existing record's empty
// stored role.
//
// It asks the binary rather than the pin: too eager breaks scions without
// roles, too shy hands agents full authority on scions whose default role is
// full.
//
// The answer is deliberately not cached: scion.source and scion.binary are
// paths, so the artifact can change under a long-lived broker without a
// config change, and one stale "no --role" would disarm both the --role stamp
// and VerifyAgentRole silently. The probe is one local scion start --help.
func (c *Client) RolesSupported(ctx context.Context) (bool, error) {
	out, err := c.run(ctx, "", "start", "--help")
	if err != nil {
		return false, err
	}
	return strings.Contains(out, "--role"), nil
}

type StartOpts struct {
	Worker  string
	Task    string
	Harness string // default "claude"
	Project string
	Image   string // optional
	// Model is the LLM the agent runs on (scion start --model): a scion alias
	// or an explicit model ID. Empty emits no flag, so scion resolves it.
	//
	// Create-time only: scion resume has no --model, so a changed model
	// reaches an agent only on a fresh create.
	Model string
	// Instructions is the agent's standing instructions text, sent as scion
	// inline config agent_instructions on stdin, never on argv, and bound by
	// MaxInstructionsBytes. The harness projects it into the agent's
	// user-level ~/.claude/CLAUDE.md on every container start. Empty sends no
	// config at all.
	//
	// Create-time only: scion does not re-stage on resume, but it does
	// re-project the staged copy, so an agent that rewrites its own managed
	// block gets the staged text back at its next start.
	Instructions string
	// Volumes are extra bind mounts for the agent container, sent as inline
	// config. The broker mounts the worker's enrolment-ticket directory
	// read-only at /run/lever, so the ticket is visible to that container
	// alone, never to the manager's whole-tree mount. Create-time only.
	Volumes []VolumeMount
	// Env is extra container environment, sent as inline config.
	// Create-time only.
	Env map[string]string
	// Resources caps the agent container's memory and CPU, sent as inline
	// config resources.limits. The zero value sends nothing. Create-time
	// only.
	Resources Resources
	// Workspace is the path mounted as /workspace (--workspace). For a
	// directory project it must be set to the in-jail project tree to get a
	// live bind mount; scion's default mounts a managed copy instead. Empty
	// leaves scion to resolve it.
	Workspace string
	// WorkspaceSubdir, when set, is a path relative to the project's
	// workspace_path, passed as a relative --workspace. scion resolves it
	// against the project root with a containment guard against .. and
	// symlink escape, confining a worker to its own subdir. Mutually exclusive
	// with Workspace. Requires scion >= b4c9911d.
	WorkspaceSubdir string
	// APIKey selects --harness-auth api-key, satisfied by a placeholder
	// ANTHROPIC_API_KEY Hub secret. The placeholder is not a credential: it
	// exists only because scion's start-time auth gate requires one. The real
	// credential is the broker /llm capability token, which the broker
	// verifies before overwriting x-api-key with the real key host-side.
	APIKey bool
}

// List lists the agents in project. Empty stdout is an empty slice.
func (c *Client) List(ctx context.Context, project string) ([]Agent, error) {
	args := append([]string{"list", "--format", "json"}, projectFlag(project)...)
	args = append(args, nonInteractive)
	// runValue: stderr warnings after the JSON would break the parse.
	out, err := c.runValue(ctx, "", args...)
	if err != nil {
		return nil, err
	}
	var agents []Agent
	if err := parseJSON(out, &agents); err != nil {
		return nil, err
	}
	return agents, nil
}

// ErrDeleteUnconfirmed is wrapped by Delete when scion exits 0 for a delete the
// hub accepted (202) but whose completion scion's own wait (about three
// minutes) never saw: the record may still be there, or still be deleting.
var ErrDeleteUnconfirmed = errors.New("the hub accepted the delete but scion could not confirm the record is gone")

// deleteAcceptedNotice is scion delete's line for that case
// (cmd/hub_delete_wait.go hubDeletePendingMessage).
const deleteAcceptedNotice = "delete accepted; cannot observe completion"

// Delete removes an agent's record entirely, with its containers and files. It
// fails with ErrDeleteUnconfirmed rather than report a delete nobody saw
// finish; a caller that must know re-lists either way.
func (c *Client) Delete(ctx context.Context, worker, project string) error {
	out, err := c.run(ctx, "", agentVerb("delete", worker, project)...)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, deleteAcceptedNotice) {
			return fmt.Errorf("scion delete %s: %w: %s", worker, ErrDeleteUnconfirmed, boundSummary(strings.TrimSpace(line), 300))
		}
	}
	return nil
}

func (c *Client) Start(ctx context.Context, o StartOpts) error {
	// Backstop for every caller, before any scion call.
	if err := CheckTask(o.Task); err != nil {
		return err
	}
	if err := CheckInstructions(o.Instructions); err != nil {
		return err
	}
	// Pin the agent role whenever the installed scion understands roles. An
	// older scion rejects --role, while a newer one defaults to full, so not
	// passing it there hands every agent create/lifecycle/secret-read
	// authority. Omitting it on a scion without roles widens nothing.
	supported, err := c.RolesSupported(ctx)
	role := ""
	switch {
	case err != nil:
		// Fail closed: guessing risks silently granting full authority.
		return fmt.Errorf("determining scion agent-role support: %w", err)
	case supported:
		role = EffectiveAgentRole(c.agentRole)
	case c.agentRole != "":
		return fmt.Errorf("scion.agent_role is %q but this scion has no --role flag "+
			"(it predates scion#1089); remove the setting or move to a newer pin", c.agentRole)
	}
	args := startArgs(o, role)
	if !o.hasInlineConfig() {
		_, runErr := c.run(ctx, "", args...)
		return runErr
	}
	// JSON, not YAML: scion picks the JSON parser on a leading brace, and
	// json.Marshal escapes anything a manual can contain. Inline content, not
	// a file:// URI, so nothing depends on scion's working directory.
	body, err := json.Marshal(inlineAgentConfig{AgentInstructions: o.Instructions, Volumes: o.Volumes, Env: o.Env, Resources: o.Resources.spec()})
	if err != nil {
		return fmt.Errorf("encoding inline agent config: %w", err)
	}
	_, runErr := c.runStdin(ctx, bytes.NewReader(body), args...)
	return runErr
}

// startArgs builds the scion start argv for o; role "" omits --role.
//
// Every flag comes first and both positionals (worker name, then task) sit
// behind --: both are chosen by an untrusted caller, and scion parses
// interspersed flags, so a task like --config=<path> would author the
// worker's inline config (volumes, env, services). CheckTask is the second
// layer.
func startArgs(o StartOpts, role string) []string {
	harness := o.Harness
	if harness == "" {
		harness = "claude"
	}
	args := projectFlag(o.Project)
	args = append(args, "start", "--harness", harness)
	if o.APIKey {
		// See StartOpts.APIKey.
		args = append(args, "--harness-auth", "api-key")
	} else {
		args = append(args, "--harness-auth", "oauth-token")
	}
	if role != "" {
		args = append(args, "--role", role)
	}
	if o.Image != "" {
		args = append(args, "--image", o.Image)
	}
	if o.Model != "" {
		// An empty --model would replace scion's default with an invalid value.
		args = append(args, "--model", o.Model)
	}
	if o.WorkspaceSubdir != "" {
		// Same flag as Workspace: scion branches on filepath.IsAbs.
		args = append(args, "--workspace", o.WorkspaceSubdir)
	} else if o.Workspace != "" {
		args = append(args, "--workspace", o.Workspace)
	}
	if o.hasInlineConfig() {
		args = append(args, "--config", "-")
	}
	return append(args, nonInteractive, "--", o.Worker, o.Task)
}

func (o StartOpts) hasInlineConfig() bool {
	return o.Instructions != "" || len(o.Volumes) > 0 || len(o.Env) > 0 || o.Resources != (Resources{})
}

// Resources is an agent's container limits in scion's own notation: Memory a
// Kubernetes quantity ("1536Mi", "2Gi"), CPU in cores ("1.5") or millicores
// ("500m"). Config validation bounds them.
type Resources struct {
	Memory string
	CPU    string
}

type resourceSpec struct {
	Limits resourceList `json:"limits"`
}

type resourceList struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

func (r Resources) spec() *resourceSpec {
	if r == (Resources{}) {
		return nil
	}
	return &resourceSpec{Limits: resourceList{CPU: r.CPU, Memory: r.Memory}}
}

// ContainerWorkspace is where scion mounts an agent's workspace (the
// --workspace path) inside its container, and the container's CWD.
const ContainerWorkspace = "/workspace"

// VolumeMount is one container bind mount in scion's inline-config shape:
// source is a guest path, target the path inside the container.
type VolumeMount struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// inlineAgentConfig is the subset of scion's inline agent config lever sends.
// Any other field would silently override the template chain. Every field is
// omitempty: scion reads a present empty agent_instructions as
// inline-provided and would skip the template's instructions.
type inlineAgentConfig struct {
	AgentInstructions string            `json:"agent_instructions,omitempty"`
	Volumes           []VolumeMount     `json:"volumes,omitempty"`
	Env               map[string]string `json:"env,omitempty"`
	Resources         *resourceSpec     `json:"resources,omitempty"`
}

func (c *Client) Resume(ctx context.Context, worker, project string) error {
	_, err := c.run(ctx, "", agentVerb("resume", worker, project)...)
	return err
}

// ResumeTask resumes a stopped worker in a fresh session that runs task, and
// the hub stores task as the record's task for later fresh starts. On a
// suspended record the hub would instead add task to the continued session,
// so the caller must have seen the record stopped and hold off every other
// lifecycle change until this returns.
//
// Both positionals sit behind --, as in startArgs: scion parses interspersed
// flags.
func (c *Client) ResumeTask(ctx context.Context, worker, project, task string) error {
	if task == "" {
		return errors.New("scion resume: empty task (use Resume)")
	}
	if err := CheckTask(task); err != nil {
		return err
	}
	args := append([]string{"resume"}, projectFlag(project)...)
	_, err := c.run(ctx, "", append(args, nonInteractive, "--", worker, task)...)
	return err
}

// agentVerb is the argv of a scion verb on one agent, with extra flags.
func agentVerb(verb, agent, project string, flags ...string) []string {
	args := append([]string{verb, agent}, flags...)
	args = append(args, projectFlag(project)...)
	return append(args, nonInteractive)
}

// ResumeForce is Resume with scion's --force (scion >= 68507153), the only
// verb that recovers a record from phase "error"; it refuses phase
// "running".
func (c *Client) ResumeForce(ctx context.Context, worker, project string) error {
	_, err := c.run(ctx, "", agentVerb("resume", worker, project, "--force")...)
	return err
}

func (c *Client) Stop(ctx context.Context, worker, project string) error {
	_, err := c.run(ctx, "", agentVerb("stop", worker, project)...)
	return err
}
func (c *Client) Suspend(ctx context.Context, worker, project string) error {
	_, err := c.run(ctx, "", agentVerb("suspend", worker, project)...)
	return err
}

// ResetAuth gives a running agent a new hub token without a restart (scion
// reset-auth). The hub mints it from the agent's stored role and scopes, so
// no user or controller authority moves into the container. It is the one
// recovery for an expired agent token, which cannot refresh itself.
func (c *Client) ResetAuth(ctx context.Context, agent, project string) error {
	_, err := c.run(ctx, "", agentVerb("reset-auth", agent, project)...)
	return err
}

// AttachArgv returns the in-jail argv to attach interactively. The caller
// execs it to hand over the TTY, bypassing env().
//
// It pins the endpoint with an env SCION_HUB_ENDPOINT= prefix, or scion would
// fall back to the endpoint persisted in the jail's project config, which can
// name another port (a link made against a throwaway hub). It must not embed the
// controller PAT: the argv lands on a host command line readable via ps. The
// caller stages the token in the guest instead (jail.StageHubToken).
func (c *Client) AttachArgv(worker, project string) []string {
	argv := append([]string{c.bin, "attach", worker}, projectFlag(project)...)
	if c.hubEndpoint != "" {
		argv = append([]string{"env", "SCION_HUB_ENDPOINT=" + c.hubEndpoint}, argv...)
	}
	return argv
}

// HubToken is the controller PAT this client authenticates with ("" when none
// is set), for the attach path, which cannot use env().
func (c *Client) HubToken() string { return c.currentHubToken() }

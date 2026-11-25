package host

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/convbackup"
	"github.com/stevegeek/lever/internal/hostledger"
	"github.com/stevegeek/lever/internal/wire"
)

// `lever agent remote-verb` is the forced command of an ssh key in the host
// user's authorized_keys, for a caller on another machine (a factory's
// tick) that spawns and destroys agents:
//
//	restrict,command="lever agent remote-verb --config /srv/lever/lever.yaml --templates session --as factory" ssh-ed25519 AAAA... factory
//
// sshd runs it whatever the caller asked for, and puts what the caller
// asked for in SSH_ORIGINAL_COMMAND. That text is parsed here against a
// fixed grammar; it never reaches a shell, a flag parser or a path. The
// caller may spawn from the --templates listed only, and may see, suspend,
// resume and destroy only the agents it spawned. It is recorded as their
// creator, "ssh:<--as>", and the broker rate-limits its spawns as it does
// the manager's.

// remoteCommandEnv is where sshd puts the caller's command.
const remoteCommandEnv = "SSH_ORIGINAL_COMMAND"

// maxRemoteCommand bounds the caller's command line, in bytes.
const maxRemoteCommand = 1024

// remoteBriefTimeout bounds the wait for a spawn's brief on stdin: a caller
// that never closes it is refused, not waited on.
var remoteBriefTimeout = 60 * time.Second

// remoteDeadline bounds one run: a stalled broker or hub never holds a
// caller's process for good (sshd keeps no time limit, and with restrict a
// caller that hangs up does not stop it).
var remoteDeadline = 10 * time.Minute

// remoteSlots bounds the runs of one key at once: each holds a slot, a
// flock on <state>/remote-verb/<as>.<n>.lock.
const remoteSlots = 4

// remoteNameRE is a template or agent name (config's nameRE).
var remoteNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// remoteGrammar is the whole grammar, for refusals.
const remoteGrammar = "spawn TEMPLATE [--memory SIZE] [LABEL...] (brief on stdin) | destroy NAME [--stop] | suspend NAME | resume NAME | list [--json]"

// remoteRefusal is what the caller is told for a broker refusal: a fixed
// text per reason, since the broker's own text is written for the operator
// and can name other agents, host paths and commands.
func remoteRefusal(reason string) string {
	switch reason {
	case broker.ReasonNoTemplate:
		return "no template that this key may spawn from"
	case broker.ReasonNotSpawned, broker.ReasonNotYours, broker.ReasonNotAWorker:
		return "no agent of yours by that name"
	case broker.ReasonTemplateFull:
		return "the template has as many agents as it may: destroy one first"
	case broker.ReasonMemoryBusy:
		return "busy: the memory budget has too little free for that size; suspend one of your agents or pick a smaller size"
	case broker.ReasonBadMemory:
		return "the template has no such memory size: list --json shows its memory_sizes"
	case broker.ReasonTemplateBusy, broker.ReasonWorkerCap, broker.ReasonBusy:
		return "busy: too many agents are running, or another verb is acting on this one; try again later"
	case broker.ReasonRateLimited:
		return "rate limited: at most 4 spawns a minute and 200 a day; try again later"
	case broker.ReasonBadLabel:
		return "the label is not allowed: at most 60 printable characters"
	case broker.ReasonRunning:
		return "the agent is running: destroy it with --stop"
	case broker.ReasonBadRequest:
		return "bad request"
	case reasonNoOperatorSocket, "":
		return "lever did not answer; try again later"
	}
	return "refused (" + cmp.Or(safeToken(reason), "unknown") + "); the operator can see why in the broker's audit log"
}

// remoteVerb is one parsed caller command.
type remoteVerb struct {
	verb     string // spawn, destroy, suspend, resume, list
	template string // spawn
	label    string // spawn
	memory   string // spawn, "" for the template's default
	name     string // destroy, suspend, resume
	stop     bool   // destroy
}

// parseRemoteVerb parses the caller's command line. Words are separated by
// ASCII spaces; any other whitespace or control or formatting character
// refuses the line, so nothing the caller sends can split, hide or
// re-order a word.
func parseRemoteVerb(line string) (remoteVerb, error) {
	if len(line) > maxRemoteCommand {
		return remoteVerb{}, fmt.Errorf("the command is longer than %d bytes", maxRemoteCommand)
	}
	if !utf8.ValidString(line) {
		return remoteVerb{}, errors.New("the command is not valid UTF-8")
	}
	for _, r := range line {
		if r != ' ' && (!unicode.IsPrint(r) || unicode.Is(unicode.Cf, r)) {
			return remoteVerb{}, errors.New("the command has a control, formatting or non-space whitespace character")
		}
	}
	w := strings.Fields(line)
	if len(w) == 0 {
		return remoteVerb{}, errors.New("no command")
	}
	name := func(kind, s string) error {
		if !remoteNameRE.MatchString(s) {
			return fmt.Errorf("%s %q is not a name", kind, s)
		}
		return nil
	}
	v := remoteVerb{verb: w[0]}
	switch {
	case v.verb == "spawn" && len(w) >= 2:
		if err := name("template", w[1]); err != nil {
			return remoteVerb{}, err
		}
		rest := w[2:]
		if len(rest) > 0 && rest[0] == "--memory" {
			if len(rest) < 2 {
				return remoteVerb{}, errors.New("--memory needs a size, such as 8GiB")
			}
			if _, ok := broker.ParseMemorySize(rest[1]); !ok {
				return remoteVerb{}, fmt.Errorf("memory size %q is not a number and MiB or GiB, such as 8GiB", rest[1])
			}
			v.memory, rest = rest[1], rest[2:]
		}
		v.template, v.label = w[1], strings.Join(rest, " ")
		if utf8.RuneCountInString(v.label) > broker.MaxAgentLabel {
			return remoteVerb{}, fmt.Errorf("the label is longer than %d characters", broker.MaxAgentLabel)
		}
	case v.verb == "destroy" && (len(w) == 2 || len(w) == 3 && w[2] == "--stop"):
		v.stop = len(w) == 3
		fallthrough
	case (v.verb == "suspend" || v.verb == "resume") && len(w) == 2:
		if err := name("agent", w[1]); err != nil {
			return remoteVerb{}, err
		}
		v.name = w[1]
	case v.verb == "list" && (len(w) == 1 || len(w) == 2 && w[1] == "--json"):
	default:
		return remoteVerb{}, errors.New("not a command this key may run")
	}
	return v, nil
}

// readBrief reads a spawn's brief from r: at most broker.MaxTaskFileBytes,
// within remoteBriefTimeout.
func readBrief(r io.Reader) (string, error) {
	type result struct {
		b   []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		b, err := io.ReadAll(io.LimitReader(r, broker.MaxTaskFileBytes+1))
		done <- result{b, err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			return "", fmt.Errorf("reading the brief: %w", res.err)
		}
		if len(res.b) > broker.MaxTaskFileBytes {
			return "", fmt.Errorf("the brief is longer than %d bytes", broker.MaxTaskFileBytes)
		}
		if !utf8.Valid(res.b) {
			return "", errors.New("the brief is not valid UTF-8")
		}
		return strings.TrimSpace(string(res.b)), nil
	case <-time.After(remoteBriefTimeout):
		return "", fmt.Errorf("no brief within %s: send it on stdin and close stdin (or redirect from /dev/null for none)", remoteBriefTimeout)
	}
}

// remoteList is `list`: the caller's templates, and its own agents.
type remoteList struct {
	OK        bool             `json:"ok"`
	Templates []remoteTemplate `json:"templates"`
	Agents    []remoteAgent    `json:"agents"`
	// Memory is the instance's memory budget for running agents and its
	// use by every agent, the caller's or not: what a spawn or resume is
	// checked against. Absent with no budget.
	Memory *wire.MemoryTotals `json:"memory,omitempty"`
}

// remoteTemplate's counts are of every agent of the template, the caller's
// or not: they are what a spawn is checked against.
type remoteTemplate struct {
	Name         string `json:"name"`
	MaxInstances int    `json:"max_instances"`
	MaxRunning   int    `json:"max_running"`
	Instances    int    `json:"instances"`
	Running      int    `json:"running"`
	// MemorySizes are the sizes spawn --memory may name, smallest first;
	// MemoryDefault is used without --memory.
	MemorySizes   []string `json:"memory_sizes,omitempty"`
	MemoryDefault string   `json:"memory_default,omitempty"`
}

type remoteAgent struct {
	Name      string    `json:"name"`
	Template  string    `json:"template"`
	Label     string    `json:"label,omitempty"`
	State     string    `json:"state"`
	Phase     string    `json:"phase,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// Memory is its memory limit ("8GiB"), and MemoryBytes the same in
	// bytes.
	Memory      string `json:"memory,omitempty"`
	MemoryBytes int64  `json:"memory_bytes,omitempty"`
}

func newAgentRemoteVerbCmd() *cobra.Command {
	var templates []string
	var as, configPath string
	c := &cobra.Command{
		Use:   "remote-verb",
		Args:  cobra.NoArgs,
		Short: "The forced command of an ssh key that may spawn and destroy agents",
		Long: "The command an authorized_keys entry forces, for a caller on another machine:\n\n" +
			"  restrict,command=\"lever agent remote-verb --config /path/lever.yaml --templates session --as factory\" ssh-ed25519 AAAA... factory\n\n" +
			"It reads the caller's command from " + remoteCommandEnv + ", which must be one of:\n  " + remoteGrammar + "\n" +
			"and prints one JSON line. The caller may spawn from the --templates listed only, and may list, suspend,\n" +
			"resume and destroy only the agents it spawned, which are recorded as created by ssh:<--as>.\n" +
			"A destroy always backs up the conversation. See docs/operations.md, Spawning over ssh.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRemoteVerb(cmd, templates, as, configPath)
		},
	}
	c.Flags().StringSliceVar(&templates, "templates", nil, "the templates the caller may spawn from (required)")
	c.Flags().StringVar(&as, "as", "", "the caller's name, recorded as ssh:<name> (required)")
	c.Flags().StringVar(&configPath, "config", "", "the instance's lever.yaml (default: the one in the current directory)")
	return c
}

func runRemoteVerb(cmd *cobra.Command, templates []string, as, configPath string) error {
	res := agentResult{}
	fail := func(err error, reason string, code int) error {
		res.OK, res.Reason, res.Error, res.Exit = false, reason, err.Error(), code
		printJSONLine(cmd, res)
		return cli.WithExitCode(fmt.Errorf("agent remote-verb: %w", err), code)
	}
	by := "ssh:" + as
	if !broker.ValidRemoteCaller(by) {
		return fail(errors.New("--as must be a name of at most 32 lowercase letters, digits and dashes (the authorized_keys entry is wrong)"), "", 1)
	}
	if len(templates) == 0 {
		return fail(errors.New("--templates is required (the authorized_keys entry is wrong)"), "", 1)
	}
	for _, t := range templates {
		if !remoteNameRE.MatchString(t) {
			return fail(fmt.Errorf("--templates: %q is not a name (the authorized_keys entry is wrong)", t), "", 1)
		}
	}
	line, ok := os.LookupEnv(remoteCommandEnv)
	if !ok {
		return fail(errors.New(remoteCommandEnv+" is not set: this runs as an ssh forced command, not by hand"), broker.ReasonBadRequest, exitWorkerRefused)
	}
	v, err := parseRemoteVerb(line)
	if err != nil {
		return fail(fmt.Errorf("%v; the commands are: %s", err, remoteGrammar), broker.ReasonBadRequest, exitWorkerRefused)
	}
	var args []string
	if configPath != "" {
		args = []string{configPath}
	}
	// Host paths stay on the host: the caller is told only that lever is
	// not available, and the operator runs the command by hand to see why.
	app, st, err := loadAppAndState(args)
	if err != nil {
		return fail(errors.New("lever is not available on this host (its config does not load)"), "", 1)
	}
	sock, err := operatorSocket(app, st)
	if err != nil {
		return fail(errors.New("lever is not running on this host"), reasonNoOperatorSocket, exitOperatorSocketMissing)
	}
	release, err := takeRemoteSlot(st.Dir, as)
	if err != nil {
		return fail(err, broker.ReasonBusy, exitWorkerBusy)
	}
	defer release()
	ctx, cancel := context.WithTimeout(cmd.Context(), remoteDeadline)
	defer cancel()
	brokerFail := func(err error, path string) error {
		if ref, ok := operatorFailure(err, path); ok {
			res.Phase = safeToken(ref.Phase)
			reason := safeToken(ref.Reason)
			return fail(errors.New(remoteRefusal(reason)), reason, agentExit(reason))
		}
		return fail(errors.New("lever did not answer; try again later"), "", 1)
	}
	post := func(path string, body, out any) error {
		if err := operatorPost(ctx, sock, path, body, out); err != nil {
			return brokerFail(err, path)
		}
		return nil
	}
	// registry is the broker's list; with hub, phases are read from the hub.
	registry := func(hub bool) (broker.AgentListResponse, error) {
		path := broker.PathOperatorAgentList
		if !hub {
			path += "?registry=1"
		}
		var resp broker.AgentListResponse
		if err := operatorGet(ctx, sock, path, &resp); err != nil {
			return resp, brokerFail(err, broker.PathOperatorAgentList)
		}
		return resp, nil
	}
	// own finds v.name among the caller's live agents: one it spawned, from a
	// template it may still use. Anything else is refused alike, so the
	// caller learns nothing of other agents.
	own := func() error {
		res.Name = v.name
		list, err := registry(false)
		if err != nil {
			return err
		}
		for _, a := range list.Agents {
			if a.Name == v.name && a.CreatedBy == by && a.State != broker.AgentDestroyed && slices.Contains(templates, a.Template) {
				return nil
			}
		}
		return fail(errors.New(remoteRefusal(broker.ReasonNotSpawned)), broker.ReasonNotSpawned, exitWorkerRefused)
	}

	switch v.verb {
	case "spawn":
		res.Template, res.Label = v.template, v.label
		if !slices.Contains(templates, v.template) {
			return fail(errors.New(remoteRefusal(broker.ReasonNoTemplate)), broker.ReasonNoTemplate, exitWorkerRefused)
		}
		brief, err := readBrief(cmd.InOrStdin())
		if err != nil {
			return fail(err, broker.ReasonBadRequest, exitWorkerRefused)
		}
		var resp broker.SpawnResponse
		if err := post(broker.PathOperatorAgentSpawn, broker.SpawnRequest{Template: v.template, Task: brief, Label: v.label, Memory: v.memory, By: by}, &resp); err != nil {
			return err
		}
		res.OK, res.Name, res.Phase = true, safeToken(resp.Name), safeToken(resp.Phase)
		if _, ok := broker.ParseMemorySize(resp.Memory); ok {
			res.Memory = resp.Memory
		}
	case "destroy":
		if err := own(); err != nil {
			return err
		}
		var resp broker.DestroyResponse
		if err := post(broker.PathOperatorAgentDestroy, broker.DestroyRequest{Name: v.name, Stop: v.stop, By: by}, &resp); err != nil {
			return err
		}
		res.OK, res.Was = true, cmp.Or(safeToken(resp.Was), "unknown")
		if convbackup.ValidID(resp.Transcript) {
			res.Transcript = resp.Transcript
		}
		// The workspace line and warnings name host paths: the caller is
		// told that something needs the operator, not what.
		if len(resp.Warnings) > 0 {
			res.Warnings = []string{"destroyed, with warnings the operator can see in the broker's audit log"}
		}
	case "suspend", "resume":
		if err := own(); err != nil {
			return err
		}
		// The broker checks again that the agent is the caller's, and
		// audits the verb as the caller's.
		path := broker.PathOperatorAgentSuspend
		if v.verb == "resume" {
			path = broker.PathOperatorAgentResume
		}
		var resp wire.WorkerResponse
		if err := post(path, broker.AgentVerbRequest{Name: v.name, By: by}, &resp); err != nil {
			return err
		}
		res.OK, res.Phase = true, safeToken(resp.Phase)
	case "list":
		list, err := registry(true)
		if err != nil {
			return err
		}
		out := remoteList{OK: true, Templates: []remoteTemplate{}, Agents: []remoteAgent{}}
		if m := list.Memory; m != nil {
			out.Memory = &wire.MemoryTotals{BudgetBytes: m.BudgetBytes, UsedBytes: m.UsedBytes, FreeBytes: m.FreeBytes}
		}
		for _, t := range list.Templates {
			if slices.Contains(templates, t.Name) {
				rt := remoteTemplate{Name: t.Name, MaxInstances: t.MaxInstances, MaxRunning: t.MaxRunning, Instances: t.Instances, Running: t.Running}
				for _, sz := range t.MemorySizes {
					if _, ok := broker.ParseMemorySize(sz.Name); ok {
						rt.MemorySizes = append(rt.MemorySizes, sz.Name)
					}
				}
				if _, ok := broker.ParseMemorySize(t.MemoryDefault); ok {
					rt.MemoryDefault = t.MemoryDefault
				}
				out.Templates = append(out.Templates, rt)
			}
		}
		for _, a := range list.Agents {
			if a.CreatedBy == by && a.State != broker.AgentDestroyed && slices.Contains(templates, a.Template) {
				ra := remoteAgent{Name: a.Name, Template: a.Template, Label: a.Label, State: a.State, Phase: safeToken(a.Phase), CreatedAt: a.CreatedAt}
				if _, ok := broker.ParseMemorySize(a.Memory); ok {
					ra.Memory, ra.MemoryBytes = a.Memory, a.MemoryBytes
				}
				out.Agents = append(out.Agents, ra)
			}
		}
		printJSONLine(cmd, out)
		return nil
	}
	printJSONLine(cmd, res)
	return nil
}

// takeRemoteSlot takes one of the key's remoteSlots, or refuses: a key
// cannot hold more than that many processes on the host at once.
func takeRemoteSlot(stateDir, as string) (func(), error) {
	dir := filepath.Join(stateDir, "remote-verb")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, errors.New("lever is not available on this host")
	}
	for n := range remoteSlots {
		release, err := hostledger.TryLockFile(filepath.Join(dir, fmt.Sprintf("%s.%d.lock", as, n)))
		if err == nil {
			return release, nil
		}
		if !errors.Is(err, hostledger.ErrLocked) {
			return nil, errors.New("lever is not available on this host")
		}
	}
	return nil, fmt.Errorf("busy: this key already runs %d commands; try again when one ends", remoteSlots)
}

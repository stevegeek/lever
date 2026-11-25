package config

import (
	"cmp"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Template is one `templates:` entry: the shape of agents the broker spawns
// at runtime and destroys when done. Every instance gets the template's
// fields as they are configured when the broker starts; the spawner picks
// only the task and a label. An instance's name is "<prefix>-<id>", made by
// the broker and never reused, and its workspace is <dir_root>/<name>.
//
// The json tags keep unset optional fields out of brokerctl.ConfigHash.
type Template struct {
	Name string `yaml:"name"`
	// Prefix starts every instance name; empty is Name.
	Prefix string `yaml:"prefix" json:",omitempty"`
	// DirRoot holds the instances' workspaces, tree-relative. It is disjoint
	// from every worker dir, every other template's root and every
	// protected directory.
	DirRoot          string          `yaml:"dir_root"`
	Image            string          `yaml:"image" json:",omitempty"`
	ImageTar         string          `yaml:"image_tar" json:",omitempty"`
	Model            string          `yaml:"model" json:",omitempty"`
	InstructionsFile string          `yaml:"instructions_file" json:",omitempty"`
	ClaudeSettings   ClaudeSettings  `yaml:"claude_settings" json:",omitzero"`
	AfterCompactNote string          `yaml:"after_compact_note" json:",omitempty"`
	LLMAuth          LLMAuthMode     `yaml:"llm_auth" json:",omitempty"`
	Obtain           []Grant         `yaml:"obtain" json:",omitempty"`
	Delegate         []DelegateGrant `yaml:"delegate" json:",omitempty"`
	// Resources must name a memory limit: the fit check below needs it.
	// With MemorySizes, resources.memory is the default size.
	Resources Resources `yaml:"resources"`
	// MemorySizes are the memory limits a spawn may choose from (`spawn
	// --memory`); unset: resources.memory only.
	MemorySizes   []string `yaml:"memory_sizes" json:",omitempty"`
	SkipRmConfirm bool     `yaml:"even_more_dangerously_skip_permissions" json:",omitempty"`
	// MaxInstances bounds the instances that exist (any phase), so disk and
	// hub records stay bounded.
	MaxInstances int `yaml:"max_instances"`
	// MaxRunning bounds the instances running at once.
	MaxRunning int               `yaml:"max_running"`
	OnDestroy  TemplateOnDestroy `yaml:"on_destroy" json:",omitzero"`
	// Spawners are who may spawn from it: "operator" (always allowed, the
	// default) and "manager".
	Spawners []string `yaml:"spawners" json:",omitempty"`
	// IdleSuspend suspends an instance that has waited at its prompt this
	// long (0, the default, never does). Suspend keeps the conversation.
	IdleSuspend time.Duration `yaml:"idle_suspend" json:",omitempty"`
}

// MinIdleSuspend is the shortest idle_suspend: the broker looks once a
// minute, and a shorter one would suspend an agent between two prompts.
const MinIdleSuspend = 5 * time.Minute

// TemplateOnDestroy is what destroy does with an instance's workspace.
type TemplateOnDestroy struct {
	Workspace string `yaml:"workspace" json:",omitempty"`
}

// on_destroy.workspace values.
const (
	// WorkspaceArchive packs the workspace into the state directory, then
	// removes it from the tree. The default.
	WorkspaceArchive = "archive"
	WorkspaceRemove  = "remove"
	WorkspaceKeep    = "keep"
)

// Spawner names.
const (
	SpawnerOperator = "operator"
	SpawnerManager  = "manager"
)

// TemplateRefPrefix starts a delegate target that names every active
// instance of a template: "template:<name>".
const TemplateRefPrefix = "template:"

// MaxTemplateInstances bounds max_instances.
const MaxTemplateInstances = 100

// InstanceIDLen is the length of the random part of an instance name.
const InstanceIDLen = 6

// instanceIDRE is the random part: lowercase base32, no padding.
var instanceIDRE = regexp.MustCompile(`^[a-z2-7]{6}$`)

// scionRunningCap is scion's cap on agents running at once on one runtime
// broker, the manager included.
const scionRunningCap = 12

// agentOverheadMemory is the guest and hub's share of the VM in the memory
// fit check.
const agentOverheadMemory = 1 << 30

// EffectivePrefix is the instance-name prefix.
func (t Template) EffectivePrefix() string {
	if t.Prefix != "" {
		return t.Prefix
	}
	return t.Name
}

// EffectiveOnDestroy is on_destroy.workspace, archive when unset.
func (t Template) EffectiveOnDestroy() string {
	if t.OnDestroy.Workspace != "" {
		return t.OnDestroy.Workspace
	}
	return WorkspaceArchive
}

// MaySpawn reports whether spawner may spawn from t. The operator always may.
func (t Template) MaySpawn(spawner string) bool {
	return spawner == SpawnerOperator || slices.Contains(t.Spawners, spawner)
}

// InstanceName reports whether name has the shape of one of t's instances.
func (t Template) InstanceName(name string) bool {
	id, ok := strings.CutPrefix(name, t.EffectivePrefix()+"-")
	return ok && instanceIDRE.MatchString(id)
}

// InstanceDir is the tree-relative workspace of instance name.
func (t Template) InstanceDir(name string) string {
	return path.Join(filepath.ToSlash(path.Clean(filepath.ToSlash(t.DirRoot))), name)
}

// Worker is instance name as a worker: every helper that resolves a worker's
// image, model, instructions, LLM mode or Claude settings resolves an
// instance the same way. It has no task_file, is not recyclable and never
// autostarts.
func (t Template) Worker(name string) Worker {
	return Worker{
		Name: name, Dir: t.InstanceDir(name),
		Image: t.Image, ImageTar: t.ImageTar, Model: t.Model,
		InstructionsFile: t.InstructionsFile,
		ClaudeSettings:   t.ClaudeSettings, AfterCompactNote: t.AfterCompactNote,
		LLMAuth:       t.LLMAuth,
		Obtain:        slices.Clone(t.Obtain),
		Delegate:      cloneDelegates(t.Delegate),
		Resources:     t.Resources,
		SkipRmConfirm: t.SkipRmConfirm,
	}
}

func cloneDelegates(ds []DelegateGrant) []DelegateGrant {
	out := slices.Clone(ds)
	for i := range out {
		out[i].To = slices.Clone(out[i].To)
	}
	return out
}

// prototype is t as a worker for the checks that look at every agent's
// image, tar and LLM mode: it never runs.
func (t Template) prototype() Worker { return t.Worker(t.EffectivePrefix() + "-aaaaaa") }

// AnyManagerTemplate reports whether the manager may spawn from some
// template: its skill teaches `agent spawn` only then.
func (a *App) AnyManagerTemplate() bool {
	return slices.ContainsFunc(a.Templates, func(t Template) bool { return t.MaySpawn(SpawnerManager) })
}

// TemplateByName returns the configured template, or false.
func (a *App) TemplateByName(name string) (Template, bool) {
	for _, t := range a.Templates {
		if t.Name == name {
			return t, true
		}
	}
	return Template{}, false
}

// TemplateOf returns the template name is an instance of by its shape, or
// false. Only the broker's registry says whether the instance exists.
func (a *App) TemplateOf(name string) (Template, bool) {
	for _, t := range a.Templates {
		if t.InstanceName(name) {
			return t, true
		}
	}
	return Template{}, false
}

// AgentDir is one directory an agent other than the manager mounts
// read-write: a worker's dir, or a template's dir_root, which holds its
// instances' dirs.
type AgentDir struct {
	Who string // `worker "x"` or `template "y"`, for messages
	Key string // "dir" or "dir_root"
	Dir string
}

func (d AgentDir) String() string { return fmt.Sprintf("%s (%s %q)", d.Who, d.Key, d.Dir) }

// AgentDirs lists every worker dir and template dir_root.
func (a *App) AgentDirs() []AgentDir {
	out := make([]AgentDir, 0, len(a.Workers)+len(a.Templates))
	for _, g := range a.Workers {
		out = append(out, AgentDir{fmt.Sprintf("worker %q", g.Name), "dir", g.Dir})
	}
	for _, t := range a.Templates {
		out = append(out, AgentDir{fmt.Sprintf("template %q", t.Name), "dir_root", t.DirRoot})
	}
	return out
}

// workerShapes is every worker plus a prototype per template, for the checks
// over every agent's image, tar and LLM mode.
func (a *App) workerShapes() []Worker {
	out := slices.Clone(a.Workers)
	for _, t := range a.Templates {
		out = append(out, t.prototype())
	}
	return out
}

func (a *App) validateTemplates() error {
	seenName := map[string]bool{}
	seenPrefix := map[string]string{}
	for _, t := range a.Templates {
		if err := a.validateTemplate(t); err != nil {
			return err
		}
		if seenName[t.Name] {
			return fmt.Errorf("config: template %q is declared twice", t.Name)
		}
		seenName[t.Name] = true
		p := t.EffectivePrefix()
		if prev, ok := seenPrefix[p]; ok {
			return fmt.Errorf("config: templates %q and %q both use the prefix %q; their instances' names could not be told apart", prev, t.Name, p)
		}
		seenPrefix[p] = t.Name
	}
	// No fixed name may look like an instance: an instance would then share
	// its certificate CN and scion slug.
	fixed := []string{a.Name, a.ManagerCN(), managerAlias, operatorName}
	for _, g := range a.Workers {
		fixed = append(fixed, g.Name)
	}
	for _, n := range fixed {
		if t, ok := a.TemplateOf(n); ok {
			return fmt.Errorf("config: %q has the shape of an instance of template %q (%s-<6 letters or digits 2-7>); rename it or change the template's prefix", n, t.Name, t.EffectivePrefix())
		}
	}
	for _, t := range a.Templates {
		if slices.Contains(fixed, t.Name) {
			return fmt.Errorf("config: template name %q is already the name of an agent (or reserved); rename the template", t.Name)
		}
	}
	if err := a.checkDelegationsToManagerTemplates(); err != nil {
		return err
	}
	return a.validateTemplateFit()
}

func (a *App) validateTemplate(t Template) error {
	who := fmt.Sprintf("template %q", t.Name)
	if !nameRE.MatchString(t.Name) {
		return fmt.Errorf("config: template name %q must match %s", t.Name, nameRE)
	}
	p := t.EffectivePrefix()
	if !nameRE.MatchString(p) || len(p) > 63-1-InstanceIDLen {
		return fmt.Errorf("config: %s prefix %q must match %s and be at most %d characters (an instance name is <prefix>-<%d characters>)", who, p, nameRE, 63-1-InstanceIDLen, InstanceIDLen)
	}
	if t.DirRoot == "" || !confinedRel(t.DirRoot) {
		return fmt.Errorf("config: %s dir_root %q must be a relative path inside the tree, and not the tree itself", who, t.DirRoot)
	}
	if t.Image != "" {
		if err := a.Security.validateImage(who+" image", t.Image); err != nil {
			return err
		}
	}
	if err := validateImageTar(fmt.Sprintf("templates[%s]", t.Name), t.Image, t.ImageTar); err != nil {
		return err
	}
	if t.InstructionsFile != "" && !confinedRel(t.InstructionsFile) {
		return fmt.Errorf("config: %s instructions_file %q must be a relative path inside the instance root (no \"..\", not absolute)", who, t.InstructionsFile)
	}
	if err := validateClaude(who, t.ClaudeSettings, t.AfterCompactNote); err != nil {
		return err
	}
	if !t.LLMAuth.valid() {
		return fmt.Errorf("config: %s llm_auth %q invalid (want subscription|api-key)", who, t.LLMAuth)
	}
	if t.Resources.Memory == "" {
		return fmt.Errorf("config: %s needs resources.memory: lever admits its agents by their memory", who)
	}
	if err := a.validateResources(who, t.Resources); err != nil {
		return err
	}
	if err := a.validateMemorySizes(who, t); err != nil {
		return err
	}
	if t.MaxRunning < 1 {
		return fmt.Errorf("config: %s max_running must be at least 1", who)
	}
	if t.MaxInstances < t.MaxRunning || t.MaxInstances > MaxTemplateInstances {
		return fmt.Errorf("config: %s max_instances %d must be between max_running (%d) and %d", who, t.MaxInstances, t.MaxRunning, MaxTemplateInstances)
	}
	if t.IdleSuspend != 0 && (t.IdleSuspend < MinIdleSuspend || t.IdleSuspend > 30*24*time.Hour) {
		return fmt.Errorf("config: %s idle_suspend %s must be 0 (off) or between %s and 30 days", who, t.IdleSuspend, MinIdleSuspend)
	}
	switch t.OnDestroy.Workspace {
	case "", WorkspaceArchive, WorkspaceRemove, WorkspaceKeep:
	default:
		return fmt.Errorf("config: %s on_destroy.workspace %q invalid (want archive|remove|keep; unset is archive)", who, t.OnDestroy.Workspace)
	}
	for i, s := range t.Spawners {
		if s != SpawnerOperator && s != SpawnerManager {
			return fmt.Errorf("config: %s spawners lists %q (want operator or manager)", who, s)
		}
		if slices.Contains(t.Spawners[:i], s) {
			return fmt.Errorf("config: %s spawners lists %q twice", who, s)
		}
	}
	if t.MaySpawn(SpawnerManager) {
		if err := a.notWiderThanManager(t); err != nil {
			return err
		}
	}
	return nil
}

// notWiderThanManager: a manager must not mint agents more powerful than
// itself. Every capability a manager-spawnable template obtains or
// delegates, the manager must obtain itself.
func (a *App) notWiderThanManager(t Template) error {
	mgr := map[Grant]bool{}
	for _, g := range a.Manager.Obtain {
		mgr[g] = true
	}
	for _, g := range t.Obtain {
		if !mgr[g] {
			return fmt.Errorf("config: template %q lists manager in spawners but obtains %s.%s, which the manager does not obtain; a manager may not spawn an agent wider than itself", t.Name, g.Tool, g.Op)
		}
	}
	// The manager controls what it spawns, so a delegation by one of its
	// agents is the manager's own: it may delegate nothing.
	if len(t.Delegate) > 0 {
		return fmt.Errorf("config: template %q lists manager in spawners and delegates %s.%s; a template the manager may spawn from may not delegate (a manager may not spawn an agent wider than itself)", t.Name, t.Delegate[0].Tool, t.Delegate[0].Op)
	}
	return nil
}

// checkDelegationsToManagerTemplates: an agent the manager spawned is the
// manager's to steer, so only the manager may delegate to one.
func (a *App) checkDelegationsToManagerTemplates() error {
	check := func(who string, ds []DelegateGrant) error {
		for _, d := range ds {
			for _, to := range d.To {
				name, ok := templateRef(to)
				if !ok {
					continue
				}
				if t, ok := a.TemplateByName(name); ok && t.MaySpawn(SpawnerManager) {
					return fmt.Errorf("config: %s delegates %s.%s to %q, a template the manager may spawn from; only the manager may delegate to its agents", who, d.Tool, d.Op, to)
				}
			}
		}
		return nil
	}
	for _, g := range a.Workers {
		if err := check("worker "+g.Name, g.Delegate); err != nil {
			return err
		}
	}
	for _, t := range a.Templates {
		if err := check("template "+t.Name, t.Delegate); err != nil {
			return err
		}
	}
	return nil
}

// validateTemplateFit refuses templates whose running instances, with the
// manager and the workers, would exceed scion's running cap.
func (a *App) validateTemplateFit() error {
	if len(a.Templates) == 0 {
		return nil
	}
	running := len(a.Workers)
	for _, t := range a.Templates {
		running += t.MaxRunning
	}
	if c := a.Broker.MaxRunningWorkers; c > 0 && c < running {
		running = c
	}
	if running+1 > scionRunningCap {
		return fmt.Errorf("config: the manager, the workers and every template's max_running come to %d agents running at once, past scion's cap of %d; lower max_running or set broker.max_running_workers to at most %d",
			running+1, scionRunningCap, scionRunningCap-1)
	}
	return nil
}

// MaxMemorySizes bounds a template's memory_sizes.
const MaxMemorySizes = 8

// validateMemorySizes: each size is a valid limit, none is listed twice
// (by value), and resources.memory, the default, is one of them.
func (a *App) validateMemorySizes(who string, t Template) error {
	if len(t.MemorySizes) == 0 {
		return nil
	}
	if len(t.MemorySizes) > MaxMemorySizes {
		return fmt.Errorf("config: %s memory_sizes lists %d sizes; at most %d", who, len(t.MemorySizes), MaxMemorySizes)
	}
	seen := map[int64]string{}
	for _, m := range t.MemorySizes {
		if err := a.validateResources(who+" memory_sizes", Resources{Memory: m}); err != nil {
			return err
		}
		n, _ := agentMemoryBytes(m)
		if prev, ok := seen[n]; ok {
			return fmt.Errorf("config: %s memory_sizes lists %q and %q, the same size", who, prev, m)
		}
		seen[n] = m
	}
	def, _ := agentMemoryBytes(t.Resources.Memory)
	if _, ok := seen[def]; !ok {
		return fmt.Errorf("config: %s resources.memory %q is the default size and must be one of memory_sizes %v", who, t.Resources.Memory, t.MemorySizes)
	}
	return nil
}

// TemplateMemorySize is one memory size an instance may be spawned with.
type TemplateMemorySize struct {
	// Name is the size as the config spells it ("8GiB").
	Name  string
	Bytes int64
	// Scion is the limit in scion's spelling ("8Gi").
	Scion string
}

// Sizes are the memory sizes t's instances may be spawned with, smallest
// first: memory_sizes, or resources.memory alone.
func (t Template) Sizes() []TemplateMemorySize {
	names := t.MemorySizes
	if len(names) == 0 {
		names = []string{t.Resources.Memory}
	}
	var out []TemplateMemorySize
	for _, m := range names {
		n, ok := agentMemoryBytes(m)
		if !ok {
			continue
		}
		sc, _ := Resources{Memory: m}.ScionLimits()
		// One spelling whatever the config wrote ("8Gi", "8192MiB"): the
		// name is what a spawn, the record and every list carry.
		out = append(out, TemplateMemorySize{Name: CanonicalMemory(n), Bytes: n, Scion: sc})
	}
	slices.SortFunc(out, func(x, y TemplateMemorySize) int { return cmp.Compare(x.Bytes, y.Bytes) })
	return out
}

// CanonicalMemory spells n bytes as whole GiB, else MiB ("8GiB",
// "1536MiB").
func CanonicalMemory(n int64) string {
	if n%(1<<30) == 0 {
		return fmt.Sprintf("%dGiB", n>>30)
	}
	return fmt.Sprintf("%dMiB", n>>20)
}

// AgentMemoryBytes is an agent's resources.memory in bytes; 0 when unset
// (nothing bounds it, so it counts as nothing against the budget).
func AgentMemoryBytes(r Resources) int64 {
	n, _ := agentMemoryBytes(r.Memory)
	return n
}

// MemoryBudgetBytes is the memory the running agents' caps may add up to:
// broker.memory_budget, else the VM's memory less the manager's limit and
// 1GiB for the guest. false: no budget (no memory on either).
func (a *App) MemoryBudgetBytes() (int64, bool) {
	if a.Broker.MemoryBudget != "" {
		return sizeBytes(a.Broker.MemoryBudget)
	}
	vm, ok := sizeBytes(a.Memory)
	if a.Memory == "" || !ok {
		return 0, false
	}
	n := vm - agentOverheadMemory - AgentMemoryBytes(a.Manager.Resources)
	if n <= 0 {
		return 0, false
	}
	return n, true
}

// validateMemoryFit refuses a config with an agent that could never start:
// one larger than the budget, or one that with the manager and the guest
// would not fit in the VM. How many run at once is the budget's to decide,
// at each start.
func (a *App) validateMemoryFit() error {
	if a.Broker.MemoryBudget != "" {
		n, ok := sizeBytes(a.Broker.MemoryBudget)
		if !ok || n <= 0 {
			return fmt.Errorf("config: broker.memory_budget %q invalid (want e.g. 30GiB or 20480MiB)", a.Broker.MemoryBudget)
		}
		if vm, ok := sizeBytes(a.Memory); a.Memory != "" && ok && n > vm {
			return fmt.Errorf("config: broker.memory_budget %q is more than the VM's memory %q", a.Broker.MemoryBudget, a.Memory)
		}
	}
	type agent struct {
		who string
		n   int64
	}
	var agents []agent
	for _, g := range a.Workers {
		agents = append(agents, agent{fmt.Sprintf("worker %q", g.Name), AgentMemoryBytes(g.Resources)})
	}
	for _, t := range a.Templates {
		if sz := t.Sizes(); len(sz) > 0 {
			agents = append(agents, agent{fmt.Sprintf("template %q (size %s)", t.Name, sz[len(sz)-1].Name), sz[len(sz)-1].Bytes})
		}
	}
	vm, vmOK := sizeBytes(a.Memory)
	vmOK = vmOK && a.Memory != ""
	mgr := AgentMemoryBytes(a.Manager.Resources)
	budget, budgetOK := a.MemoryBudgetBytes()
	for _, g := range agents {
		if vmOK && g.n+mgr+agentOverheadMemory > vm {
			return fmt.Errorf("config: %s needs %dMiB; with the manager's %dMiB and 1GiB for the guest that is more than the VM's memory %q; lower its memory or raise memory",
				g.who, g.n>>20, mgr>>20, a.Memory)
		}
		if budgetOK && g.n > budget {
			return fmt.Errorf("config: %s needs %dMiB, more than the memory budget for agents (%dMiB); it could never start. Lower its memory or raise broker.memory_budget",
				g.who, g.n>>20, budget>>20)
		}
	}
	return nil
}

// templateRef returns the template a delegate target names, or false.
func templateRef(to string) (string, bool) {
	return strings.CutPrefix(to, TemplateRefPrefix)
}

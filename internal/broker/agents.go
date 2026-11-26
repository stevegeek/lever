package broker

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/broker/rules"
)

// agentRegistry holds the workers the broker may drive, each with the spec
// it was built from: the config's workers, and the agents spawned from a
// template. Every read goes through it, under its lock.
//
// A spawned agent also has a record, kept in the registry file
// (PersistenceConfig.Agents) whatever its state, so a destroyed name is
// never drawn again. Its spec exists while it is spawning, active or being
// destroyed and its template is configured.
type agentRegistry struct {
	mu    sync.RWMutex
	specs map[string]WorkerSpec
	// records are the spawned agents, destroyed ones included.
	records   map[string]*AgentRecord
	templates map[string]TemplateSpec
	// retired are the names gc dropped the records of (AgentsState.Retired).
	retired map[string]bool
	// persist saves the records, under mu; nil: not saved (tests).
	persist func(AgentsState) error
}

// Agent record states.
const (
	AgentSpawning   = "spawning"
	AgentActive     = "active"
	AgentDestroying = "destroying"
	AgentDestroyed  = "destroyed"
)

// AgentRecord is one spawned agent. Only the broker writes it: the spawner
// supplies the template, the task and the label, never the rest.
type AgentRecord struct {
	Name      string    `json:"name"`
	Template  string    `json:"template"`
	Label     string    `json:"label,omitempty"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	State     string    `json:"state"`
	// Dir is the tree-relative workspace, kept so the agent can be
	// destroyed after its template is removed from the config.
	Dir string `json:"dir"`
	// DirID is the workspace's "dev:ino" when spawn created it: destroy
	// acts only on that directory.
	DirID       string    `json:"dir_id,omitempty"`
	DestroyedAt time.Time `json:"destroyed_at,omitzero"`
	// Transcript is the id of the conversation backup destroy took.
	Transcript string `json:"transcript,omitempty"`
	// Workspace is what destroy did with the workspace.
	Workspace string `json:"workspace,omitempty"`
	// Memory is the memory limit the agent was created with, as the config
	// spells it ("8GiB"), and MemoryBytes the same in bytes. Unset in a
	// record from before sizes: its template's resources.memory.
	Memory      string `json:"memory,omitempty"`
	MemoryBytes int64  `json:"memory_bytes,omitempty"`
	// RestoredFrom names the destroyed agent whose archive seeded it.
	RestoredFrom string `json:"restored_from,omitempty"`
}

// AgentsState is the registry file.
type AgentsState struct {
	Agents []AgentRecord `json:"agents"`
	// Retired are the names of destroyed agents whose records `lever agent
	// gc` dropped: a name only, kept for good, so it is never drawn again.
	Retired []string `json:"retired,omitempty"`
}

// TemplateSpec is one template, from config only.
type TemplateSpec struct {
	Name         string
	Prefix       string
	DirRoot      string // tree-relative
	MaxInstances int
	MaxRunning   int
	// OnDestroy is "archive", "remove" or "keep".
	OnDestroy string
	// ManagerMay lets the manager spawn from it (spawners: [manager]).
	ManagerMay bool
	Obtain     []rules.Grant
	Delegate   []rules.Delegation
	// Worker builds instance name's spec; the name is the only input.
	Worker func(name string) WorkerSpec
	// SkillRel (workspace-relative) and Skill are the agent's lever skill,
	// written into a new workspace before its start.
	SkillRel string
	Skill    []byte
	// IdleSuspend suspends an agent waiting at its prompt this long (0:
	// never).
	IdleSuspend time.Duration
	// Sizes are the memory sizes a spawn may choose, smallest first;
	// Default is the one used when a spawn names none.
	Sizes         []MemorySize
	DefaultMemory MemorySize
}

// MemorySize is one memory limit an agent may be spawned with.
type MemorySize struct {
	// Name is the config's spelling ("8GiB"), Scion scion's ("8Gi").
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
	Scion string `json:"-"`
}

// size finds the template's size of n bytes.
func (t TemplateSpec) size(n int64) (MemorySize, bool) {
	for _, s := range t.Sizes {
		if s.Bytes == n {
			return s, true
		}
	}
	return MemorySize{}, false
}

// sizeNames lists the template's sizes for a message.
func (t TemplateSpec) sizeNames() string {
	names := make([]string, len(t.Sizes))
	for i, s := range t.Sizes {
		names[i] = s.Name
	}
	return strings.Join(names, ", ")
}

// errStateBackwards: a change would move a record back (destroying to
// active, say).
var errStateBackwards = errors.New("a spawned agent's state only moves forward")

func stateRank(s string) int {
	switch s {
	case AgentSpawning:
		return 0
	case AgentActive:
		return 1
	case AgentDestroying:
		return 2
	case AgentDestroyed:
		return 3
	}
	return -1
}

// errTemplateFull: max_instances agents of the template exist.
var errTemplateFull = errors.New("template is at max_instances")

func newAgentRegistry(specs []WorkerSpec) *agentRegistry {
	r := &agentRegistry{specs: make(map[string]WorkerSpec, len(specs)), records: map[string]*AgentRecord{}, templates: map[string]TemplateSpec{}}
	for _, s := range specs {
		r.specs[s.Name] = s
	}
	return r
}

// load adds the templates and the saved records. An agent that is not
// destroyed gets its spec from its template as configured now; one whose
// template is gone gets a bare spec that only destroy uses.
func (r *agentRegistry) load(templates []TemplateSpec, st AgentsState, tree string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range templates {
		r.templates[t.Name] = t
	}
	if r.retired == nil {
		r.retired = map[string]bool{}
	}
	for _, n := range st.Retired {
		if n != "" {
			r.retired[n] = true
		}
	}
	for _, rec := range st.Agents {
		rec := rec
		if _, static := r.specs[rec.Name]; static || rec.Name == "" {
			continue
		}
		r.records[rec.Name] = &rec
		if rec.State == AgentDestroyed {
			continue
		}
		if t, ok := r.templates[rec.Template]; ok && t.Worker != nil && inPlace(rec, t) {
			r.specs[rec.Name] = spawnedSpec(t, rec)
			continue
		}
		r.specs[rec.Name] = bareSpec(rec, tree)
	}
}

// inPlace reports whether rec's workspace is where its template puts it
// now. One whose template's dir_root changed is driven no more, as if its
// template were gone: its old directory is no longer read-only for the
// manager nor checked against the other agents' dirs, so only destroy (which
// acts on the directory spawn made, by its device and inode) may touch it.
func inPlace(rec AgentRecord, t TemplateSpec) bool {
	return rec.Dir != "" && rec.Dir == strings.TrimSuffix(t.DirRoot, "/")+"/"+rec.Name
}

// bareSpec is enough of a spec to destroy an agent whose template is gone
// or whose workspace is not where its template puts it now.
func bareSpec(rec AgentRecord, tree string) WorkerSpec {
	ws := ""
	if tree != "" && rec.Dir != "" {
		ws = tree + "/" + rec.Dir
	}
	// Its memory still counts against the budget while it runs.
	return WorkerSpec{Name: rec.Name, WorkspaceSubdir: rec.Dir, HostWorkspace: ws, MemoryBytes: rec.MemoryBytes, spawned: true}
}

// spawnedSpec is rec's spec from its template, with the memory it was
// created with. A record from before sizes keeps the template's default.
func spawnedSpec(t TemplateSpec, rec AgentRecord) WorkerSpec {
	s := t.Worker(rec.Name)
	s.spawned = true
	if rec.MemoryBytes > 0 {
		s.MemoryBytes = rec.MemoryBytes
		s.Resources.Memory = scionMemory(rec.Memory)
	}
	return s
}

// scionMemory is a config size in scion's spelling: scion reads "Gi", not
// "GiB".
func scionMemory(m string) string {
	if strings.HasSuffix(m, "iB") {
		return strings.TrimSuffix(m, "B")
	}
	return m
}

// get returns a copy of name's spec.
func (r *agentRegistry) get(name string) (WorkerSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.specs[name]
	return s, ok
}

func (r *agentRegistry) has(name string) bool {
	_, ok := r.get(name)
	return ok
}

// names returns the registered names, sorted: a snapshot, so a caller that
// ranges over it holds no lock.
func (r *agentRegistry) names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.specs))
	for name := range r.specs {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// put adds or replaces spec.
func (r *agentRegistry) put(spec WorkerSpec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.specs[spec.Name] = spec
}

// record returns a copy of name's record, or false for a config worker or
// an unknown name.
func (r *agentRegistry) record(name string) (AgentRecord, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.records[name]
	if !ok {
		return AgentRecord{}, false
	}
	return *rec, true
}

// spawned reports whether name is a spawned agent (any state).
func (r *agentRegistry) spawned(name string) bool {
	_, ok := r.record(name)
	return ok
}

// usable reports whether name's spec may be started or resumed: a config
// worker, or a spawned agent that is active with its template configured.
func (r *agentRegistry) usable(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.records[name]
	if !ok {
		_, ok := r.specs[name]
		return ok
	}
	t, tmpl := r.templates[rec.Template]
	return rec.State == AgentActive && tmpl && inPlace(*rec, t)
}

// delegateTemplateOf is the policy's template resolver: agent's template
// when requester may delegate to it through a "template:" recipient. Only
// an active agent of a configured template matches, and the spawner rule
// holds as everywhere else: the manager reaches only the agents it spawned
// from templates that still let it, and a spawned agent only the agents of
// its own spawner. A configured worker's delegate is the operator's word and
// reaches every agent of the template.
func (r *agentRegistry) delegateTemplateOf(requester, agent, manager string) (string, bool) {
	// One read lock for the whole answer: a destroy (or gc) between two
	// would leave it reading a record that is gone.
	r.mu.RLock()
	defer r.mu.RUnlock()
	target, ok := r.records[agent]
	if !ok || target.State != AgentActive {
		return "", false
	}
	t, ok := r.templates[target.Template]
	if !ok || !inPlace(*target, t) {
		return "", false
	}
	if requester == manager {
		return t.Name, target.CreatedBy == manager && t.ManagerMay
	}
	if from, spawned := r.records[requester]; spawned {
		return t.Name, from.CreatedBy == target.CreatedBy
	}
	return t.Name, true
}

// activeTemplateOf: only an active agent of a configured template, in place,
// matches a "template:" recipient.
func (r *agentRegistry) activeTemplateOf(name string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.records[name]
	if !ok || rec.State != AgentActive {
		return "", false
	}
	if t, ok := r.templates[rec.Template]; !ok || !inPlace(*rec, t) {
		return "", false
	}
	return rec.Template, true
}

// templateOfRecord returns the configured template of spawned agent name,
// in any state but destroyed.
func (r *agentRegistry) templateOfRecord(name string) (TemplateSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.records[name]
	if !ok || rec.State == AgentDestroyed {
		return TemplateSpec{}, false
	}
	t, ok := r.templates[rec.Template]
	return t, ok
}

func (r *agentRegistry) template(name string) (TemplateSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.templates[name]
	return t, ok
}

func (r *agentRegistry) templateList() []TemplateSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]TemplateSpec, 0, len(r.templates))
	for _, t := range r.templates {
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b TemplateSpec) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// recordList returns the records, sorted by creation then name.
func (r *agentRegistry) recordList() []AgentRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.recordListLocked()
}

func (r *agentRegistry) recordListLocked() []AgentRecord {
	out := make([]AgentRecord, 0, len(r.records))
	for _, rec := range r.records {
		out = append(out, *rec)
	}
	slices.SortFunc(out, func(a, b AgentRecord) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

// saveLocked requires mu held.
func (r *agentRegistry) saveLocked() error {
	if r.persist == nil {
		return nil
	}
	return r.persist(AgentsState{Agents: r.recordListLocked(), Retired: r.retiredListLocked()})
}

func (r *agentRegistry) retiredListLocked() []string {
	out := make([]string, 0, len(r.retired))
	for n := range r.retired {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// retiredList is every retired name, sorted.
func (r *agentRegistry) retiredList() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.retiredListLocked()
}

// retire drops destroyed agent name's record, keeping its name as retired,
// and saves. A record that is not destroyed is refused.
func (r *agentRegistry) retire(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.records[name]
	if !ok || rec.State != AgentDestroyed {
		return fmt.Errorf("%s is not a destroyed agent", name)
	}
	if r.retired == nil {
		r.retired = map[string]bool{}
	}
	r.retired[name] = true
	delete(r.records, name)
	delete(r.specs, name)
	if err := r.saveLocked(); err != nil {
		r.records[name] = rec
		delete(r.retired, name)
		return err
	}
	return nil
}

// instanceIDEncoding is lowercase base32 without padding.
var instanceIDEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// newInstanceID draws InstanceIDLen characters from crypto/rand.
func newInstanceID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return instanceIDEncoding.EncodeToString(b[:])[:6], nil
}

// reserve draws a fresh name for template t and records it as spawning,
// saved before it returns. taken reports a name used by anything outside
// the registry (the manager, a revoked or superseded name). A save that
// fails leaves nothing behind.
func (r *agentRegistry) reserve(t TemplateSpec, size MemorySize, createdBy, label string, now time.Time, taken func(string) bool, draw func() (string, error)) (AgentRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, rec := range r.records {
		if rec.Template == t.Name && rec.State != AgentDestroyed {
			n++
		}
	}
	if n >= t.MaxInstances {
		return AgentRecord{}, fmt.Errorf("%w (%d)", errTemplateFull, t.MaxInstances)
	}
	var name string
	for range 32 {
		id, err := draw()
		if err != nil {
			return AgentRecord{}, err
		}
		cand := t.Prefix + "-" + id
		if _, ok := r.records[cand]; ok || r.retired[cand] {
			continue
		}
		if _, ok := r.specs[cand]; ok || taken(cand) {
			continue
		}
		name = cand
		break
	}
	if name == "" {
		return AgentRecord{}, errors.New("could not draw an unused agent name")
	}
	rec := &AgentRecord{Name: name, Template: t.Name, Label: label, CreatedBy: createdBy, CreatedAt: now.UTC(),
		State: AgentSpawning, Dir: strings.TrimSuffix(t.DirRoot, "/") + "/" + name,
		Memory: size.Name, MemoryBytes: size.Bytes}
	r.records[name] = rec
	if err := r.saveLocked(); err != nil {
		delete(r.records, name)
		return AgentRecord{}, err
	}
	r.specs[name] = spawnedSpec(t, *rec)
	return *rec, nil
}

// update applies change to name's record and saves it. On a failed save the
// record is put back as it was. A record that becomes destroyed loses its
// spec.
func (r *agentRegistry) update(name string, change func(rec *AgentRecord)) (AgentRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.records[name]
	if !ok {
		return AgentRecord{}, fmt.Errorf("agent %q has no record", name)
	}
	old := *rec
	change(rec)
	if stateRank(rec.State) < stateRank(old.State) {
		// A record never goes back: a destroy that began wins over a spawn
		// or a reconcile that read the record before it.
		*rec = old
		return old, fmt.Errorf("agent %q is %s; it cannot become %s again: %w", name, old.State, rec.State, errStateBackwards)
	}
	if err := r.saveLocked(); err != nil {
		*rec = old
		return old, err
	}
	if rec.State == AgentDestroyed {
		delete(r.specs, name)
	}
	return *rec, nil
}

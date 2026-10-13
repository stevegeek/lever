package config

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/stevegeek/lever/internal/treebackup"
)

// TreeBackup is the `tree_backup:` block: host-side snapshots of the tree.
// Unknown keys are load errors, at every level of the block, so a typo in a
// budget never passes as the default.
type TreeBackup struct {
	Enabled bool `yaml:"enabled"`
	// AtStop: nil is true.
	AtStop   *bool          `yaml:"at_stop"`
	Keep     TreeBackupKeep `yaml:"keep"`
	Reverify *Days          `yaml:"reverify"`
	LockWait *Days          `yaml:"lock_wait"`
	MinFree  *ByteSize      `yaml:"min_free"`
	Limits   TreeLimits     `yaml:"limits"`
	Exclude  []string       `yaml:"exclude"`
}

type TreeBackupKeep struct {
	Last   *int `yaml:"last"`
	Daily  *int `yaml:"daily"`
	Weekly *int `yaml:"weekly"`
}

// TreeLimits are the snapshot budgets; an unset one takes its default.
type TreeLimits struct {
	File       *ByteSize `yaml:"file"`
	Read       *ByteSize `yaml:"read"`
	Entries    *int64    `yaml:"entries"`
	Manifest   *ByteSize `yaml:"manifest"`
	DirEntries *int64    `yaml:"dir_entries"`
	Depth      *int64    `yaml:"depth"`
	Path       *int64    `yaml:"path"`
	Time       *Days     `yaml:"time"`
}

var (
	treeBackupKeys = []string{"enabled", "at_stop", "keep", "reverify", "lock_wait", "min_free", "limits", "exclude"}
	treeKeepKeys   = []string{"last", "daily", "weekly"}
	treeLimitKeys  = []string{"file", "read", "entries", "manifest", "dir_entries", "depth", "path", "time"}
)

func strictKeys(n *yaml.Node, block string, keys []string) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: %s must be a mapping", n.Line, block)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i]
		if !slices.Contains(keys, k.Value) {
			return fmt.Errorf("line %d: %s: unknown key %q (keys are %s)", k.Line, block, k.Value, strings.Join(keys, ", "))
		}
	}
	return nil
}

func (t *TreeBackup) UnmarshalYAML(n *yaml.Node) error {
	if err := strictKeys(n, "tree_backup", treeBackupKeys); err != nil {
		return err
	}
	type plain TreeBackup
	return n.Decode((*plain)(t))
}

func (k *TreeBackupKeep) UnmarshalYAML(n *yaml.Node) error {
	if err := strictKeys(n, "tree_backup.keep", treeKeepKeys); err != nil {
		return err
	}
	type plain TreeBackupKeep
	return n.Decode((*plain)(k))
}

func (l *TreeLimits) UnmarshalYAML(n *yaml.Node) error {
	if err := strictKeys(n, "tree_backup.limits", treeLimitKeys); err != nil {
		return err
	}
	type plain TreeLimits
	return n.Decode((*plain)(l))
}

// ByteSize is a size in bytes, written as a plain integer or with a unit
// (KiB, MiB, GiB, TiB, KB, MB, GB, TB).
type ByteSize int64

var byteSizeRE = regexp.MustCompile(`^([0-9]{1,15})\s*(B|KiB|MiB|GiB|TiB|KB|MB|GB|TB)?$`)

var byteUnits = map[string]int64{"": 1, "B": 1, "KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40,
	"KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12}

func ParseByteSize(s string) (ByteSize, error) {
	m := byteSizeRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("size %q invalid (want e.g. 512MiB, 2GiB or a number of bytes)", s)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q invalid: %v", s, err)
	}
	u := byteUnits[m[2]]
	if n > (1<<62)/u {
		return 0, fmt.Errorf("size %q is too large", s)
	}
	return ByteSize(n * u), nil
}

func (b *ByteSize) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: a size must be a scalar", n.Line)
	}
	v, err := ParseByteSize(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*b = v
	return nil
}

// Days is a duration that also accepts a whole number of days ("7d") and
// "0" for none.
type Days time.Duration

var daysRE = regexp.MustCompile(`^([0-9]{1,5})d$`)

func ParseDays(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "0" {
		return 0, nil
	}
	if m := daysRE.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("duration %q invalid (want e.g. 90s, 5m, 2h or 7d)", s)
	}
	return d, nil
}

func (d *Days) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: a duration must be a scalar", n.Line)
	}
	v, err := ParseDays(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = Days(v)
	return nil
}

// Tree backup defaults that are not snapshot budgets.
const (
	DefaultTreeKeepLast   = 20
	DefaultTreeKeepDaily  = 7
	DefaultTreeKeepWeekly = 8
	DefaultTreeReverify   = 7 * 24 * time.Hour
	DefaultTreeLockWait   = 2 * time.Minute
	DefaultTreeMinFree    = 2 << 30
)

func (t TreeBackup) EffectiveAtStop() bool { return t.AtStop == nil || *t.AtStop }

func intOr(p *int, d int) int {
	if p == nil {
		return d
	}
	return *p
}

func (t TreeBackup) EffectiveKeep() (last, daily, weekly int) {
	return intOr(t.Keep.Last, DefaultTreeKeepLast), intOr(t.Keep.Daily, DefaultTreeKeepDaily), intOr(t.Keep.Weekly, DefaultTreeKeepWeekly)
}

func daysOr(p *Days, d time.Duration) time.Duration {
	if p == nil {
		return d
	}
	return time.Duration(*p)
}

func (t TreeBackup) EffectiveReverify() time.Duration { return daysOr(t.Reverify, DefaultTreeReverify) }
func (t TreeBackup) EffectiveLockWait() time.Duration { return daysOr(t.LockWait, DefaultTreeLockWait) }

func (t TreeBackup) EffectiveMinFree() int64 {
	if t.MinFree == nil {
		return DefaultTreeMinFree
	}
	return int64(*t.MinFree)
}

// EffectiveLimits are the configured budgets over the defaults. Time is the
// budget for snapshots at stop and destroy; `lever tree snapshot` overrides
// it.
func (t TreeBackup) EffectiveLimits() treebackup.Limits {
	l := treebackup.DefaultLimits()
	set := func(dst *int64, p *ByteSize) {
		if p != nil {
			*dst = int64(*p)
		}
	}
	setN := func(dst *int64, p *int64) {
		if p != nil {
			*dst = *p
		}
	}
	set(&l.File, t.Limits.File)
	set(&l.Read, t.Limits.Read)
	set(&l.Manifest, t.Limits.Manifest)
	setN(&l.Entries, t.Limits.Entries)
	setN(&l.DirEntries, t.Limits.DirEntries)
	setN(&l.Depth, t.Limits.Depth)
	setN(&l.Path, t.Limits.Path)
	if t.Limits.Time != nil {
		l.Time = time.Duration(*t.Limits.Time)
	}
	return l
}

func (t TreeBackup) validate() error {
	last, daily, weekly := t.EffectiveKeep()
	if last < 1 {
		return fmt.Errorf("config: tree_backup.keep.last must be at least 1")
	}
	if daily < 0 || weekly < 0 {
		return fmt.Errorf("config: tree_backup.keep.daily and keep.weekly must not be negative")
	}
	for _, d := range []struct {
		key string
		v   *Days
	}{{"reverify", t.Reverify}, {"lock_wait", t.LockWait}, {"limits.time", t.Limits.Time}} {
		if d.v != nil && *d.v < 0 {
			return fmt.Errorf("config: tree_backup.%s must not be negative", d.key)
		}
	}
	if t.MinFree != nil && *t.MinFree < 0 {
		return fmt.Errorf("config: tree_backup.min_free must not be negative")
	}
	l := t.EffectiveLimits()
	for _, c := range []struct {
		key      string
		v, upper int64
	}{
		{"file", l.File, 1 << 50},
		{"read", l.Read, 1 << 50},
		{"entries", l.Entries, treebackup.MaxEntriesCap},
		{"manifest", l.Manifest, treebackup.MaxManifestCap},
		{"dir_entries", l.DirEntries, treebackup.MaxEntriesCap},
		{"depth", l.Depth, 4096},
		{"path", l.Path, treebackup.MaxPathCap},
	} {
		if c.v <= 0 || c.v > c.upper {
			return fmt.Errorf("config: tree_backup.limits.%s must be positive and at most %d", c.key, c.upper)
		}
	}
	for _, p := range t.Exclude {
		if err := ValidTreeExclude(p); err != nil {
			return fmt.Errorf("config: tree_backup.exclude %q: %w", p, err)
		}
	}
	return nil
}

// ValidTreeExclude checks one exclude pattern: relative, no "..", no empty
// or "." component, valid path.Match syntax per component.
func ValidTreeExclude(p string) error {
	if p == "" {
		return fmt.Errorf("empty pattern")
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("must be relative to the tree")
	}
	if strings.ContainsRune(p, 0) {
		return fmt.Errorf("contains NUL")
	}
	for _, c := range strings.Split(p, "/") {
		switch c {
		case "":
			return fmt.Errorf("has an empty component")
		case ".", "..":
			return fmt.Errorf("has a %q component", c)
		}
		if _, err := path.Match(c, ""); err != nil {
			return fmt.Errorf("invalid pattern syntax: %v", err)
		}
	}
	return nil
}

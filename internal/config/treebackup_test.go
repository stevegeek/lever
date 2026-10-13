package config

import (
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/treebackup"
)

const treeBase = "name: demo\nbackend: orbstack\ntree: ws\nmanager: {}\n"

func TestTreeBackupDefaults(t *testing.T) {
	app, err := LoadNoHostChecks(writeConfig(t, treeBase))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tb := app.TreeBackup
	if tb.Enabled {
		t.Fatal("tree_backup must be off by default")
	}
	if !tb.EffectiveAtStop() {
		t.Fatal("at_stop must default to true")
	}
	if l, d, w := tb.EffectiveKeep(); l != 20 || d != 7 || w != 8 {
		t.Fatalf("keep = %d/%d/%d, want 20/7/8", l, d, w)
	}
	if tb.EffectiveReverify() != 7*24*time.Hour || tb.EffectiveLockWait() != 2*time.Minute || tb.EffectiveMinFree() != 2<<30 {
		t.Fatalf("defaults: reverify %v lock_wait %v min_free %d", tb.EffectiveReverify(), tb.EffectiveLockWait(), tb.EffectiveMinFree())
	}
	if tb.EffectiveLimits() != treebackup.DefaultLimits() {
		t.Fatalf("limits = %+v, want the defaults", tb.EffectiveLimits())
	}
	if tb.EffectiveMaxStore() != 32<<30 {
		t.Fatalf("max_store %d, want 32 GiB", tb.EffectiveMaxStore())
	}
}

func TestTreeBackupLoad(t *testing.T) {
	body := treeBase + `tree_backup:
  enabled: true
  at_stop: false
  keep: {last: 5, daily: 0, weekly: 2}
  reverify: 1d
  lock_wait: 30s
  min_free: 512MiB
  max_store: 0
  limits:
    file: 1GiB
    read: 100MB
    new: 50MB
    entries: 2000
    manifest: 64MiB
    dir_entries: 50
    depth: 16
    path: 1024
    time: 0
  exclude: [node_modules, "build/*.o"]
`
	app, err := LoadNoHostChecks(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tb := app.TreeBackup
	if !tb.Enabled || tb.EffectiveAtStop() {
		t.Fatalf("enabled %v at_stop %v", tb.Enabled, tb.EffectiveAtStop())
	}
	if l, d, w := tb.EffectiveKeep(); l != 5 || d != 0 || w != 2 {
		t.Fatalf("keep = %d/%d/%d", l, d, w)
	}
	if tb.EffectiveReverify() != 24*time.Hour || tb.EffectiveLockWait() != 30*time.Second || tb.EffectiveMinFree() != 512<<20 {
		t.Fatalf("reverify %v lock_wait %v min_free %d", tb.EffectiveReverify(), tb.EffectiveLockWait(), tb.EffectiveMinFree())
	}
	if tb.EffectiveMaxStore() != 0 {
		t.Fatalf("max_store %d, want 0 (no cap)", tb.EffectiveMaxStore())
	}
	want := treebackup.Limits{File: 1 << 30, Read: 100e6, New: 50e6, Entries: 2000, Manifest: 64 << 20, DirEntries: 50, Depth: 16, Path: 1024, Time: 0}
	if got := tb.EffectiveLimits(); got != want {
		t.Fatalf("limits = %+v, want %+v", got, want)
	}
	if len(tb.Exclude) != 2 {
		t.Fatalf("exclude = %q", tb.Exclude)
	}
}

func TestTreeBackupRejected(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"unknown key":          {"tree_backup:\n  enable: true\n", `tree_backup: unknown key "enable"`},
		"unknown limit":        {"tree_backup:\n  limits: {files: 1}\n", `tree_backup.limits: unknown key "files"`},
		"unknown keep":         {"tree_backup:\n  keep: {monthly: 1}\n", `tree_backup.keep: unknown key "monthly"`},
		"not a mapping":        {"tree_backup: true\n", "tree_backup must be a mapping"},
		"keep last zero":       {"tree_backup:\n  keep: {last: 0}\n", "keep.last must be at least 1"},
		"negative daily":       {"tree_backup:\n  keep: {daily: -1}\n", "must not be negative"},
		"bad size":             {"tree_backup:\n  min_free: 2 gigs\n", "size \"2 gigs\" invalid"},
		"bad duration":         {"tree_backup:\n  reverify: 1w\n", "duration \"1w\" invalid"},
		"negative duration":    {"tree_backup:\n  lock_wait: -1m\n", "lock_wait must not be negative"},
		"path over cap":        {"tree_backup:\n  limits: {path: 4097}\n", "limits.path must be positive and at most 4096"},
		"manifest over cap":    {"tree_backup:\n  limits: {manifest: 5GiB}\n", "limits.manifest must be positive"},
		"entries over cap":     {"tree_backup:\n  limits: {entries: 10000001}\n", "limits.entries must be positive"},
		"zero file":            {"tree_backup:\n  limits: {file: 0}\n", "limits.file must be positive"},
		"zero new":             {"tree_backup:\n  limits: {new: 0}\n", "limits.new must be positive"},
		"absolute exclude":     {"tree_backup:\n  exclude: [/etc]\n", "must be relative"},
		"dotdot exclude":       {"tree_backup:\n  exclude: [a/../b]\n", `".." component`},
		"empty component":      {"tree_backup:\n  exclude: [a//b]\n", "empty component"},
		"bad pattern":          {"tree_backup:\n  exclude: [\"a/[\"]\n", "invalid pattern syntax"},
		"empty exclude":        {"tree_backup:\n  exclude: [\"\"]\n", "empty pattern"},
		"dot exclude":          {"tree_backup:\n  exclude: [./a]\n", `"." component`},
		"size scalar required": {"tree_backup:\n  min_free: [1]\n", "a size must be a scalar"},
	}
	for label, c := range cases {
		t.Run(label, func(t *testing.T) { rejectNoHost(t, treeBase+c.body, c.want) })
	}
}

func TestParseByteSizeAndDays(t *testing.T) {
	for in, want := range map[string]int64{"0": 0, "1": 1, "1KiB": 1024, "2GiB": 2 << 30, "3MB": 3e6, "1 TiB": 1 << 40} {
		got, err := ParseByteSize(in)
		if err != nil || int64(got) != want {
			t.Errorf("ParseByteSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := ParseByteSize("99999999999999TiB"); err == nil {
		t.Error("an overflowing size must be refused")
	}
	for in, want := range map[string]time.Duration{"0": 0, "7d": 7 * 24 * time.Hour, "90s": 90 * time.Second, "2h": 2 * time.Hour} {
		got, err := ParseDays(in)
		if err != nil || got != want {
			t.Errorf("ParseDays(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
}

package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/lever/captool"
)

func TestPushBackstop(t *testing.T) {
	b := pushBackstop(map[string]bool{"stevegeek/lever": true}, "agent/")
	ok := map[string]string{"repo": "stevegeek/lever", "branch": "agent/x", "bundle": "x.bundle"}
	if err := b(captool.ValidatedContext{Operation: "push"}, ok); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []map[string]string{
		{"repo": "stevegeek/lever", "branch": "main", "bundle": "x.bundle"},
		{"repo": "x/y", "branch": "agent/x", "bundle": "x.bundle"},
		{"repo": "stevegeek/lever", "branch": "agent/x", "bundle": "../x.bundle"},
	} {
		if err := b(captool.ValidatedContext{Operation: "push"}, bad); err == nil {
			t.Errorf("backstop must refuse %v", bad)
		}
	}
	if err := b(captool.ValidatedContext{Operation: "delete"}, ok); err == nil {
		t.Error("backstop must refuse a non-push operation")
	}
}

func TestAsResult(t *testing.T) {
	v, err := asResult(nil, errors.New("branch moved"))
	m, ok := v.(map[string]any)
	if err != nil || !ok || m["ok"] != false || m["error"] != "branch moved" {
		t.Fatalf("got %v %v", v, err)
	}
	v, err = asResult("x", nil)
	if err != nil || v != "x" {
		t.Fatalf("got %v %v", v, err)
	}
}

func TestParseFlagsRequired(t *testing.T) {
	_, err := parseFlags([]string{"-backend", "127.0.0.1:0", "-admin", "http://127.0.0.1:1"})
	if err == nil || !strings.Contains(err.Error(), "-tree") {
		t.Fatalf("want a missing -tree error, got %v", err)
	}
	_, err = parseFlags([]string{"-tree", "rel/path", "-state", "/s", "-app-id", "1", "-installation-id", "2", "-app-key", "/k", "-repos", "a/b"})
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("want an absolute-path error, got %v", err)
	}
}

func TestParseFlagsRefusesStateInsideTree(t *testing.T) {
	tree := t.TempDir()
	base := []string{"-app-id", "1", "-installation-id", "2", "-app-key", "/k", "-repos", "a/b"}
	for _, state := range []string{filepath.Join(tree, "st"), filepath.Join(tree, "a", "b", "st"), tree} {
		_, err := parseFlags(append([]string{"-tree", tree, "-state", state}, base...))
		if err == nil || !strings.Contains(err.Error(), "inside") {
			t.Errorf("state %s: want an inside-tree refusal, got %v", state, err)
		}
	}
	// A symlinked path to the tree is the same tree.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(tree, link); err != nil {
		t.Fatal(err)
	}
	if _, err := parseFlags(append([]string{"-tree", tree, "-state", filepath.Join(link, "st")}, base...)); err == nil {
		t.Error("state under a symlink to the tree must be refused")
	}
	if _, err := parseFlags(append([]string{"-tree", tree, "-state", filepath.Join(t.TempDir(), "st")}, base...)); err != nil {
		t.Errorf("separate state must be accepted: %v", err)
	}
}

func TestParseFlagsRefusesAppKeyInsideTree(t *testing.T) {
	tree := t.TempDir()
	base := []string{"-tree", tree, "-state", filepath.Join(t.TempDir(), "st"), "-app-id", "1", "-installation-id", "2", "-repos", "a/b"}
	key := filepath.Join(tree, "secrets", "app.pem")
	if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := parseFlags(append(base, "-app-key", key))
	if err == nil || !strings.Contains(err.Error(), "-app-key") || !strings.Contains(err.Error(), "inside") {
		t.Fatalf("want an -app-key inside-tree refusal, got %v", err)
	}
	// A symlink outside the tree that points at a key inside it is the same file.
	link := filepath.Join(t.TempDir(), "app.pem")
	if err := os.Symlink(key, link); err != nil {
		t.Fatal(err)
	}
	if _, err := parseFlags(append(base, "-app-key", link)); err == nil {
		t.Error("a key reached through a symlink into the tree must be refused")
	}
	if _, err := parseFlags(append(base, "-app-key", filepath.Join(t.TempDir(), "app.pem"))); err != nil {
		t.Errorf("a key outside the tree must be accepted: %v", err)
	}
}

func baseFlags(t *testing.T) []string {
	t.Helper()
	return []string{"-tree", t.TempDir(), "-state", filepath.Join(t.TempDir(), "st"), "-app-id", "1",
		"-installation-id", "2", "-app-key", filepath.Join(t.TempDir(), "app.pem"), "-repos", "stevegeek/lever"}
}

func opNames(ops []captool.Operation) []string {
	var n []string
	for _, o := range ops {
		n = append(n, o.Name)
	}
	return n
}

func TestParseFlagsOpsDefaultIsPushOnly(t *testing.T) {
	o, err := parseFlags(baseFlags(t))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(o.ops, []string{"push"}) || !slices.Equal(o.mergeMethods, []string{"squash"}) ||
		!o.mergeRequireReview || o.appSlug != "" || o.mergeChecks.Any || o.mergeChecks.None || len(o.mergeChecks.Names) != 0 || o.textMax != 8000 || !o.releaseTag.MatchString("v1.2.3") {
		t.Fatalf("defaults %+v", o)
	}
	ops := operations(o, nil, nil)
	if !slices.Equal(opNames(ops), []string{"push"}) {
		t.Fatalf("default operations %v", opNames(ops))
	}
	// push is the same operation as before -ops existed.
	push := ops[0]
	if len(push.Params) != 3 || push.Params[0].Name != "repo" || push.Params[1].Name != "branch" || push.Params[2].Name != "bundle" ||
		push.CaveatParam["repo"] != "repo" || len(push.CaveatParam) != 1 || push.Backstop == nil {
		t.Fatalf("push operation changed: %+v", push)
	}
}

func TestParseFlagsOps(t *testing.T) {
	o, err := parseFlags(append(baseFlags(t), "-ops", "release, push,pr_open,push,pr_merge,pr_status", "-merge-checks", "any"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"push", "pr_open", "pr_status", "pr_merge", "release"}
	if !slices.Equal(o.ops, want) || !slices.Equal(opNames(operations(o, nil, nil)), want) {
		t.Fatalf("ops %v", o.ops)
	}
	o, err = parseFlags(append(baseFlags(t), "-ops", "pr_status"))
	if err != nil {
		t.Fatal(err)
	}
	ops := operations(o, nil, nil)
	if !slices.Equal(opNames(ops), []string{"pr_status"}) {
		t.Fatalf("an operation -ops leaves out must not be registered: %v", opNames(ops))
	}
	for _, op := range operations(mustParse(t, "-ops", strings.Join(want, ","), "-merge-checks", "test"), nil, nil) {
		if op.CaveatParam["repo"] != "repo" || op.Backstop == nil || op.Handler == nil {
			t.Errorf("%s: want a repo caveat, a backstop and a handler", op.Name)
		}
	}
}

func mustParse(t *testing.T, extra ...string) opts {
	t.Helper()
	o, err := parseFlags(append(baseFlags(t), extra...))
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestParseFlagsOpsRefusals(t *testing.T) {
	for _, c := range []struct{ flags []string }{
		{[]string{"-ops", ""}},
		{[]string{"-ops", "push,delete"}},
		{[]string{"-ops", "pr_create"}},
		{[]string{"-merge-methods", "squash,fast-forward"}},
		{[]string{"-merge-methods", ""}},
		{[]string{"-merge-checks", "any,test"}},
		{[]string{"-merge-checks", " "}},
		{[]string{"-ops", "pr_merge"}},
		{[]string{"-ops", "push,pr_merge", "-merge-checks", ""}},
		{[]string{"-release-tag", "("}},
		{[]string{"-release-tag", ""}},
		{[]string{"-text-max", "0"}},
		{[]string{"-text-max", "65537"}},
	} {
		if _, err := parseFlags(append(baseFlags(t), c.flags...)); err == nil {
			t.Errorf("%v must be refused", c.flags)
		}
	}
	o := mustParse(t, "-merge-methods", "rebase,merge", "-merge-checks", "test,e2e", "-release-tag", `v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?`, "-text-max", "100")
	if !slices.Equal(o.mergeMethods, []string{"rebase", "merge"}) || !slices.Equal(o.mergeChecks.Names, []string{"test", "e2e"}) || o.textMax != 100 {
		t.Fatalf("opts %+v", o)
	}
	// The pattern matches the whole tag.
	if !o.releaseTag.MatchString("v1.2.3-rc.1") || o.releaseTag.MatchString("xv1.2.3") || o.releaseTag.MatchString("v1.2.3-rc.1x") {
		t.Fatalf("release pattern %s", o.releaseTag)
	}
}

func TestParseFlagsMergeChecksIsExplicit(t *testing.T) {
	_, err := parseFlags(append(baseFlags(t), "-ops", "push,pr_merge"))
	if err == nil || !strings.Contains(err.Error(), "-merge-checks must be set explicitly") {
		t.Fatalf("want an explicit -merge-checks refusal, got %v", err)
	}
	for in, want := range map[string]func(o opts) bool{
		"any":     func(o opts) bool { return o.mergeChecks.Any },
		"none":    func(o opts) bool { return o.mergeChecks.None },
		"ci/test": func(o opts) bool { return slices.Equal(o.mergeChecks.Names, []string{"ci/test"}) },
	} {
		if o := mustParse(t, "-ops", "pr_merge", "-merge-checks", in); !want(o) {
			t.Errorf("-merge-checks %s: %+v", in, o.mergeChecks)
		}
	}
	// Without pr_merge the flag is not needed.
	mustParse(t, "-ops", "push,pr_open,pr_status,release")
}

func TestReleaseOperationHasNoNotes(t *testing.T) {
	ops := operations(mustParse(t, "-ops", "release"), nil, nil)
	if len(ops) != 1 || len(ops[0].Params) != 2 || ops[0].Params[0].Name != "repo" || ops[0].Params[1].Name != "tag" {
		t.Fatalf("release params %+v", ops)
	}
	if !strings.Contains(ops[0].Description, "no GitHub release") {
		t.Fatalf("description %q", ops[0].Description)
	}
	// A stale caller that still sends notes gets a named refusal before any
	// GitHub call (the Ops is nil: a call would panic).
	v, err := ops[0].Handler(captool.ValidatedContext{Operation: "release"}, map[string]string{"repo": "stevegeek/lever", "tag": "v1.2.3", "notes": ""})
	m, _ := v.(map[string]any)
	if err != nil || m["ok"] != false || !strings.Contains(m["error"].(string), "release takes no notes") {
		t.Fatalf("notes: %v %v", v, err)
	}
}

func TestMergeRequireReview(t *testing.T) {
	o := mustParse(t, "-ops", "pr_merge", "-merge-checks", "any")
	if !o.mergeRequireReview || len(startupWarnings(o)) != 0 {
		t.Fatalf("the review gate must be on by default, without a warning: %+v", o)
	}
	if d := operations(o, nil, nil)[0].Description; !strings.Contains(d, "a person approved") {
		t.Fatalf("description %q", d)
	}
	o = mustParse(t, "-ops", "pr_merge", "-merge-checks", "any", "-merge-require-review=false")
	w := startupWarnings(o)
	if o.mergeRequireReview || len(w) != 1 || !strings.Contains(w[0], "without a person's approving review") {
		t.Fatalf("off: %+v %v", o, w)
	}
	if d := operations(o, nil, nil)[0].Description; !strings.Contains(d, "no review is required") {
		t.Fatalf("description %q", d)
	}
	// No warning when pr_merge is not offered.
	if w := startupWarnings(mustParse(t, "-merge-require-review=false")); len(w) != 0 {
		t.Fatalf("warnings %v", w)
	}
	if o := mustParse(t, "-app-slug", "lever-dev"); o.appSlug != "lever-dev" {
		t.Fatalf("slug %q", o.appSlug)
	}
	for _, bad := range []string{"-x", "a/b", "a b", "a[bot]"} {
		if _, err := parseFlags(append(baseFlags(t), "-app-slug", bad)); err == nil {
			t.Errorf("-app-slug %q must be refused", bad)
		}
	}
}

func TestOptInBackstops(t *testing.T) {
	o := mustParse(t, "-ops", "push,pr_open,pr_status,pr_merge,release", "-merge-methods", "squash,merge", "-merge-checks", "none", "-text-max", "10")
	ops := map[string]captool.Operation{}
	for _, op := range operations(o, nil, nil) {
		ops[op.Name] = op
	}
	for _, c := range []struct {
		op   string
		args map[string]string
		ok   bool
	}{
		{"pr_open", map[string]string{"repo": "stevegeek/lever", "branch": "agent/x", "title": "t", "body": "b\n"}, true},
		{"pr_open", map[string]string{"repo": "x/y", "branch": "agent/x", "title": "t"}, false},
		{"pr_open", map[string]string{"repo": "stevegeek/lever", "branch": "main", "title": "t"}, false},
		{"pr_open", map[string]string{"repo": "stevegeek/lever", "branch": "agent/x", "title": ""}, false},
		{"pr_open", map[string]string{"repo": "stevegeek/lever", "branch": "agent/x", "title": "t", "body": "12345678901"}, false},
		{"pr_open", map[string]string{"repo": "stevegeek/lever", "branch": "agent/x", "title": "t", "body": "a\x07"}, false},
		{"pr_open", map[string]string{"repo": "stevegeek/lever", "branch": "agent/x", "title": "t", "body": "@o/t"}, false},
		{"pr_open", map[string]string{"repo": "stevegeek/lever", "branch": "agent/x", "title": "fixes #1"}, false},
		{"pr_status", map[string]string{"repo": "stevegeek/lever", "number": "12"}, true},
		{"pr_status", map[string]string{"repo": "stevegeek/lever", "number": "12/files"}, false},
		{"pr_merge", map[string]string{"repo": "stevegeek/lever", "number": "12"}, true},
		{"pr_merge", map[string]string{"repo": "stevegeek/lever", "number": "12", "method": "merge"}, true},
		{"pr_merge", map[string]string{"repo": "stevegeek/lever", "number": "12", "method": "rebase"}, false},
		{"pr_merge", map[string]string{"repo": "x/y", "number": "12"}, false},
		{"release", map[string]string{"repo": "stevegeek/lever", "tag": "v1.2.3"}, true},
		{"release", map[string]string{"repo": "stevegeek/lever", "tag": "1.2.3"}, false},
		{"release", map[string]string{"repo": "x/y", "tag": "v1.2.3"}, false},
	} {
		err := ops[c.op].Backstop(captool.ValidatedContext{Operation: c.op}, c.args)
		if (err == nil) != c.ok {
			t.Errorf("%s %v: err %v, want ok=%v", c.op, c.args, err, c.ok)
		}
	}
	ok := map[string]string{"repo": "stevegeek/lever", "number": "12"}
	if err := ops["pr_status"].Backstop(captool.ValidatedContext{Operation: "pr_merge"}, ok); err == nil {
		t.Error("a backstop must refuse another operation")
	}
}

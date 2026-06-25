package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTmp(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	_ = os.MkdirAll(filepath.Join(tree, "groves", "appa"), 0o755)
	p := filepath.Join(dir, "app.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadValid(t *testing.T) {
	p := writeTmp(t, `name: demo
backend: orbstack
tree: ./tree
manager:
  image: scionlocal/lever-claude:latest
  allow_ports: [3305]
groves:
  - name: appa
    dir: groves/appa
`)
	app, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if app.Name != "demo" || app.Backend != "orbstack" {
		t.Fatalf("app=%+v", app)
	}
	if len(app.Groves) != 1 || app.Groves[0].Name != "appa" {
		t.Fatalf("groves=%+v", app.Groves)
	}
	if app.Manager.Image == "" || len(app.Manager.AllowPorts) != 1 {
		t.Fatalf("manager=%+v", app.Manager)
	}
	if !filepath.IsAbs(app.Tree) {
		t.Fatalf("tree not absolute: %q", app.Tree)
	}
}

func TestScionSourceResolved(t *testing.T) {
	// relative path → resolved under the config dir and absolute
	p := writeTmp(t, "name: x\nbackend: orbstack\ntree: ./tree\nmanager: {}\nscion:\n  source: relative/scion\n")
	app, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := filepath.Join(filepath.Dir(p), "relative", "scion")
	if app.Scion.Source != want {
		t.Fatalf("relative scion source: want %q got %q", want, app.Scion.Source)
	}
	if !filepath.IsAbs(app.Scion.Source) {
		t.Fatalf("scion source not absolute: %q", app.Scion.Source)
	}

	// absolute path → stays as-is
	p = writeTmp(t, "name: x\nbackend: orbstack\ntree: ./tree\nmanager: {}\nscion:\n  source: /abs/scion-src\n")
	app, err = Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if app.Scion.Source != "/abs/scion-src" {
		t.Fatalf("absolute scion source: want /abs/scion-src got %q", app.Scion.Source)
	}

	// empty → stays empty
	p = writeTmp(t, "name: x\nbackend: orbstack\ntree: ./tree\nmanager: {}\n")
	app, err = Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if app.Scion.Source != "" {
		t.Fatalf("empty scion source: want \"\" got %q", app.Scion.Source)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialFileResolved(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "lever.yaml")
	mustWriteFile(t, cfg, "name: a\nbackend: orbstack\ntree: ws\nmanager:\n  credential_file: secrets/tok\n")
	app, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "secrets/tok"); app.Manager.CredentialFile != want {
		t.Fatalf("CredentialFile = %q, want %q", app.Manager.CredentialFile, want)
	}

	home, _ := os.UserHomeDir()
	cfg2 := filepath.Join(dir, "l2.yaml")
	mustWriteFile(t, cfg2, "name: a\nbackend: orbstack\ntree: ws\nmanager:\n  credential_file: ~/.scion/oauth-token\n")
	app2, err := Load(cfg2)
	if err != nil {
		t.Fatal(err)
	}
	if app2.Manager.CredentialFile != filepath.Join(home, ".scion/oauth-token") {
		t.Fatalf("CredentialFile ~ expand = %q", app2.Manager.CredentialFile)
	}

	cfg3 := filepath.Join(dir, "l3.yaml")
	mustWriteFile(t, cfg3, "name: a\nbackend: orbstack\ntree: ws\n")
	app3, _ := Load(cfg3)
	if app3.Manager.CredentialFile != "" {
		t.Fatalf("empty CredentialFile = %q", app3.Manager.CredentialFile)
	}
}

func TestValidateRejectsUnknownBackend(t *testing.T) {
	p := writeTmp(t, "name: x\nbackend: vmware\ntree: ./tree\nmanager: {}\n")
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for unknown backend")
	}
}

func TestValidateRejectsGroveOutsideTree(t *testing.T) {
	p := writeTmp(t, "name: x\nbackend: orbstack\ntree: ./tree\nmanager: {}\ngroves:\n  - name: bad\n    dir: ../escape\n")
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for grove dir outside tree")
	}
}

func TestGroveImageFallsBackToManagerImage(t *testing.T) {
	app := &App{
		Manager: Manager{Image: "scionlocal/lever-claude:latest"},
		Groves: []Grove{
			{Name: "plain", Dir: "groves/plain"},
			{Name: "custom", Dir: "groves/custom", Image: "scionlocal/lever-rust:latest"},
		},
	}
	g0, _ := app.GroveByName("plain")
	if got := app.GroveImage(g0); got != "scionlocal/lever-claude:latest" {
		t.Fatalf("plain grove image = %q, want manager image", got)
	}
	g1, _ := app.GroveByName("custom")
	if got := app.GroveImage(g1); got != "scionlocal/lever-rust:latest" {
		t.Fatalf("custom grove image = %q, want override", got)
	}
	if _, ok := app.GroveByName("missing"); ok {
		t.Fatal("GroveByName(missing) should be false")
	}
}

func TestLoadParsesGroveImage(t *testing.T) {
	p := writeTmp(t, `name: demo
backend: orbstack
tree: ./tree
manager:
  image: scionlocal/lever-claude:latest
groves:
  - name: appa
    dir: groves/appa
    image: scionlocal/lever-rust:latest
`)
	app, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if app.Groves[0].Image != "scionlocal/lever-rust:latest" {
		t.Fatalf("grove image = %q", app.Groves[0].Image)
	}
}

func TestLoadConfinesTree(t *testing.T) {
	// tree must be a confined relative subdir: reject "." (root==mount),
	// absolute, "..", and empty. A normal subdir is accepted and joined.
	base := "name: demo\nbackend: orbstack\nmanager: {}\n"
	for _, bad := range []string{".", "/abs/tree", "../escape", ""} {
		body := base
		if bad != "" {
			body += "tree: " + bad + "\n"
		}
		dir := t.TempDir()
		p := filepath.Join(dir, CanonicalName)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Fatalf("tree %q should be rejected", bad)
		}
	}
	dir := t.TempDir()
	p := filepath.Join(dir, CanonicalName)
	if err := os.WriteFile(p, []byte(base+"tree: workspace\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	wantDir, _ := filepath.Abs(filepath.Join(dir, "workspace"))
	if app.Tree != wantDir {
		t.Fatalf("tree = %q, want %q", app.Tree, wantDir)
	}
}

func TestValidateRejectsBadNameImagePrompt(t *testing.T) {
	cases := map[string]string{
		"bad name":         "name: Bad_Name\nbackend: orbstack\ntree: ws\nmanager: {}\n",
		"bad image":        "name: demo\nbackend: orbstack\ntree: ws\nmanager:\n  image: \"bad image;rm\"\n",
		"prompt traversal": "name: demo\nbackend: orbstack\ntree: ws\nmanager:\n  prompt_file: ../../etc/shadow\n",
		"bad grove name":   "name: demo\nbackend: orbstack\ntree: ws\ngroves:\n  - name: Bad\n    dir: groves/x\n",
	}
	for label, body := range cases {
		p := writeTmp(t, body)
		if _, err := Load(p); err == nil {
			t.Fatalf("%s should be rejected", label)
		}
	}
}

func TestManagerPromptPathIsRootRelative(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, CanonicalName)
	body := "name: demo\nbackend: orbstack\ntree: workspace\nmanager:\n  prompt_file: boot.md\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	app, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want, _ := filepath.Abs(filepath.Join(dir, "boot.md")) // root, NOT under workspace/
	if app.ManagerPromptPath() != want {
		t.Fatalf("prompt path = %q, want %q (root-relative, outside the mount)", app.ManagerPromptPath(), want)
	}
}

func TestManifestRoundTrip(t *testing.T) {
	app := &App{
		Manager: Manager{Image: "mgr:1"},
		Groves: []Grove{
			{Name: "a", Dir: "groves/a"},                 // inherits mgr:1
			{Name: "b", Dir: "groves/b", Image: "alt:1"}, // override
		},
	}
	dir := t.TempDir()
	if err := WriteManifest(dir, ManifestFromApp(app)); err != nil {
		t.Fatal(err)
	}
	got, err := LoadManifest(filepath.Join(dir, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if img, ok := got.ImageFor("a"); !ok || img != "mgr:1" {
		t.Fatalf("a image = %q,%v want mgr:1", img, ok)
	}
	if img, ok := got.ImageFor("b"); !ok || img != "alt:1" {
		t.Fatalf("b image = %q,%v want alt:1", img, ok)
	}
	if _, ok := got.ImageFor("missing"); ok {
		t.Fatal("unknown grove should not resolve")
	}
}

func TestSecurityImagePolicy(t *testing.T) {
	mk := func(sec, img string) string {
		return "name: demo\nbackend: orbstack\ntree: ws\n" + sec + "manager:\n  image: " + img + "\n"
	}
	allowlist := "security:\n  allowed_image_registries: [scionlocal]\n"
	digest := "security:\n  require_image_digest: true\n"
	both := "security:\n  allowed_image_registries: [scionlocal]\n  require_image_digest: true\n"
	pinned := "scionlocal/lever-claude@sha256:" + strings.Repeat("a", 64)

	cases := []struct {
		name   string
		body   string
		wantOK bool
	}{
		{"no policy, any registry tag", mk("", "ghcr.io/who/x:latest"), true},
		{"allowlist permits scionlocal", mk(allowlist, "scionlocal/lever-claude:latest"), true},
		{"allowlist rejects other registry", mk(allowlist, "ghcr.io/who/x:latest"), false},
		{"allowlist not fooled by prefix", mk(allowlist, "scionlocalevil/x:latest"), false},
		{"require digest rejects tag", mk(digest, "scionlocal/lever-claude:latest"), false},
		{"require digest accepts pin", mk(digest, pinned), true},
		{"both accept allowed+pinned", mk(both, pinned), true},
		{"both reject allowed+unpinned", mk(both, "scionlocal/lever-claude:latest"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeTmp(t, tc.body)
			_, err := Load(p)
			if tc.wantOK && err != nil {
				t.Fatalf("expected OK, got %v", err)
			}
			if !tc.wantOK && err == nil {
				t.Fatal("expected rejection, got nil")
			}
		})
	}
}

func TestSecurityImagePolicyAppliesToGroves(t *testing.T) {
	body := "name: demo\nbackend: orbstack\ntree: ws\n" +
		"security:\n  allowed_image_registries: [scionlocal]\n" +
		"manager:\n  image: scionlocal/mgr:latest\n" +
		"groves:\n  - name: g\n    dir: groves/g\n    image: ghcr.io/who/x:latest\n"
	if _, err := Load(writeTmp(t, body)); err == nil {
		t.Fatal("grove image outside the allowlist should be rejected")
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "lever.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const baseCfg = `
name: demo
backend: orbstack
tree: work
manager:
  image: scionlocal/mgr
  delegate:
    - {tool: db, op: read, to: [worker]}
groves:
  - {name: worker, dir: work, obtain: []}
broker:
  jail_port: 8443
  admin_port: 8444
  tools:
    - name: db
      command: [lever-tool-db, -dsn, "file:ref.db"]
      backend: 127.0.0.1:3201
      operations:
        - {name: read, caveat_param: {table: table, filter: filter}}
      allowed_values: {table: [A, B]}
`

func TestLoadParsesBrokerAndGrants(t *testing.T) {
	app, err := Load(writeConfig(t, baseCfg))
	if err != nil {
		t.Fatal(err)
	}
	if app.Broker.JailPort != 8443 || app.Broker.AdminPort != 8444 {
		t.Fatalf("ports = %+v", app.Broker)
	}
	if app.ManagerCN() != "manager" {
		t.Fatalf("default manager CN = %q", app.ManagerCN())
	}
	if len(app.Manager.Delegate) != 1 || app.Manager.Delegate[0].To[0] != "worker" {
		t.Fatalf("manager delegate = %+v", app.Manager.Delegate)
	}
	if len(app.Broker.Tools) != 1 || app.Broker.Tools[0].Operations[0].CaveatParam["table"] != "table" {
		t.Fatalf("tool = %+v", app.Broker.Tools)
	}
}

func TestLoadRejectsGrantWithUnknownTool(t *testing.T) {
	bad := baseCfg + "\n# trailing\n"
	bad = replaceFirst(bad, "tool: db, op: read, to: [worker]", "tool: nope, op: read, to: [worker]")
	if _, err := Load(writeConfig(t, bad)); err == nil {
		t.Fatal("a delegate grant referencing an undeclared tool must be rejected")
	}
}

func TestLoadRejectsDelegateToUnknownAgent(t *testing.T) {
	bad := replaceFirst(baseCfg, "to: [worker]", "to: [ghost]")
	if _, err := Load(writeConfig(t, bad)); err == nil {
		t.Fatal("a delegate.to naming an undeclared agent must be rejected")
	}
}

func replaceFirst(s, old, new string) string {
	i := indexOf(s, old)
	if i < 0 {
		return s
	}
	return s[:i] + new + s[i+len(old):]
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

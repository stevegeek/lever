package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTmp(t *testing.T, body string) string {
	t.Helper()
	// The default llm_auth is api-key, which requires a broker.api_key_file these
	// minimal fixtures don't provide. Tests that don't exercise llm_auth default to
	// subscription; tests that care set broker:/llm_auth: explicitly.
	if !strings.Contains(body, "llm_auth") && !strings.Contains(body, "broker:") {
		body += "broker:\n  llm_auth: subscription\n"
	}
	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	_ = os.MkdirAll(filepath.Join(tree, "workers", "appa"), 0o755)
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
workers:
  - name: appa
    dir: workers/appa
`)
	app, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if app.Name != "demo" || app.Backend != "orbstack" {
		t.Fatalf("app=%+v", app)
	}
	if len(app.Workers) != 1 || app.Workers[0].Name != "appa" {
		t.Fatalf("workers=%+v", app.Workers)
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
	mustWriteFile(t, cfg, "name: a\nbackend: orbstack\ntree: ws\nbroker:\n  llm_auth: subscription\nmanager:\n  credential_file: secrets/tok\n")
	app, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "secrets/tok"); app.Manager.CredentialFile != want {
		t.Fatalf("CredentialFile = %q, want %q", app.Manager.CredentialFile, want)
	}

	home, _ := os.UserHomeDir()
	cfg2 := filepath.Join(dir, "l2.yaml")
	mustWriteFile(t, cfg2, "name: a\nbackend: orbstack\ntree: ws\nbroker:\n  llm_auth: subscription\nmanager:\n  credential_file: ~/.scion/oauth-token\n")
	app2, err := Load(cfg2)
	if err != nil {
		t.Fatal(err)
	}
	if app2.Manager.CredentialFile != filepath.Join(home, ".scion/oauth-token") {
		t.Fatalf("CredentialFile ~ expand = %q", app2.Manager.CredentialFile)
	}

	cfg3 := filepath.Join(dir, "l3.yaml")
	mustWriteFile(t, cfg3, "name: a\nbackend: orbstack\ntree: ws\nbroker:\n  llm_auth: subscription\n")
	app3, _ := Load(cfg3)
	if app3.Manager.CredentialFile != "" {
		t.Fatalf("empty CredentialFile = %q", app3.Manager.CredentialFile)
	}
}

func TestValidateRejectsUnknownBackend(t *testing.T) {
	p := writeTmp(t, "name: x\nbackend: vmware\ntree: ./tree\nmanager: {}\n")
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for unknown backend")
	}
	if !strings.Contains(err.Error(), "unknown backend") {
		t.Errorf("error %q should say 'unknown backend'", err)
	}
}

// A backend lever cannot run — including ex-roadmap names like linux-docker —
// must be rejected at config load, naming the valid set.
func TestConfigRejectsUnknownBackend(t *testing.T) {
	p := writeTmp(t, "name: x\nbackend: linux-docker\ntree: ./tree\nmanager: {}\n")
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for an unknown backend")
	}
	if !strings.Contains(err.Error(), "unknown backend") || !strings.Contains(err.Error(), "orbstack") {
		t.Errorf("error %q should say 'unknown backend' and name the valid set", err)
	}
}

func TestValidateRequiresBackend(t *testing.T) {
	p := writeTmp(t, "name: x\ntree: ./tree\nmanager: {}\n")
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error when backend is omitted")
	}
	if !strings.Contains(err.Error(), "backend is required") {
		t.Errorf("error %q should say 'backend is required'", err)
	}
}

func TestValidateRejectsWorkerOutsideTree(t *testing.T) {
	p := writeTmp(t, "name: x\nbackend: orbstack\ntree: ./tree\nmanager: {}\nworkers:\n  - name: bad\n    dir: ../escape\n")
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for worker dir outside tree")
	}
}

// A worker with dir "." makes WorkerDir(g) == a.Tree, so its jail path collides
// with the manager's mount root (/lever) — then the worker's register-step
// removal (scion project-configs + .scion marker) would target the MANAGER's
// state. Reject it at config time, mirroring confinedRel's "." rejection for
// `tree`.
func TestValidateRejectsWorkerDirDot(t *testing.T) {
	for _, dir := range []string{".", "./"} {
		p := writeTmp(t, "name: x\nbackend: orbstack\ntree: ./tree\nbroker:\n  llm_auth: subscription\nmanager: {}\nworkers:\n  - name: bad\n    dir: "+dir+"\n")
		_, err := Load(p)
		if err == nil {
			t.Fatalf("dir %q: expected error for worker dir collapsing to the tree root", dir)
		}
		if !strings.Contains(err.Error(), "collides with the manager's mount root") {
			t.Errorf("dir %q: error %q should explain the mount-root collision", dir, err)
		}
	}
	// A normal subdir must still pass.
	ok := writeTmp(t, "name: x\nbackend: orbstack\ntree: ./tree\nbroker:\n  llm_auth: subscription\nmanager: {}\nworkers:\n  - name: good\n    dir: workers/good\n")
	if _, err := Load(ok); err != nil {
		t.Fatalf("a normal worker dir (workers/good) must pass, got %v", err)
	}
}

func TestValidateRejectsGitTree(t *testing.T) {
	tree := t.TempDir()
	if err := os.Mkdir(filepath.Join(tree, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := &App{Name: "demo", Backend: "orbstack", Tree: tree}
	a.Broker.LLMAuth = "subscription"
	if err := a.validateNonGitTree(); err == nil {
		t.Fatalf("expected rejection: tree is itself a git repo")
	}
}

func TestValidateRejectsScionSourceAndVersionTogether(t *testing.T) {
	p := writeTmp(t, "name: x\nbackend: orbstack\ntree: ./tree\nmanager: {}\nscion:\n  source: ./scion-src\n  version: abc123\n")
	if _, err := Load(p); err == nil {
		t.Fatal("expected error: scion.source and scion.version are mutually exclusive")
	}
}

func TestDefaultLLMAuthIsAPIKey(t *testing.T) {
	a := &App{}
	if got := a.EffectiveManagerLLMAuth(); got != LLMAuthAPIKey {
		t.Fatalf("default llm_auth = %q, want api-key (secure default)", got)
	}
}

// Broker ports default to fixed constants when unset (0), so a config need not
// declare them; an explicit value still wins. Both `apply` and the spawned
// `broker serve` read the same config, so the default is computed identically
// in both processes.
func TestBrokerPortsDefaultWhenUnset(t *testing.T) {
	a := &App{}
	if got := a.EffectiveJailPort(); got != DefaultBrokerJailPort {
		t.Fatalf("EffectiveJailPort default = %d, want %d", got, DefaultBrokerJailPort)
	}
	if got := a.EffectiveAdminPort(); got != DefaultBrokerAdminPort {
		t.Fatalf("EffectiveAdminPort default = %d, want %d", got, DefaultBrokerAdminPort)
	}
	a2 := &App{Broker: Broker{JailPort: 9001, AdminPort: 9002}}
	if a2.EffectiveJailPort() != 9001 || a2.EffectiveAdminPort() != 9002 {
		t.Fatalf("explicit ports not honoured: jail=%d admin=%d", a2.EffectiveJailPort(), a2.EffectiveAdminPort())
	}
	if DefaultBrokerJailPort == DefaultBrokerAdminPort {
		t.Fatal("jail and admin default ports must differ")
	}
}

// The api-key default must demand a broker.api_key_file — a minimal config that
// opts into neither subscription nor a key must fail closed.
func TestAPIKeyDefaultRequiresAPIKeyFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, CanonicalName)
	mustWriteFile(t, p, "name: demo\nbackend: orbstack\ntree: ws\nmanager: {}\n")
	if _, err := Load(p); err == nil {
		t.Fatal("api-key default must require broker.api_key_file")
	}
}

// egress: closed requires a uniformly api-key instance.
func TestEgressClosedRejectsSubscription(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, CanonicalName)
	mustWriteFile(t, p, "name: demo\nbackend: orbstack\negress: closed\ntree: ws\nbroker:\n  llm_auth: subscription\nmanager: {}\n")
	if _, err := Load(p); err == nil {
		t.Fatal("egress: closed with a subscription agent must be rejected")
	}
}

func TestRejectsInvalidEgress(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, CanonicalName)
	mustWriteFile(t, p, "name: demo\nbackend: orbstack\negress: maybe\ntree: ws\nbroker:\n  llm_auth: subscription\nmanager: {}\n")
	if _, err := Load(p); err == nil {
		t.Fatal("invalid egress value must be rejected")
	}
}

// api-key + egress: closed + a 0600 api_key_file is the fully-locked-down posture.
func TestAPIKeyEgressClosedLoads(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "console.key")
	mustWriteFile(t, key, "sk-ant-fake")
	if err := os.Chmod(key, 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, CanonicalName)
	mustWriteFile(t, p, "name: demo\nbackend: orbstack\negress: closed\ntree: ws\nbroker:\n  llm_auth: api-key\n  api_key_file: "+key+"\nmanager: {}\n")
	if _, err := Load(p); err != nil {
		t.Fatalf("api-key + egress: closed + api_key_file must load: %v", err)
	}
}

func TestWorkerImageFallsBackToManagerImage(t *testing.T) {
	app := &App{
		Manager: Manager{Image: "scionlocal/lever-claude:latest"},
		Workers: []Worker{
			{Name: "plain", Dir: "workers/plain"},
			{Name: "custom", Dir: "workers/custom", Image: "scionlocal/lever-rust:latest"},
		},
	}
	g0, _ := app.WorkerByName("plain")
	if got := app.WorkerImage(g0); got != "scionlocal/lever-claude:latest" {
		t.Fatalf("plain worker image = %q, want manager image", got)
	}
	g1, _ := app.WorkerByName("custom")
	if got := app.WorkerImage(g1); got != "scionlocal/lever-rust:latest" {
		t.Fatalf("custom worker image = %q, want override", got)
	}
	if _, ok := app.WorkerByName("missing"); ok {
		t.Fatal("WorkerByName(missing) should be false")
	}
}

func TestLoadParsesWorkerImage(t *testing.T) {
	p := writeTmp(t, `name: demo
backend: orbstack
tree: ./tree
manager:
  image: scionlocal/lever-claude:latest
workers:
  - name: appa
    dir: workers/appa
    image: scionlocal/lever-rust:latest
`)
	app, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if app.Workers[0].Image != "scionlocal/lever-rust:latest" {
		t.Fatalf("worker image = %q", app.Workers[0].Image)
	}
}

func TestLoadConfinesTree(t *testing.T) {
	// tree must be a confined relative subdir: reject "." (root==mount),
	// absolute, "..", and empty. A normal subdir is accepted and joined.
	base := "name: demo\nbackend: orbstack\nbroker:\n  llm_auth: subscription\nmanager: {}\n"
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
		"bad worker name":  "name: demo\nbackend: orbstack\ntree: ws\nworkers:\n  - name: Bad\n    dir: workers/x\n",
	}
	for label, body := range cases {
		p := writeTmp(t, body)
		if _, err := Load(p); err == nil {
			t.Fatalf("%s should be rejected", label)
		}
	}
}

// A worker named the same as the manager identity would enrol with the manager's
// CN and gain full manager messaging authority — config validation must reject
// this as a CN-collision escalation.
func TestValidateRejectsWorkerNameCollidingWithManagerIdentity(t *testing.T) {
	cases := []struct {
		label  string
		config string
	}{
		{
			label:  "worker named manager (default manager identity)",
			config: "name: demo\nbackend: orbstack\ntree: ws\nbroker:\n  llm_auth: subscription\nmanager: {}\nworkers:\n  - name: manager\n    dir: workers/manager\n",
		},
		{
			label:  "worker named with custom manager identity",
			config: "name: demo\nbackend: orbstack\ntree: ws\nbroker:\n  llm_auth: subscription\n  manager_identity: custom-mgr\nmanager: {}\nworkers:\n  - name: custom-mgr\n    dir: workers/worker1\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			p := writeTmp(t, tc.config)
			_, err := Load(p)
			if err == nil {
				t.Fatal("worker name colliding with manager identity must be rejected")
			}
			if !strings.Contains(err.Error(), "collides with the manager identity") {
				t.Errorf("error %q should mention 'collides with the manager identity'", err)
			}
		})
	}
}

// A worker named like the APP would collide with the manager's scion agent slug
// (the manager is dispatched as Worker: app.Name): with slug matching in the
// broker's resolveMsgTarget, messages addressed to that worker would silently
// route to the manager instead. Config validation must reject the collision.
func TestValidateRejectsWorkerNameCollidingWithAppName(t *testing.T) {
	p := writeTmp(t, "name: demo\nbackend: orbstack\ntree: ws\nbroker:\n  llm_auth: subscription\nmanager: {}\nworkers:\n  - name: demo\n    dir: workers/demo\n")
	_, err := Load(p)
	if err == nil {
		t.Fatal("worker name colliding with the app name must be rejected")
	}
	if !strings.Contains(err.Error(), "collides with the manager agent") {
		t.Errorf("error %q should mention 'collides with the manager agent'", err)
	}
}

// manager.allow_ports opens host-loopback ports to the jailed agent — the
// egress allowlist is the ONLY thing isolating the host-loopback admin API
// (bootstrap/revoke/bump-epoch) from the guest, so listing the admin port
// there must be rejected at config load, not left as an operator footgun.
func TestManagerAllowPortsRejectsExplicitAdminPort(t *testing.T) {
	p := writeTmp(t, "name: demo\nbackend: orbstack\ntree: ws\nbroker:\n  llm_auth: subscription\n  admin_port: 9444\nmanager:\n  allow_ports: [9444]\n")
	_, err := Load(p)
	if err == nil {
		t.Fatal("manager.allow_ports containing the (explicit) broker admin port must be rejected")
	}
	if !strings.Contains(err.Error(), "allow_ports") || !strings.Contains(err.Error(), "admin") {
		t.Errorf("error %q should mention allow_ports and the admin port", err)
	}
}

func TestManagerAllowPortsRejectsDefaultAdminPortWhenUnset(t *testing.T) {
	// broker.admin_port is left unset, so the effective admin port is the
	// package default (DefaultBrokerAdminPort) — the rejection must use the
	// EFFECTIVE port, not just a configured one.
	p := writeTmp(t, fmt.Sprintf("name: demo\nbackend: orbstack\ntree: ws\nmanager:\n  allow_ports: [%d]\n", DefaultBrokerAdminPort))
	if _, err := Load(p); err == nil {
		t.Fatal("manager.allow_ports containing the DEFAULT broker admin port must be rejected")
	}
}

func TestManagerAllowPortsAcceptsOrdinaryPorts(t *testing.T) {
	p := writeTmp(t, "name: demo\nbackend: orbstack\ntree: ws\nmanager:\n  allow_ports: [3305]\n")
	if _, err := Load(p); err != nil {
		t.Fatalf("an ordinary allow_ports entry should load fine: %v", err)
	}
}

func TestManagerPromptPathIsRootRelative(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, CanonicalName)
	body := "name: demo\nbackend: orbstack\ntree: workspace\nbroker:\n  llm_auth: subscription\nmanager:\n  prompt_file: boot.md\n"
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

func TestSecurityImagePolicyAppliesToWorkers(t *testing.T) {
	body := "name: demo\nbackend: orbstack\ntree: ws\n" +
		"security:\n  allowed_image_registries: [scionlocal]\n" +
		"manager:\n  image: scionlocal/mgr:latest\n" +
		"workers:\n  - name: g\n    dir: workers/g\n    image: ghcr.io/who/x:latest\n"
	if _, err := Load(writeTmp(t, body)); err == nil {
		t.Fatal("worker image outside the allowlist should be rejected")
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
workers:
  - {name: worker, dir: work, obtain: []}
broker:
  llm_auth: subscription
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

func TestLoadRejectsGrantWithUnknownOp(t *testing.T) {
	// db exists but has only op "read"; granting "write" must be rejected.
	bad := replaceFirst(baseCfg, "op: read, to:", "op: write, to:")
	if _, err := Load(writeConfig(t, bad)); err == nil {
		t.Fatal("a delegate grant referencing an undeclared op must be rejected")
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

// TestRejectsMixedLLMAuthInstance: an instance that mixes api-key and
// subscription agents is UNSUPPORTED and must fail validation — the OAuth
// credential is a single jail-wide Hub secret and egress is jail-wide, so a
// subscription agent forces the real token into (and open egress for) the
// api-key agents' containers, defeating their key isolation. See
// security-model.md §6.1.
func TestRejectsMixedLLMAuthInstance(t *testing.T) {
	// Broker default api-key ⇒ manager is api-key; worker overrides to subscription.
	a := &App{
		Name: "demo", Backend: "orbstack", Tree: "/x",
		Broker:  Broker{LLMAuth: LLMAuthAPIKey},
		Workers: []Worker{{Name: "worker", Dir: "w", LLMAuth: LLMAuthSubscription}},
	}
	err := a.Validate()
	if err == nil || !strings.Contains(err.Error(), "mixed") {
		t.Fatalf("mixed instance must be rejected with a 'mixed' error, got: %v", err)
	}
}

// TestUniformInstancesValidate: the two pure cases are accepted (uniform
// subscription needs no key; uniform api-key needs a 0600 api_key_file).
func TestUniformInstancesValidate(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "api-key")
	if err := os.WriteFile(keyPath, []byte("sk-test"), 0o600); err != nil {
		t.Fatal(err)
	}
	subscription := &App{
		Name: "demo", Backend: "orbstack", Tree: "/x",
		Broker:  Broker{LLMAuth: LLMAuthSubscription},
		Workers: []Worker{{Name: "worker", Dir: "w"}}, // both subscription (explicit opt-in)
	}
	if err := subscription.Validate(); err != nil {
		t.Fatalf("uniform subscription should validate: %v", err)
	}
	apikey := &App{
		Name: "demo", Backend: "orbstack", Tree: "/x",
		Broker:  Broker{LLMAuth: LLMAuthAPIKey, APIKeyFile: keyPath},
		Workers: []Worker{{Name: "worker", Dir: "w"}}, // both inherit api-key
	}
	if err := apikey.Validate(); err != nil {
		t.Fatalf("uniform api-key should validate: %v", err)
	}
}

// TestInjectsLLMGrantPerAgentMode unit-tests the grant-injection discrimination
// directly (a valid instance is uniform, so this is exercised on an in-memory
// App rather than through Load): an api-key agent gets the implicit llm grant, a
// subscription agent does not.
func TestInjectsLLMGrantPerAgentMode(t *testing.T) {
	a := &App{
		Manager: Manager{LLMAuth: LLMAuthAPIKey},
		Workers: []Worker{{Name: "worker", LLMAuth: LLMAuthSubscription}},
	}
	a.injectLLMGrants()
	if !hasGrant(a.Manager.Obtain, "llm", "generate") {
		t.Errorf("manager (api-key) missing injected llm grant: %+v", a.Manager.Obtain)
	}
	if hasGrant(a.Workers[0].Obtain, "llm", "generate") {
		t.Errorf("subscription agent must NOT get an llm grant: %+v", a.Workers[0].Obtain)
	}
}

func hasGrant(gs []Grant, tool, op string) bool {
	for _, g := range gs {
		if g.Tool == tool && g.Op == op {
			return true
		}
	}
	return false
}

func TestEffectiveLLMAuthWorkerOverride(t *testing.T) {
	a := &App{Broker: Broker{LLMAuth: LLMAuthAPIKey}, Workers: []Worker{{Name: "w"}}}
	if got := a.EffectiveManagerLLMAuth(); got != LLMAuthAPIKey {
		t.Fatalf("manager: got %q want api-key", got)
	}
	// worker inherits broker default when unset
	if got := a.EffectiveWorkerLLMAuth(a.Workers[0]); got != LLMAuthAPIKey {
		t.Fatalf("worker inherit: got %q want api-key", got)
	}
	// worker override wins
	a.Workers[0].LLMAuth = LLMAuthSubscription
	if got := a.EffectiveWorkerLLMAuth(a.Workers[0]); got != LLMAuthSubscription {
		t.Fatalf("worker override: got %q want subscription", got)
	}
}

func TestValidateBrokerLLMAuth(t *testing.T) {
	t.Run("invalid llm_auth rejects", func(t *testing.T) {
		body := "name: demo\nbackend: orbstack\ntree: work\nmanager: {}\nbroker:\n  llm_auth: bogus\n"
		if _, err := Load(writeConfig(t, body)); err == nil {
			t.Fatal("expected error for invalid llm_auth value, got nil")
		}
	})

	t.Run("api-key without api_key_file rejects", func(t *testing.T) {
		body := "name: demo\nbackend: orbstack\ntree: work\nmanager: {}\nbroker:\n  llm_auth: api-key\n"
		if _, err := Load(writeConfig(t, body)); err == nil {
			t.Fatal("expected error: api-key mode requires api_key_file, got nil")
		}
	})

	t.Run("api-key with 0644 api_key_file rejects mentioning 0600", func(t *testing.T) {
		dir := t.TempDir()
		keyPath := filepath.Join(dir, "api.key")
		if err := os.WriteFile(keyPath, []byte("sk-ant-test"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(keyPath, 0o644); err != nil {
			t.Fatal(err)
		}
		body := "name: demo\nbackend: orbstack\ntree: work\nmanager: {}\nbroker:\n  llm_auth: api-key\n  api_key_file: " + keyPath + "\n"
		_, err := Load(writeConfig(t, body))
		if err == nil {
			t.Fatal("expected error for 0644 api_key_file, got nil")
		}
		if !strings.Contains(err.Error(), "0600") {
			t.Errorf("error must mention 0600, got: %v", err)
		}
	})
}

func TestClosedInternetEgress(t *testing.T) {
	// Explicit knob, decoupled from llm_auth: egress: closed ⇒ closed; unset ⇒ open.
	closedApp := &App{Egress: EgressClosed, Broker: Broker{LLMAuth: LLMAuthAPIKey}, Workers: []Worker{{Name: "w"}}}
	if closed, warn := closedApp.ClosedInternetEgress(); !closed || warn != "" {
		t.Fatalf("egress: closed ⇒ closed=%v warn=%q want true/empty", closed, warn)
	}
	openAPIKey := &App{Broker: Broker{LLMAuth: LLMAuthAPIKey}, Workers: []Worker{{Name: "w"}}}
	if closed, _ := openAPIKey.ClosedInternetEgress(); closed {
		t.Fatal("api-key without egress: closed must leave egress open (decoupled)")
	}
	openSub := &App{Broker: Broker{LLMAuth: LLMAuthSubscription}, Workers: []Worker{{Name: "w"}}}
	if closed, warn := openSub.ClosedInternetEgress(); closed || warn != "" {
		t.Fatalf("default (open): closed=%v warn=%q want false/empty", closed, warn)
	}
}

// extCfg is baseCfg plus a fine and a coarse external tool appended to its
// broker.tools list (baseCfg ends inside that list, 4-space item indent).
var extCfg = baseCfg + `    - name: devonthink
      external: true
      backend: 127.0.0.1:3302
      operations:
        - {name: search}
      allowed_values: {database: [work, personal]}
    - name: things3
      external: true
      backend: 127.0.0.1:3300
      gate: coarse
`

func TestLoadAcceptsExternalTools(t *testing.T) {
	app, err := Load(writeConfig(t, extCfg))
	if err != nil {
		t.Fatal(err)
	}
	var dt, th Tool
	for _, tl := range app.Broker.Tools {
		switch tl.Name {
		case "devonthink":
			dt = tl
		case "things3":
			th = tl
		}
	}
	if !dt.External || dt.EffectiveGate() != GateFine {
		t.Fatalf("devonthink = %+v; want external fine", dt)
	}
	if !th.External || th.EffectiveGate() != GateCoarse {
		t.Fatalf("things3 = %+v; want external coarse", th)
	}
}

func TestLoadAcceptsWildcardGrantOnCoarseTool(t *testing.T) {
	cfg := replaceFirst(extCfg, "workers:\n  - {name: worker, dir: work, obtain: []}",
		"workers:\n  - {name: worker, dir: work, obtain: [{tool: things3, op: \"*\"}]}")
	if _, err := Load(writeConfig(t, cfg)); err != nil {
		t.Fatalf("a wildcard grant on a coarse tool must load: %v", err)
	}
}

func TestLoadRejectsWildcardGrantOnFineTool(t *testing.T) {
	cfg := replaceFirst(extCfg, "workers:\n  - {name: worker, dir: work, obtain: []}",
		"workers:\n  - {name: worker, dir: work, obtain: [{tool: devonthink, op: \"*\"}]}")
	if _, err := Load(writeConfig(t, cfg)); err == nil {
		t.Fatal("a wildcard grant on a fine tool must be rejected at load")
	}
}

func TestLoadRejectsExternalToolShapeErrors(t *testing.T) {
	cases := []struct{ name, find, repl string }{
		{"command on external", "external: true\n      backend: 127.0.0.1:3302",
			"external: true\n      command: [oops]\n      backend: 127.0.0.1:3302"},
		{"missing backend", "backend: 127.0.0.1:3302\n", ""},
		{"coarse with operations", "gate: coarse", "gate: coarse\n      operations:\n        - {name: x}"},
		{"coarse with allowed_values", "gate: coarse", "gate: coarse\n      allowed_values: {a: [b]}"},
		{"bad gate value", "gate: coarse", "gate: medium"},
		{"non-loopback backend", "backend: 127.0.0.1:3300", "backend: 192.168.1.9:3300"},
		{"hostname backend", "backend: 127.0.0.1:3300", "backend: localhost:3300"},
		{"scheme in backend", "backend: 127.0.0.1:3300", "backend: http://127.0.0.1:3300"},
		{"gate on supervised tool", "command: [lever-tool-db, -dsn, \"file:ref.db\"]",
			"command: [lever-tool-db, -dsn, \"file:ref.db\"]\n      gate: coarse"},
		{"reserved op name", "- {name: search}", "- {name: \"*\"}"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, replaceFirst(extCfg, c.find, c.repl))); err == nil {
				t.Fatalf("%s: must be rejected", c.name)
			}
		})
	}
}

func TestLoadRejectsFineExternalWithoutOperations(t *testing.T) {
	cfg := replaceFirst(extCfg, "      operations:\n        - {name: search}\n      allowed_values: {database: [work, personal]}\n", "")
	if _, err := Load(writeConfig(t, cfg)); err == nil {
		t.Fatal("a fine external tool with no operations must be rejected")
	}
}

func TestLoadRejectsSupervisedToolWithoutCommand(t *testing.T) {
	cfg := replaceFirst(extCfg, "command: [lever-tool-db, -dsn, \"file:ref.db\"]\n      ", "")
	if _, err := Load(writeConfig(t, cfg)); err == nil {
		t.Fatal("a non-external tool with no command must be rejected")
	}
}

func TestLoadAllowsNonLoopbackBackendWithOptIn(t *testing.T) {
	cfg := replaceFirst(extCfg, "external: true\n      backend: 127.0.0.1:3300",
		"external: true\n      allow_non_loopback: true\n      backend: 192.168.1.9:3300")
	if _, err := Load(writeConfig(t, cfg)); err != nil {
		t.Fatalf("allow_non_loopback must permit a LAN backend: %v", err)
	}
}

func TestLoadAcceptsIPv6LoopbackBackend(t *testing.T) {
	cfg := replaceFirst(extCfg, "backend: 127.0.0.1:3300", "backend: \"[::1]:3300\"")
	if _, err := Load(writeConfig(t, cfg)); err != nil {
		t.Fatalf("[::1] is loopback and must be accepted: %v", err)
	}
}

func TestLoadAcceptsBackendWithPath(t *testing.T) {
	cfg := replaceFirst(extCfg, "backend: 127.0.0.1:3300", "backend: 127.0.0.1:3101/mcp")
	if _, err := Load(writeConfig(t, cfg)); err != nil {
		t.Fatalf("a loopback backend with a path (qmd-style) must be accepted: %v", err)
	}
}

// TestLoadRejectsNonExternalAllowNonLoopback: allow_non_loopback only makes
// sense on an external (fronted) tool, mirroring the existing gate check — a
// supervised tool setting it is a config mistake that should fail closed, not
// be silently ignored.
func TestLoadRejectsNonExternalAllowNonLoopback(t *testing.T) {
	cfg := replaceFirst(baseCfg, "command: [lever-tool-db, -dsn, \"file:ref.db\"]",
		"command: [lever-tool-db, -dsn, \"file:ref.db\"]\n      allow_non_loopback: true")
	_, err := Load(writeConfig(t, cfg))
	if err == nil {
		t.Fatal("a non-external tool setting allow_non_loopback must be rejected")
	}
	if !strings.Contains(err.Error(), "allow_non_loopback") || !strings.Contains(err.Error(), "external") {
		t.Errorf("error %q should mention allow_non_loopback and external", err)
	}
}

// TestLoadRejectsIllegalToolNames: tool names flow into the broker's
// /mcp/<name>/ gateway route and into `claude mcp add`, so they must be
// constrained to a safe charset like instance/worker names. Both the tool
// declaration AND the grant referencing it are renamed together, so the
// failure is the charset check itself, not an unrelated "undeclared tool"
// error from a stale grant reference.
func TestLoadRejectsIllegalToolNames(t *testing.T) {
	bad := []string{"bad/name", "Bad", "has space", "star*"}
	for _, name := range bad {
		t.Run(name, func(t *testing.T) {
			cfg := replaceFirst(baseCfg, "name: db", "name: "+name)
			cfg = replaceFirst(cfg, "tool: db, op: read", "tool: "+name+", op: read")
			if _, err := Load(writeConfig(t, cfg)); err == nil {
				t.Fatalf("tool name %q should be rejected", name)
			}
		})
	}
}

// TestLoadAcceptsExistingValidToolNames pins the existing legitimate tool
// names (which contain a digit, but no uppercase/space/slash/star) as staying
// valid once the charset check lands.
func TestLoadAcceptsExistingValidToolNames(t *testing.T) {
	if _, err := Load(writeConfig(t, baseCfg)); err != nil {
		t.Fatalf("tool name %q (db) should remain valid: %v", "db", err)
	}
	if _, err := Load(writeConfig(t, extCfg)); err != nil {
		t.Fatalf("tool names devonthink/things3 should remain valid: %v", err)
	}
}

// TestLoadAcceptsUnderscoreToolName pins that a real MCP server name with an
// underscore (e.g. apple_notes) is a valid broker tool name: MCP server names
// legitimately use underscores, and they are safe in the /mcp/<name>/ URL path.
// This is the case the initial nameRE reuse wrongly rejected.
func TestLoadAcceptsUnderscoreToolName(t *testing.T) {
	cfg := replaceFirst(baseCfg, "name: db", "name: apple_notes")
	cfg = replaceFirst(cfg, "tool: db, op: read", "tool: apple_notes, op: read")
	if _, err := Load(writeConfig(t, cfg)); err != nil {
		t.Fatalf("underscore tool name apple_notes must be accepted: %v", err)
	}
}

// TestLoadRejectsEmptyOperationName: an operation with an empty name is a
// config mistake — it can never be granted (checkCap compares op strings) and
// would silently sink into the tool's op set undetected.
func TestLoadRejectsEmptyOperationName(t *testing.T) {
	cfg := replaceFirst(baseCfg, "{name: read, caveat_param: {table: table, filter: filter}}",
		"{name: read, caveat_param: {table: table, filter: filter}}\n        - {name: \"\"}")
	if _, err := Load(writeConfig(t, cfg)); err == nil {
		t.Fatal("an operation with an empty name must be rejected")
	}
}

func TestWorkerToWorkerMessagingDefaultsTrue(t *testing.T) {
	tr := true
	fa := false
	cases := []struct {
		name string
		val  *bool
		want bool
	}{
		{"unset defaults true", nil, true},
		{"explicit true", &tr, true},
		{"explicit false", &fa, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &App{}
			a.Broker.Messaging.WorkerToWorker = c.val
			if got := a.WorkerToWorkerMessaging(); got != c.want {
				t.Fatalf("WorkerToWorkerMessaging() = %v, want %v", got, c.want)
			}
		})
	}
}

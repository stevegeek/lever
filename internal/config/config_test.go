package config

import (
	"os"
	"path/filepath"
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

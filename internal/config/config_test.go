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

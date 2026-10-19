package host

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/config"
)

func TestServiceHostPair(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "work")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, config.CanonicalName)
	app := &config.App{Name: "demo", Tree: tree, Broker: config.Broker{ServiceHosts: []config.ServiceHost{{Name: "laptop"}}}}
	run := func(a *config.App, name string, rotate bool) (string, error) {
		var out bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&out)
		err := runServiceHostPair(cmd, a, path, name, rotate)
		return out.String(), err
	}
	if _, err := run(app, "desktop", false); err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("undeclared host: %v", err)
	}
	first, err := run(app, "laptop", false)
	if err != nil || !strings.Contains(first, "client fingerprint: sha256:") {
		t.Fatalf("pair: %q %v", first, err)
	}
	key := filepath.Join(root, ".lever-state", "service-hosts", "laptop", "client.key")
	if fi, err := os.Stat(key); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("client key: %v %v", fi, err)
	}
	fp := first[strings.Index(first, "sha256:"):][:71]
	again, err := run(app, "laptop", false)
	if err != nil || !strings.Contains(again, "already paired") || !strings.Contains(again, fp) {
		t.Fatalf("second pair: %q %v", again, err)
	}
	rotated, err := run(app, "laptop", true)
	if err != nil || strings.Contains(rotated, fp) {
		t.Fatalf("rotate kept the key: %q %v", rotated, err)
	}
	inside := *app
	inside.Tree = root
	if _, err := run(&inside, "laptop", false); err == nil || !strings.Contains(err.Error(), "inside the tree") {
		t.Fatalf("state inside the tree: %v", err)
	}
}

func TestServiceHostKeyPaths(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "sh.yaml")
	c, k, err := serviceHostKeyPaths(cfg)
	if err != nil || c != filepath.Join(dir, "server.crt") || k != filepath.Join(dir, "server.key") {
		t.Fatalf("defaults: %s %s %v", c, k, err)
	}
	if err := os.WriteFile(cfg, []byte("listen: 1.2.3.4:1\ncert: tls/s.crt\nkey: /abs/s.key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, k, err = serviceHostKeyPaths(cfg)
	if err != nil || c != filepath.Join(dir, "tls", "s.crt") || k != "/abs/s.key" {
		t.Fatalf("from config: %s %s %v", c, k, err)
	}
}

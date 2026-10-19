package servicehost

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testFP = "sha256:" + "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"

func writeConfig(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "service-host.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfig(t *testing.T) {
	good := "listen: 100.64.1.2:7443\nclient_fingerprint: " + testFP + "\ntools:\n  - name: mail\n    command: [mail-mcp]\n    allow: [search]\n"
	cases := []struct {
		name, body string
		mode       os.FileMode
		wantErr    string
	}{
		{"good", good, 0o600, ""},
		{"upper-case fingerprint", strings.Replace(good, "ab12", "AB12", 1), 0o644, ""},
		{"group-writable config", good, 0o620, "writable by group or others"},
		{"wildcard v4", strings.Replace(good, "100.64.1.2", "0.0.0.0", 1), 0o600, "wildcard"},
		{"wildcard v6", strings.Replace(good, "100.64.1.2", "[::]", 1), 0o600, "wildcard"},
		{"no host", strings.Replace(good, "100.64.1.2", "", 1), 0o600, "every network"},
		{"wildcard forced", "allow_wildcard_listen: true\n" + strings.Replace(good, "100.64.1.2", "0.0.0.0", 1), 0o600, ""},
		{"hostname listen", strings.Replace(good, "100.64.1.2", "box.local", 1), 0o600, "literal IP"},
		{"no port", strings.Replace(good, ":7443", ":0", 1), 0o600, "fixed port"},
		{"no fingerprint", strings.Replace(good, "client_fingerprint: "+testFP+"\n", "", 1), 0o600, "client_fingerprint is required"},
		{"bad fingerprint", strings.Replace(good, testFP, "sha256:zz", 1), 0o600, "64 hex"},
		{"unknown key", good + "allows: [x]\n", 0o600, "not found"},
		{"no allow", strings.Replace(good, "    allow: [search]\n", "", 1), 0o600, "needs allow"},
		{"star mixed", strings.Replace(good, "[search]", "[\"*\", search]", 1), 0o600, "mixes"},
		{"both kinds", strings.Replace(good, "command: [mail-mcp]", "command: [mail-mcp]\n    upstream: http://127.0.0.1:1/", 1), 0o600, "exactly one"},
		{"neither kind", strings.Replace(good, "    command: [mail-mcp]\n", "", 1), 0o600, "exactly one"},
		{"bad tool name", strings.Replace(good, "name: mail", "name: ../x", 1), 0o600, "must match"},
		{"bad env name", strings.Replace(good, "command: [mail-mcp]", "command: [mail-mcp]\n    env: {\"A=B\": x}", 1), 0o600, "environment variable"},
		{"headers on command", strings.Replace(good, "command: [mail-mcp]", "command: [mail-mcp]\n    headers: {X-A: b}", 1), 0o600, "upstream tool only"},
		{"upstream scheme", strings.Replace(good, "command: [mail-mcp]", "upstream: file:///etc/passwd", 1), 0o600, "http://"},
		{"upstream userinfo", strings.Replace(good, "command: [mail-mcp]", "upstream: http://u:p@127.0.0.1/", 1), 0o600, "without credentials"},
		{"reserved header", strings.Replace(good, "command: [mail-mcp]", "upstream: http://127.0.0.1:1/\n    headers: {Host: evil}", 1), 0o600, "cannot be set"},
		{"lever header", strings.Replace(good, "command: [mail-mcp]", "upstream: http://127.0.0.1:1/\n    headers: {X-Lever-Caller: x}", 1), 0o600, "cannot be set"},
		{"env on upstream", strings.Replace(good, "command: [mail-mcp]", "upstream: http://127.0.0.1:1/\n    env: {A: b}", 1), 0o600, "command tool only"},
		{"duplicate tool", good + "  - name: mail\n    command: [x]\n    allow: [y]\n", 0o600, "twice"},
		{"no tools", "listen: 100.64.1.2:7443\nclient_fingerprint: " + testFP + "\n", 0o600, "at least one tool"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadConfig(writeConfig(t, c.body, c.mode))
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Fatalf("error = %v, want %q", err, c.wantErr)
			}
		})
	}
}

func TestSecretFilesMustBePrivate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "token")
	if err := os.WriteFile(p, []byte("x\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecretValue(p, true); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("group-readable secret: %v", err)
	}
	if err := os.WriteFile(p, []byte("a\nb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecretValue(p, false); err == nil {
		t.Fatal("a two-line header value was accepted")
	}
	if v, err := readSecretValue(p, true); err != nil || v != "a\nb" {
		t.Fatalf("multi-line env value = %q, %v", v, err)
	}
}

func TestEnsureKeyPair(t *testing.T) {
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "c.crt"), filepath.Join(dir, "k", "c.key")
	fp, created, err := EnsureKeyPair(cert, key, "x", x509.ExtKeyUsageClientAuth, false)
	if err != nil || !created {
		t.Fatal(err, created)
	}
	if fi, _ := os.Stat(key); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", fi.Mode().Perm())
	}
	again, created, err := EnsureKeyPair(cert, key, "x", x509.ExtKeyUsageClientAuth, false)
	if err != nil || created || again != fp {
		t.Fatalf("second run replaced the key: %v %v %v", again, created, err)
	}
	rotated, created, err := EnsureKeyPair(cert, key, "x", x509.ExtKeyUsageClientAuth, true)
	if err != nil || !created || rotated == fp {
		t.Fatalf("rotate: %v %v %v", rotated, created, err)
	}
	if err := os.Remove(cert); err != nil {
		t.Fatal(err)
	}
	if _, _, err := EnsureKeyPair(cert, key, "x", x509.ExtKeyUsageClientAuth, false); err == nil {
		t.Fatal("half a pair was silently replaced")
	}
}

func TestParseFingerprint(t *testing.T) {
	if f, err := ParseFingerprint(strings.ToUpper(testFP[7:])); err == nil {
		t.Fatalf("no prefix accepted: %s", f)
	}
	f, err := ParseFingerprint(" sha256:" + strings.ToUpper(testFP[7:]) + " ")
	if err != nil || string(f) != testFP {
		t.Fatalf("got %q %v", f, err)
	}
	if Fingerprint("").matches(nil) {
		t.Fatal("an empty pin matched")
	}
}

package ghpush

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeKey(t *testing.T, mode os.FileMode) (string, *rsa.PrivateKey) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "app.pem")
	b := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	if err := os.WriteFile(p, b, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p, k
}

func TestLoadAppKeyMode(t *testing.T) {
	p, _ := writeKey(t, 0o600)
	if _, err := LoadAppKey(p); err != nil {
		t.Fatalf("0600: %v", err)
	}
	p2, _ := writeKey(t, 0o644)
	if _, err := LoadAppKey(p2); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("0644 must be refused, got %v", err)
	}
	link := filepath.Join(t.TempDir(), "link.pem")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAppKey(link); err == nil {
		t.Fatal("a symlink must be refused")
	}
}

func TestMinterTokenRequestAndCache(t *testing.T) {
	_, key := writeKey(t, 0o600)
	calls := 0
	now := time.Unix(1_800_000_000, 0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/app/installations/42/access_tokens" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		jwt := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		parts := strings.Split(jwt, ".")
		if len(parts) != 3 {
			t.Fatalf("jwt parts %d", len(parts))
		}
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, h[:], sig); err != nil {
			t.Errorf("jwt signature: %v", err)
		}
		claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var c struct {
			Iss string `json:"iss"`
			Iat int64  `json:"iat"`
			Exp int64  `json:"exp"`
		}
		_ = json.Unmarshal(claims, &c)
		if c.Iss != "7" || c.Exp-c.Iat > 600 {
			t.Errorf("claims %+v", c)
		}
		var body struct {
			Repositories []string          `json:"repositories"`
			Permissions  map[string]string `json:"permissions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Repositories) != 1 || body.Repositories[0] != "lever" || body.Permissions["contents"] != "write" || len(body.Permissions) != 1 {
			t.Errorf("body %+v", body)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "ghs_test", "expires_at": now.Add(time.Hour).Format(time.RFC3339)})
	}))
	defer srv.Close()
	m := &Minter{AppID: "7", InstallationID: "42", Key: key, APIBase: srv.URL, HTTP: srv.Client(), Now: func() time.Time { return now }}
	for i := 0; i < 2; i++ {
		tok, err := m.Token(context.Background(), "stevegeek/lever")
		if err != nil || tok != "ghs_test" {
			t.Fatalf("token %q %v", tok, err)
		}
	}
	if calls != 1 {
		t.Fatalf("want 1 mint (cached), got %d", calls)
	}
	now = now.Add(51 * time.Minute) // < 10 min left → re-mint
	if _, err := m.Token(context.Background(), "stevegeek/lever"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("want a re-mint, got %d calls", calls)
	}
}

func TestMinterErrorHasNoSecrets(t *testing.T) {
	_, key := writeKey(t, 0o600)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	m := &Minter{AppID: "7", InstallationID: "42", Key: key, APIBase: srv.URL, HTTP: srv.Client(), Now: time.Now}
	_, err := m.Token(context.Background(), "stevegeek/lever")
	if err == nil || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "eyJ") {
		t.Fatalf("err = %v", err)
	}
}

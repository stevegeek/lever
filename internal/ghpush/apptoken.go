package ghpush

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

// TokenSource yields a push token for one repository.
type TokenSource interface {
	Token(ctx context.Context, repo string) (string, error)
}

// LoadAppKey reads the GitHub App private key: a regular file (not a
// symlink), mode exactly 0600, owned by this process's user.
func LoadAppKey(path string) (*rsa.PrivateKey, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("app key: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("app key %s: not a regular file", path)
	}
	if fi.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("app key %s: mode %o, want 0600", path, fi.Mode().Perm())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return nil, fmt.Errorf("app key %s: not owned by the running user", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("app key: %w", err)
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("app key %s: no PEM block", path)
	}
	if k, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("app key %s: %w", path, err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("app key %s: not an RSA key", path)
	}
	return rk, nil
}

// Minter mints GitHub App installation tokens narrowed to one repository and
// contents:write, and caches each until it has less than 10 minutes left.
type Minter struct {
	AppID, InstallationID string
	Key                   *rsa.PrivateKey
	APIBase               string // https://api.github.com
	HTTP                  *http.Client
	Now                   func() time.Time

	mu    sync.Mutex
	cache map[string]cachedToken
}

type cachedToken struct {
	tok string
	exp time.Time
}

func (m *Minter) jwt() (string, error) {
	now := m.Now()
	enc := base64.RawURLEncoding
	hdr := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	cl, _ := json.Marshal(map[string]any{"iat": now.Add(-60 * time.Second).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": m.AppID})
	signing := hdr + "." + enc.EncodeToString(cl)
	h := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, m.Key, crypto.SHA256, h[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// Token returns a token for repo ("owner/name").
func (m *Minter) Token(ctx context.Context, repo string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.cache[repo]; ok && c.exp.Sub(m.Now()) > 10*time.Minute {
		return c.tok, nil
	}
	jwt, err := m.jwt()
	if err != nil {
		return "", fmt.Errorf("github app jwt: %w", err)
	}
	name := repo[strings.IndexByte(repo, '/')+1:]
	body, _ := json.Marshal(map[string]any{"repositories": []string{name}, "permissions": map[string]string{"contents": "write"}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.APIBase+"/app/installations/"+m.InstallationID+"/access_tokens", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("github app token: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("github app token: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Token == "" {
		return "", fmt.Errorf("github app token: bad response")
	}
	if m.cache == nil {
		m.cache = map[string]cachedToken{}
	}
	m.cache[repo] = cachedToken{out.Token, out.ExpiresAt}
	return out.Token, nil
}

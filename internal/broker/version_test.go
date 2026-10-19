package broker

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/jailabi"
)

// versionBroker is a broker whose host-only values are sentinels a /version
// answer must never carry.
func versionBroker(t *testing.T) *Broker {
	t.Helper()
	b := New(testConfig(t, func(c *Config) {
		c.Version = "9.8.7 (0123456789ab)"
		c.ConfigHash = "SENTINEL-CONFIG-HASH"
		c.InstanceID = "SENTINEL-INSTANCE-ID"
	}))
	return b
}

func getVersion(t *testing.T, client *http.Client, url string) (int, []byte) {
	t.Helper()
	resp, err := client.Get(url + "/version")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}

// The manager and a worker both get the host's version and Jail ABI, and the
// answer has exactly those two fields.
func TestVersionAnswersManagerAndWorker(t *testing.T) {
	b := versionBroker(t)
	srv := jailServer(t, b)
	defer srv.Close()
	for _, cn := range []string{"manager", "worker"} {
		code, body := getVersion(t, agentClient(t, b, signedCert(t, b, cn)), srv.URL)
		if code != http.StatusOK {
			t.Fatalf("%s: status %d, body %s", cn, code, body)
		}
		var fields map[string]any
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Fatalf("%s: %v in %s", cn, err, body)
		}
		if len(fields) != 2 || fields["version"] != "9.8.7 (0123456789ab)" || fields["jail_abi"] != float64(jailabi.JailABI) {
			t.Fatalf("%s: /version answered %s, want only version and jail_abi", cn, body)
		}
		for _, leak := range []string{"SENTINEL", "instance", "config", cn} {
			if strings.Contains(string(body), leak) {
				t.Fatalf("%s: /version answer %s carries %q", cn, body, leak)
			}
		}
	}
}

func TestVersionRefusesWithoutALiveIdentity(t *testing.T) {
	b := versionBroker(t)
	b.Revoke("worker")
	srv := jailServer(t, b)
	defer srv.Close()

	if code, body := getVersion(t, agentClient(t, b, tls.Certificate{}), srv.URL); code != http.StatusForbidden {
		t.Fatalf("no cert: status %d, body %s, want 403", code, body)
	}
	if code, body := getVersion(t, agentClient(t, b, signedCert(t, b, "worker")), srv.URL); code != http.StatusForbidden {
		t.Fatalf("revoked worker: status %d, body %s, want 403", code, body)
	}

	// A cert from another CA fails the handshake.
	other, err := ca.Generate()
	if err != nil {
		t.Fatal(err)
	}
	csrPEM, keyPEM := csrWithKey(t, "manager")
	certPEM, err := other.SignCSR(csrPEM)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := agentClient(t, b, foreign).Get(srv.URL + "/version"); err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatal("a cert from another CA was answered")
		}
	}

	resp, err := agentClient(t, b, signedCert(t, b, "manager")).Post(srv.URL+"/version", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /version: status %d, want 405", resp.StatusCode)
	}
}

// /version is an agent-facing route only: the admin, operator and directive
// listeners do not serve it.
func TestVersionOnlyOnTheJailListener(t *testing.T) {
	b := versionBroker(t)
	for name, h := range map[string]http.Handler{
		"admin":     b.AdminHandler(),
		"operator":  b.OperatorHandler(),
		"directive": b.DirectiveAdminHandler(),
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/version", nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s listener: GET /version status %d, want 404", name, rec.Code)
		}
	}
}

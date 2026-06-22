package broker

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEndToEndProvisionThenCall(t *testing.T) {
	// A fake MCP backend on the host side.
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, "backend-ok")
	}))
	defer up.Close()

	b := newTestBroker(t)
	b.policy.Routes = []Route{{Operation: "qmd", PathPrefix: "/mcp/qmd/", Backend: up.URL}}

	// Start the broker over mTLS.
	srvCertPEM, srvKeyPEM, err := b.ca.IssueServerCert("example.test")
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg, err := b.ca.ServerTLSConfig(srvCertPEM, srvKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(b.Handler())
	srv.TLS = tlsCfg
	srv.StartTLS()
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(b.ca.Cert)

	// 1. Manager provisions grove "scratch".
	mgrCertPEM, mgrKeyPEM, _ := b.ca.IssueAgentCert("manager")
	mgrCert, _ := tls.X509KeyPair(mgrCertPEM, mgrKeyPEM)
	mgrClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{mgrCert}, RootCAs: pool, ServerName: "example.test",
	}}}
	body, _ := json.Marshal(ProvisionRequest{Grove: "scratch"})
	resp, err := mgrClient.Post(srv.URL+"/provision", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var pr ProvisionResponse
	_ = json.NewDecoder(resp.Body).Decode(&pr)
	resp.Body.Close()
	if pr.Biscuit == "" {
		t.Fatal("no biscuit provisioned")
	}

	// 2. Grove "scratch" uses its provisioned cert + biscuit to call qmd.
	scratchCert, err := tls.X509KeyPair([]byte(pr.Cert), []byte(pr.Key))
	if err != nil {
		t.Fatal(err)
	}
	scratchClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{scratchCert}, RootCAs: pool, ServerName: "example.test",
	}}}
	req, _ := http.NewRequest("POST", srv.URL+"/mcp/qmd/tools/call", nil)
	rawBiscuit, _ := base64.RawURLEncoding.DecodeString(pr.Biscuit)
	req.Header.Set("Authorization", "Bearer "+base64.RawURLEncoding.EncodeToString(rawBiscuit))
	callResp, err := scratchClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer callResp.Body.Close()
	if callResp.StatusCode != http.StatusOK {
		t.Fatalf("call status = %d", callResp.StatusCode)
	}
	if gotPath != "/tools/call" {
		t.Errorf("backend path = %q", gotPath)
	}
}

func TestEndToEndDeniesUngrantedOpWithoutReachingBackend(t *testing.T) {
	var backendHit bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendHit = true
		_, _ = io.WriteString(w, "should-not-happen")
	}))
	defer up.Close()

	b := newTestBroker(t)
	// scratch is granted {qmd, github.api} by samplePolicy; "calendar" is NOT granted.
	b.policy.Routes = []Route{{Operation: "calendar", PathPrefix: "/mcp/calendar/", Backend: up.URL}}

	srvCertPEM, srvKeyPEM, _ := b.ca.IssueServerCert("example.test")
	tlsCfg, _ := b.ca.ServerTLSConfig(srvCertPEM, srvKeyPEM)
	srv := httptest.NewUnstartedServer(b.Handler())
	srv.TLS = tlsCfg
	srv.StartTLS()
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(b.ca.Cert)

	// Provision scratch (grants qmd, github.api — not calendar).
	mgrCertPEM, mgrKeyPEM, _ := b.ca.IssueAgentCert("manager")
	mgrCert, _ := tls.X509KeyPair(mgrCertPEM, mgrKeyPEM)
	mgrClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{mgrCert}, RootCAs: pool, ServerName: "example.test",
	}}}
	body, _ := json.Marshal(ProvisionRequest{Grove: "scratch"})
	resp, _ := mgrClient.Post(srv.URL+"/provision", "application/json", bytes.NewReader(body))
	var pr ProvisionResponse
	_ = json.NewDecoder(resp.Body).Decode(&pr)
	resp.Body.Close()

	scratchCert, _ := tls.X509KeyPair([]byte(pr.Cert), []byte(pr.Key))
	scratchClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{scratchCert}, RootCAs: pool, ServerName: "example.test",
	}}}
	req, _ := http.NewRequest("POST", srv.URL+"/mcp/calendar/x", nil)
	req.Header.Set("Authorization", "Bearer "+pr.Biscuit)
	callResp, err := scratchClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer callResp.Body.Close()
	if callResp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (ungranted op)", callResp.StatusCode)
	}
	if backendHit {
		t.Fatal("SECURITY: backend was reached despite denial")
	}
}

func TestEndToEndNonManagerCannotProvision(t *testing.T) {
	b := newTestBroker(t)
	srvCertPEM, srvKeyPEM, _ := b.ca.IssueServerCert("example.test")
	tlsCfg, _ := b.ca.ServerTLSConfig(srvCertPEM, srvKeyPEM)
	srv := httptest.NewUnstartedServer(b.Handler())
	srv.TLS = tlsCfg
	srv.StartTLS()
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(b.ca.Cert)

	// A grove (not the manager) tries to provision.
	groveCertPEM, groveKeyPEM, _ := b.ca.IssueAgentCert("scratch")
	groveCert, _ := tls.X509KeyPair(groveCertPEM, groveKeyPEM)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{groveCert}, RootCAs: pool, ServerName: "example.test",
	}}}
	body, _ := json.Marshal(ProvisionRequest{Grove: "scratch"})
	resp, err := client.Post(srv.URL+"/provision", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (non-manager provision)", resp.StatusCode)
	}
}

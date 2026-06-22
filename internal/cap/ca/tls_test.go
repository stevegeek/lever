package ca

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMTLSHandshakeRecoversAgentIdentity(t *testing.T) {
	c, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	serverCertPEM, serverKeyPEM, err := c.IssueServerCert("example.test")
	if err != nil {
		t.Fatal(err)
	}
	srvTLS, err := c.ServerTLSConfig(serverCertPEM, serverKeyPEM)
	if err != nil {
		t.Fatal(err)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent, err := AgentFromConnState(*r.TLS)
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, agent)
	})
	srv := httptest.NewUnstartedServer(handler)
	srv.TLS = srvTLS
	srv.StartTLS()
	defer srv.Close()

	agentCertPEM, agentKeyPEM, err := c.IssueAgentCert("scratch")
	if err != nil {
		t.Fatal(err)
	}
	clientCert, err := tls.X509KeyPair(agentCertPEM, agentKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(c.Cert)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      pool,
		ServerName:   "example.test",
	}}}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "scratch" {
		t.Errorf("recovered agent = %q, want scratch", string(body))
	}
}

func TestMTLSRejectsClientWithoutCert(t *testing.T) {
	c, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	serverCertPEM, serverKeyPEM, _ := c.IssueServerCert("example.test")
	srvTLS, _ := c.ServerTLSConfig(serverCertPEM, serverKeyPEM)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.TLS = srvTLS
	srv.StartTLS()
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(c.Cert)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:    pool,
		ServerName: "example.test",
	}}}
	if _, err := client.Get(srv.URL); err == nil {
		t.Fatal("expected handshake failure when client presents no cert")
	}
}

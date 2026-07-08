package broker

// e2e_llm_test.go exercises the api-key /llm proxy over real mTLS: a live
// httptest.Server with the broker CA's ServerTLSConfig, a real enrolled worker
// cert, a capability(llm) token the worker mints for itself via /request, and a
// fake upstream that records what the proxy forwarded. The unit tests in
// llmproxy_test.go fake req.TLS with httptest recorders; this proves the full
// stack — TLS client-cert verification, token mint, strip-and-inject — together.

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/broker/registry"
	"github.com/stevegeek/lever/internal/broker/rules"
	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/cap/token"
)

// llmBrokerConfig builds a broker wired for the /llm proxy: the reserved llm
// pseudo-tool registered, worker permitted to self-obtain llm.generate, and a
// fake upstream + real key.
func llmBrokerConfig(t *testing.T, apiKey, upstreamURL string) Config {
	t.Helper()
	kp, err := token.Generate()
	if err != nil {
		t.Fatal(err)
	}
	c, err := ca.Generate()
	if err != nil {
		t.Fatal(err)
	}
	rl := rules.NewPolicy()
	rl.AllowObtain("worker", ReservedLLMTool, ReservedLLMOp)
	reg := registry.New()
	if err := reg.Register(registry.Tool{
		Name:       ReservedLLMTool,
		Backend:    "lever:llm-proxy",
		Operations: map[string]registry.Operation{ReservedLLMOp: {Name: ReservedLLMOp}},
		FirstParty: true,
	}); err != nil {
		t.Fatal(err)
	}
	return Config{
		Keys: kp, CA: c, Tickets: ca.NewTicketStore(), Rules: rl, Registry: reg,
		ManagerIdentity: "manager", Agents: []string{"manager", "worker"},
		GrantTTL: time.Hour, ServerName: e2eServerName,
		APIKey: []byte(apiKey), LLMUpstream: upstreamURL,
	}
}

func TestE2ELLMProxyOverRealMTLS(t *testing.T) {
	var gotKey, gotAuth string
	upstream := fakeAnthropic(t, &gotKey, &gotAuth)
	defer upstream.Close()

	b := New(llmBrokerConfig(t, "sk-REAL-CONSOLE-KEY", upstream.URL))
	srv := jailServer(t, b)
	defer srv.Close()

	// Provision worker (manager) → ticket.
	managerClient := agentClient(t, b, signedCert(t, b, "manager"))
	provBody, _ := json.Marshal(ProvisionRequest{Worker: "worker"})
	provResp, err := managerClient.Post(srv.URL+"/provision", "application/json", bytes.NewReader(provBody))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	defer provResp.Body.Close()
	var prov ProvisionResponse
	if err := json.NewDecoder(provResp.Body).Decode(&prov); err != nil || prov.Ticket == "" {
		t.Fatalf("provision: decode=%v ticket=%q", err, prov.Ticket)
	}

	// Enrol worker (certless) → signed cert + own key → mTLS client.
	csrPEM, keyPEM := csrWithKey(t, "worker")
	enrolBody, _ := json.Marshal(EnrolRequest{Ticket: prov.Ticket, CSR: string(csrPEM)})
	enrolResp, err := agentClient(t, b, tls.Certificate{}).Post(srv.URL+"/enrol", "application/json", bytes.NewReader(enrolBody))
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}
	defer enrolResp.Body.Close()
	var enr EnrolResponse
	if err := json.NewDecoder(enrolResp.Body).Decode(&enr); err != nil || enr.Cert == "" {
		t.Fatalf("enrol: decode=%v cert empty=%v", err, enr.Cert == "")
	}
	workerCert, err := tls.X509KeyPair([]byte(enr.Cert), keyPEM)
	if err != nil {
		t.Fatalf("enrol: build tls cert: %v", err)
	}
	workerClient := agentClient(t, b, workerCert)

	// Worker self-mints a capability(llm) token via /request (the lever-agent flow).
	capBody, _ := json.Marshal(CapRequest{Tool: ReservedLLMTool, Op: ReservedLLMOp})
	capResp, err := workerClient.Post(srv.URL+"/request", "application/json", bytes.NewReader(capBody))
	if err != nil {
		t.Fatalf("request llm token: %v", err)
	}
	defer capResp.Body.Close()
	if capResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(capResp.Body)
		t.Fatalf("request llm token: status=%d body=%s", capResp.StatusCode, body)
	}
	var cap CapResponse
	if err := json.NewDecoder(capResp.Body).Decode(&cap); err != nil || cap.Token == "" {
		t.Fatalf("request llm token: decode=%v token empty=%v", err, cap.Token == "")
	}

	// Use it at /llm exactly as Claude Code would: ANTHROPIC_AUTH_TOKEN as a bearer.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/llm/v1/messages", strings.NewReader(`{"model":"claude-x"}`))
	req.Header.Set("Authorization", "Bearer "+cap.Token)
	resp, err := workerClient.Do(req)
	if err != nil {
		t.Fatalf("llm proxy call: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("llm proxy: status=%d body=%s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if gotKey != "sk-REAL-CONSOLE-KEY" {
		t.Errorf("upstream x-api-key = %q, want the injected real key", gotKey)
	}
	if gotAuth != "" {
		t.Errorf("capability token leaked upstream as Authorization=%q (must be stripped)", gotAuth)
	}
	if strings.Contains(string(body), "sk-REAL-CONSOLE-KEY") {
		t.Error("real key leaked back into the jail-facing response body")
	}
}

func TestE2ELLMProxyRejectsTokenBoundToAnotherAgent(t *testing.T) {
	// A worker presenting a token minted for a DIFFERENT CN (manager) must be denied
	// over real mTLS — the CN-binding check is the non-transferability guarantee, and
	// the upstream (real key) must never be reached.
	var gotKey, gotAuth string
	upstream := fakeAnthropic(t, &gotKey, &gotAuth)
	defer upstream.Close()

	b := New(llmBrokerConfig(t, "sk-REAL-CONSOLE-KEY", upstream.URL))
	srv := jailServer(t, b)
	defer srv.Close()

	// Worker is a legitimately enrolled identity (use a CA-signed worker cert).
	workerClient := agentClient(t, b, signedCert(t, b, "worker"))
	// ...but the token is bound to "manager", not "worker".
	foreignToken := mintLLM(t, b.keys.Private, "manager", b.MinEpoch())

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/llm/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+foreignToken)
	resp, err := workerClient.Do(req)
	if err != nil {
		t.Fatalf("llm proxy call: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (token bound to another agent)", resp.StatusCode)
	}
	if gotKey != "" {
		t.Fatal("SECURITY: foreign-bound token reached the upstream (real key forwarded)")
	}
}

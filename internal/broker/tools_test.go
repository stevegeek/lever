package broker

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/lever-to/lever/internal/broker/registry"
	"github.com/lever-to/lever/internal/broker/rules"
	"github.com/lever-to/lever/internal/cap/ca"
	"github.com/lever-to/lever/internal/cap/token"
)

// toolsBroker builds a minimal broker with an empty registry so we can
// register exactly the tools each test needs.
func toolsBroker(t *testing.T) *Broker {
	t.Helper()
	kp, err := token.Generate()
	if err != nil {
		t.Fatal(err)
	}
	c, err := ca.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return New(Config{
		Keys: kp, CA: c, Tickets: ca.NewTicketStore(),
		Rules:           rules.NewPolicy(),
		Registry:        registry.New(),
		ManagerIdentity: "manager",
		Agents:          []string{"manager", "worker"},
	})
}

func TestToolsListsRegisteredToolsForAgent(t *testing.T) {
	b := toolsBroker(t)
	if err := b.reg.Register(regTool("db", "http://127.0.0.1:3201", "read")); err != nil {
		t.Fatal(err)
	}
	srv := jailServer(t, b)
	defer srv.Close()

	cert := signedCert(t, b, "worker")
	client := agentClient(t, b, cert)

	resp, err := client.Get(srv.URL + "/tools")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /tools status = %d, want 200; body = %s", resp.StatusCode, body)
	}
	var out struct {
		Tools []string `json:"tools"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 1 || out.Tools[0] != "db" {
		t.Fatalf("tools = %v, want [db]", out.Tools)
	}
}

// TestToolsOmitsReservedLLMTool verifies that /tools never exposes the reserved
// "llm" pseudo-tool even when it is present in the registry.
func TestToolsOmitsReservedLLMTool(t *testing.T) {
	b := toolsBroker(t)
	if err := b.reg.Register(regTool("db", "http://127.0.0.1:3201", "read")); err != nil {
		t.Fatal(err)
	}
	// Also register the reserved llm pseudo-tool (as BuildBroker would for api-key mode).
	if err := b.reg.Register(registry.Tool{
		Name:       ReservedLLMTool,
		Backend:    "lever:llm-proxy",
		Operations: map[string]registry.Operation{ReservedLLMOp: {Name: ReservedLLMOp}},
		FirstParty: true,
	}); err != nil {
		t.Fatal(err)
	}
	srv := jailServer(t, b)
	defer srv.Close()

	cert := signedCert(t, b, "worker")
	client := agentClient(t, b, cert)

	resp, err := client.Get(srv.URL + "/tools")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /tools status = %d, body = %s", resp.StatusCode, body)
	}
	var out struct {
		Tools []string `json:"tools"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	for _, n := range out.Tools {
		if n == ReservedLLMTool {
			t.Fatalf("/tools must not expose the reserved llm pseudo-tool; got %v", out.Tools)
		}
	}
}

func TestToolsRejectsNonGet(t *testing.T) {
	b := toolsBroker(t)
	srv := jailServer(t, b)
	defer srv.Close()

	cert := signedCert(t, b, "worker")
	client := agentClient(t, b, cert)

	resp, err := client.Post(srv.URL+"/tools", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /tools status = %d, want 405", resp.StatusCode)
	}
}

func TestToolsRejectsUnauthenticatedClient(t *testing.T) {
	b := toolsBroker(t)
	srv := jailServer(t, b)
	defer srv.Close()

	// agentClient with zero cert = no client cert presented.
	client := agentClient(t, b, tls.Certificate{})

	resp, err := client.Get(srv.URL + "/tools")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("GET /tools (no cert) status = %d, want 403", resp.StatusCode)
	}
}

func TestToolsDeniesRevokedAgent(t *testing.T) {
	b := toolsBroker(t)
	if err := b.reg.Register(regTool("db", "http://127.0.0.1:3201", "read")); err != nil {
		t.Fatal(err)
	}
	b.Revoke("worker")
	srv := jailServer(t, b)
	defer srv.Close()

	client := agentClient(t, b, signedCert(t, b, "worker"))
	resp, err := client.Get(srv.URL + "/tools")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("revoked /tools status = %d, want 403", resp.StatusCode)
	}
}

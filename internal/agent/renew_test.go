package agent

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRefreshLLMTokenWritesAnthropicOverlay(t *testing.T) {
	env := testBroker(t)
	ticket := provisionAs(t, env.Broker, env.Server, env.CA, "worker")
	id, err := Enrol(context.Background(), env.Server.URL, env.CA.CertPEM(), ticket, "worker")
	if err != nil {
		t.Fatal(err)
	}

	overlay := map[string]string{"EXISTING_KEY": "existing_val"}
	stub := func(_ context.Context, _ string, _ *http.Client, cn string) (string, error) {
		return "STUBTOKEN_" + cn, nil
	}
	if err := RefreshLLMToken(context.Background(), env.Server.URL, id, "worker", stub, overlay); err != nil {
		t.Fatal(err)
	}

	if got := overlay["ANTHROPIC_AUTH_TOKEN"]; got != "STUBTOKEN_worker" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN = %q, want STUBTOKEN_worker", got)
	}
	if !strings.HasSuffix(overlay["ANTHROPIC_BASE_URL"], "/llm") {
		t.Errorf("ANTHROPIC_BASE_URL = %q, want suffix /llm", overlay["ANTHROPIC_BASE_URL"])
	}
	// Must not clobber pre-existing overlay keys.
	if overlay["EXISTING_KEY"] != "existing_val" {
		t.Error("RefreshLLMToken must not clobber pre-existing overlay keys")
	}
}

func TestRenewReturnsFreshCertSameCN(t *testing.T) {
	env := testBroker(t)
	ticket := provisionAs(t, env.Broker, env.Server, env.CA, "worker")
	id, err := Enrol(context.Background(), env.Server.URL, env.CA.CertPEM(), ticket, "worker")
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := Renew(context.Background(), env.Server.URL, id)
	if err != nil {
		t.Fatal(err)
	}
	if parseLeaf(t, renewed.CertPEM).Subject.CommonName != "worker" {
		t.Fatal("renewed cert must keep the authenticated CN")
	}
	if string(renewed.KeyPEM) == string(id.KeyPEM) {
		t.Fatal("renew must rotate the keypair")
	}
	if !ValidCert(renewed.CertPEM, time.Now()) {
		t.Fatal("renewed cert must be valid")
	}
}

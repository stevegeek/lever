package agent

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRefreshLLMTokenFailsClosedLeavesOverlayUntouched(t *testing.T) {
	// Fail closed (renew.go:54): the overlay map is only mutated after a successful
	// token acquisition. A failed refresh must leave the overlay byte-identical — it
	// must not add a stale/empty ANTHROPIC_AUTH_TOKEN that a long-running sidecar
	// would then write over the live, working config.
	env := testBroker(t)
	ticket := provisionAs(t, env.Broker, env.Server, env.CA, "worker")
	id, err := Enrol(context.Background(), env.Server.URL, env.CA.CertPEM(), ticket, "worker")
	if err != nil {
		t.Fatal(err)
	}
	before := map[string]string{"ANTHROPIC_AUTH_TOKEN": "STILL_GOOD", "OTHER": "x"}
	overlay := map[string]string{"ANTHROPIC_AUTH_TOKEN": "STILL_GOOD", "OTHER": "x"}
	failing := func(context.Context, string, *http.Client, string) (string, error) {
		return "", errors.New("broker refused")
	}
	if err := RefreshLLMToken(context.Background(), env.Server.URL, id, "worker", failing, overlay); err == nil {
		t.Fatal("RefreshLLMToken must return the requestFn error")
	}
	if !reflect.DeepEqual(overlay, before) {
		t.Fatalf("failed refresh mutated the overlay: got %v, want %v", overlay, before)
	}
}

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

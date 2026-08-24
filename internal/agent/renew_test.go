package agent

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRefreshLLMTokenFailsClosedLeavesOverlayUntouched(t *testing.T) {
	// Fail closed: the overlay map is only mutated after a successful token
	// acquisition. A failed refresh must leave the overlay byte-identical — it
	// must not add a stale/empty ANTHROPIC_AUTH_TOKEN that a long-running sidecar
	// would then write over the live, working config. The broker here grants
	// "worker" no llm obtain, so /request refuses.
	env := testBroker(t)
	id := enrolWorker(t, env)
	before := map[string]string{"ANTHROPIC_AUTH_TOKEN": "STILL_GOOD", "OTHER": "x"}
	overlay := map[string]string{"ANTHROPIC_AUTH_TOKEN": "STILL_GOOD", "OTHER": "x"}
	if err := RefreshLLMToken(context.Background(), env.Server.URL, id, overlay); err == nil {
		t.Fatal("RefreshLLMToken must return the broker's refusal")
	}
	if !reflect.DeepEqual(overlay, before) {
		t.Fatalf("failed refresh mutated the overlay: got %v, want %v", overlay, before)
	}
}

func TestRefreshLLMTokenWritesAnthropicOverlay(t *testing.T) {
	env := testBroker(t)
	allowLLM(t, env, "worker")
	id := enrolWorker(t, env)

	overlay := map[string]string{"EXISTING_KEY": "existing_val"}
	if err := RefreshLLMToken(context.Background(), env.Server.URL, id, overlay); err != nil {
		t.Fatal(err)
	}
	if overlay["ANTHROPIC_AUTH_TOKEN"] == "" {
		t.Error("ANTHROPIC_AUTH_TOKEN must be set from the broker's token")
	}
	// Host pinning (same as boot): Claude posts LLM traffic to the loopback
	// gateway, which presents the rotating leaf on its behalf. Pointing renew
	// back at the mTLS broker re-introduces the 24h cached-leaf outage. The
	// literal here is an independent pin of LocalGatewayURL; env.Server.URL is
	// the broker in this test, so the second check proves the base URL is
	// explicitly NOT the broker.
	if want := "http://127.0.0.1:8462/llm"; overlay["ANTHROPIC_BASE_URL"] != want {
		t.Errorf("ANTHROPIC_BASE_URL = %q, want %q (must point at the loopback gateway, not the broker)", overlay["ANTHROPIC_BASE_URL"], want)
	}
	if strings.HasPrefix(overlay["ANTHROPIC_BASE_URL"], env.Server.URL) {
		t.Errorf("ANTHROPIC_BASE_URL = %q points at the broker (%s) — renew must write the gateway URL", overlay["ANTHROPIC_BASE_URL"], env.Server.URL)
	}
	// Must not clobber pre-existing overlay keys.
	if overlay["EXISTING_KEY"] != "existing_val" {
		t.Error("RefreshLLMToken must not clobber pre-existing overlay keys")
	}
}

// TestRenewOnceAPIKeyRefreshesSettings exercises the api-key branch of RenewOnce
// (cert rotation + llm-token refresh + settings rewrite) against a real mTLS
// broker. It pins the env keys the rewritten settings.json must carry: the
// classic-renderer flag, a fresh ANTHROPIC_AUTH_TOKEN, and a gateway-hosted
// ANTHROPIC_BASE_URL — NOT the broker.
func TestRenewOnceAPIKeyRefreshesSettings(t *testing.T) {
	env := testBroker(t)
	allowLLM(t, env, "worker")
	id := enrolWorker(t, env)
	idDir := t.TempDir()
	if err := id.Write(idDir); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	if err := RenewOnce(context.Background(), RenewConfig{
		Identity:     id,
		IDDir:        idDir,
		BrokerURL:    env.Server.URL,
		LLMAuth:      LLMAuthAPIKey,
		SettingsPath: settingsPath,
	}); err != nil {
		t.Fatalf("RenewOnce: %v", err)
	}
	rotated, ok := LoadIdentity(idDir)
	if !ok || string(rotated.CertPEM) == string(id.CertPEM) {
		t.Fatal("RenewOnce must write the rotated identity back to IDDir")
	}
	envBlock := readSettingsEnv(t, settingsPath)
	if envBlock["CLAUDE_CODE_DISABLE_ALTERNATE_SCREEN"] != "1" {
		t.Errorf("settings env missing the harness overlay: %v", envBlock)
	}
	if envBlock["ANTHROPIC_AUTH_TOKEN"] == "" {
		t.Error("api-key renew must write a fresh ANTHROPIC_AUTH_TOKEN")
	}
	if want := "http://127.0.0.1:8462/llm"; envBlock["ANTHROPIC_BASE_URL"] != want {
		t.Errorf("ANTHROPIC_BASE_URL = %q, want %q (loopback gateway, not the broker)", envBlock["ANTHROPIC_BASE_URL"], want)
	}
}

// TestRenewOnceSubscriptionLeavesSettingsAlone: without api-key mode the settings
// file is not touched at all, even when a path is given.
func TestRenewOnceSubscriptionLeavesSettingsAlone(t *testing.T) {
	env := testBroker(t)
	id := enrolWorker(t, env)
	idDir := t.TempDir()
	if err := id.Write(idDir); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	if err := RenewOnce(context.Background(), RenewConfig{
		Identity: id, IDDir: idDir, BrokerURL: env.Server.URL,
		LLMAuth: LLMAuthSubscription, SettingsPath: settingsPath,
	}); err != nil {
		t.Fatalf("RenewOnce: %v", err)
	}
	if _, err := os.Stat(settingsPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("subscription renew must not write settings.json; stat err = %v", err)
	}
}

func TestRenewReturnsFreshCertSameCN(t *testing.T) {
	env := testBroker(t)
	ticket := env.ProvisionWorker(t, "worker")
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

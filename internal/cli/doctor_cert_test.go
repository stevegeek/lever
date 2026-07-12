package cli

import (
	"os"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/brokerctl"
)

func writeBrokerLog(t *testing.T, st brokerctl.State, content string) {
	t.Helper()
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.OutLog(), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// certExpiredLine is the real broker fingerprint for an expired client leaf.
func certExpiredLine(ts time.Time) string {
	return ts.Format("2006/01/02 15:04:05") +
		" http: TLS handshake error from 127.0.0.1: tls: failed to verify certificate: x509: certificate has expired or is not yet valid"
}

func TestCheckAgentCert_ActiveRejectionFails(t *testing.T) {
	now := time.Now()
	st := brokerctl.State{Dir: t.TempDir()}
	writeBrokerLog(t, st, "prior noise\n"+certExpiredLine(now.Add(-2*time.Minute))+"\n")
	r := checkAgentCert(st, now)
	if r.ok {
		t.Fatalf("want FAIL for a recent expired-leaf rejection, got ok: %q", r.detail)
	}
	if r.fix == "" {
		t.Error("want a fix hint on failure")
	}
}

func TestCheckAgentCert_StaleRejectionPasses(t *testing.T) {
	now := time.Now()
	st := brokerctl.State{Dir: t.TempDir()}
	writeBrokerLog(t, st, certExpiredLine(now.Add(-2*time.Hour))+"\n")
	if r := checkAgentCert(st, now); !r.ok {
		t.Fatalf("want PASS for a stale (healed) rejection, got fail: %q", r.detail)
	}
}

func TestCheckAgentCert_BadCertFingerprintFails(t *testing.T) {
	now := time.Now()
	st := brokerctl.State{Dir: t.TempDir()}
	line := now.Add(-1*time.Minute).Format("2006/01/02 15:04:05") +
		" http: TLS handshake error from 127.0.0.1: remote error: tls: bad certificate"
	writeBrokerLog(t, st, line+"\n")
	if r := checkAgentCert(st, now); r.ok {
		t.Fatalf("want FAIL for a recent 'tls: bad certificate' rejection, got ok: %q", r.detail)
	}
}

func TestCheckAgentCert_NoRejectionsPasses(t *testing.T) {
	now := time.Now()
	st := brokerctl.State{Dir: t.TempDir()}
	writeBrokerLog(t, st, "2026/07/08 09:00:00 http: TLS handshake succeeded\nunrelated line\n")
	if r := checkAgentCert(st, now); !r.ok {
		t.Fatalf("want PASS when no expired-leaf lines present, got fail: %q", r.detail)
	}
}

func TestCheckAgentCert_NoLogFilePasses(t *testing.T) {
	now := time.Now()
	st := brokerctl.State{Dir: t.TempDir()} // no broker.out.log written
	if r := checkAgentCert(st, now); !r.ok {
		t.Fatalf("want PASS when the broker log is absent (nothing to diagnose), got fail: %q", r.detail)
	}
}

// stampBrokerPID writes a pid file whose mtime is the broker's start time
// (reuses writeBrokerPID from doctor_checks_test.go, then backdates the mtime).
func stampBrokerPID(t *testing.T, st brokerctl.State, started time.Time) {
	t.Helper()
	writeBrokerPID(t, st, 12345)
	if err := os.Chtimes(st.PID(), started, started); err != nil {
		t.Fatal(err)
	}
}

func TestCheckAgentCert_RejectionBeforeBrokerRestartPasses(t *testing.T) {
	now := time.Now()
	st := brokerctl.State{Dir: t.TempDir()}
	// Rejection is recent (inside the window) but the broker restarted AFTER it
	// — the restart is the remedy, so the check must not cry wolf post-heal.
	writeBrokerLog(t, st, certExpiredLine(now.Add(-2*time.Minute))+"\n")
	stampBrokerPID(t, st, now.Add(-1*time.Minute))
	if r := checkAgentCert(st, now); !r.ok {
		t.Fatalf("want PASS for a rejection predating the current broker, got fail: %q", r.detail)
	}
}

func TestCheckAgentCert_RejectionAfterBrokerStartStillFails(t *testing.T) {
	now := time.Now()
	st := brokerctl.State{Dir: t.TempDir()}
	// Broker started BEFORE the rejection: the current broker is failing.
	writeBrokerLog(t, st, certExpiredLine(now.Add(-2*time.Minute))+"\n")
	stampBrokerPID(t, st, now.Add(-10*time.Minute))
	if r := checkAgentCert(st, now); r.ok {
		t.Fatalf("want FAIL when the current broker logged the rejection, got ok: %q", r.detail)
	}
}

func TestScanBrokerLogCertExpiry_PicksLatest(t *testing.T) {
	now := time.Now()
	st := brokerctl.State{Dir: t.TempDir()}
	older := now.Add(-30 * time.Minute)
	newer := now.Add(-1 * time.Minute)
	// Out of chronological order in the file: the scan must still pick the newest.
	writeBrokerLog(t, st, certExpiredLine(newer)+"\n"+certExpiredLine(older)+"\n")
	got, found, err := scanBrokerLogCertExpiry(st.OutLog())
	if err != nil || !found {
		t.Fatalf("found=%v err=%v, want found and no error", found, err)
	}
	if got.Format("2006/01/02 15:04:05") != newer.Format("2006/01/02 15:04:05") {
		t.Errorf("latest = %s, want %s", got, newer)
	}
}

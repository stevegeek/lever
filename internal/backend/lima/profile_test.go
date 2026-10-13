package lima

import (
	"context"
	"testing"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/common"
	"github.com/stevegeek/lever/internal/backend/guest"
	"github.com/stevegeek/lever/internal/proc"
)

// The guest profile converges after the hardening and before provisioning,
// so the small profile's stopped apt timers cannot compete with its apt run;
// no profile (no config) runs no script.
func TestEnsureUpGuestProfile(t *testing.T) {
	for _, profile := range []string{guest.GuestProfileSmall, guest.GuestProfileStandard} {
		f := proc.NewFakeRunner()
		scriptedVM(f)
		l := New(f, vm, common.Options{})
		if err := l.EnsureUp(context.Background(), backend.Config{MachineName: vm, ProjectTree: tree, GuestProfile: profile}); err != nil {
			t.Fatalf("%s: EnsureUp: %v", profile, err)
		}
		crash := rootBashScript(f, guest.CrashSysctlPath)
		prof := rootBashScript(f, guest.ZramUnitName)
		whoami := callIndex(f, "shell", vm, "whoami")
		if prof < 0 {
			t.Fatalf("%s: no profile script; calls=%+v", profile, f.Calls)
		}
		if !(crash < prof && prof < whoami) {
			t.Errorf("%s: order crash=%d profile=%d whoami(provision)=%d", profile, crash, prof, whoami)
		}
	}
	f := proc.NewFakeRunner()
	scriptedVM(f)
	if err := New(f, vm, common.Options{}).EnsureUp(context.Background(), backend.Config{MachineName: vm, ProjectTree: tree}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	if rootBashScript(f, guest.ZramUnitName) >= 0 {
		t.Error("no guest_profile known: the guest must be left as it is")
	}
}

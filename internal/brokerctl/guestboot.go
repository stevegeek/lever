package brokerctl

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/state"
)

// guestBootIDPath changes on every boot of the guest kernel.
const guestBootIDPath = "/proc/sys/kernel/random/boot_id"

// bootIDRE is the shape of a kernel boot id (a random UUID). The value
// comes from the guest, so anything else is refused rather than stored.
var bootIDRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// JailGuestBoot is apply.GuestBoot over the jail runner and the guest boot
// in the state directory's lifecycle record, for apply's wiring and the
// broker's bounce of the manager alike. Use GuestBootFor, which keeps it off
// a state directory inside the tree.
type JailGuestBoot struct {
	R     proc.Runner
	State state.State
	Now   func() time.Time
}

// guestBootIDOutputLimit caps the boot id read: guest root writes that
// output, and the broker's jail runner has no default cap.
const guestBootIDOutputLimit = 64 << 10

func (g JailGuestBoot) Current(ctx context.Context) (string, error) {
	res, err := g.R.Run(proc.WithOutputLimit(ctx, guestBootIDOutputLimit), nil, "cat", guestBootIDPath)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(res.Stdout)
	if !bootIDRE.MatchString(id) {
		return "", fmt.Errorf("guest boot id: unexpected value")
	}
	return id, nil
}

func (g JailGuestBoot) Recorded() (string, error) {
	l, err := g.State.LoadLifecycle()
	if err != nil {
		return "", err
	}
	if l.GuestBoot == nil || !bootIDRE.MatchString(l.GuestBoot.BootID) {
		return "", nil
	}
	return l.GuestBoot.BootID, nil
}

func (g JailGuestBoot) Record(id string) error {
	now := g.Now
	if now == nil {
		now = time.Now
	}
	return g.State.UpdateLifecycle(func(l *state.Lifecycle) {
		l.GuestBoot = &state.GuestBootRecord{BootID: id, RecordedAt: now().UTC()}
	})
}

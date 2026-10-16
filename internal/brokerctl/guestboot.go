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

type guestBootRecord struct {
	BootID     string    `json:"boot_id"`
	RecordedAt time.Time `json:"recorded_at"`
}

// JailGuestBoot is apply.GuestBoot over the jail runner and the state
// directory's guest boot record, for apply's wiring and the broker's bounce
// of the manager alike.
type JailGuestBoot struct {
	R     proc.Runner
	State state.State
	Now   func() time.Time
}

func (g JailGuestBoot) Current(ctx context.Context) (string, error) {
	res, err := g.R.Run(ctx, nil, "cat", guestBootIDPath)
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
	r, err := state.LoadJSON[guestBootRecord](g.State.GuestBoot(), "guest boot record")
	if err != nil {
		return "", err
	}
	if !bootIDRE.MatchString(r.BootID) {
		return "", nil
	}
	return r.BootID, nil
}

func (g JailGuestBoot) Record(id string) error {
	now := g.Now
	if now == nil {
		now = time.Now
	}
	return state.SaveJSON(g.State.GuestBoot(), "guest boot record", guestBootRecord{BootID: id, RecordedAt: now().UTC()})
}

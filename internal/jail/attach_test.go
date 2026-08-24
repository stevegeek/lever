package jail

import (
	"reflect"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

func TestAttachArgv(t *testing.T) {
	inner := []string{"scion", "attach", "demo", "-g", "/lever"}
	jr := New(Config{Host: proc.NewFakeRunner(), Prefix: orbPrefix("lever-demo", "leveruser"), UID: "501"})
	got := jr.AttachArgv(inner)
	// Default: own pasta netns, so SCION_FORCE_HOST_NETWORK is NOT emitted.
	want := []string{
		"orb", "-m", "lever-demo", "-u", "leveruser", "env",
		"XDG_RUNTIME_DIR=/run/user/501",
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"SCION_HUB_ENABLED=true",
		"scion", "attach", "demo", "-g", "/lever",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AttachArgv =\n %v\nwant\n %v", got, want)
	}
}

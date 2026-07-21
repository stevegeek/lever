package jail

import (
	"reflect"
	"testing"
)

func TestAttachArgv(t *testing.T) {
	t.Setenv("LEVER_FORCE_HOST_NETWORK", "") // pin the default regardless of ambient env
	inner := []string{"scion", "attach", "demo", "-g", "/lever"}
	got := AttachArgv(OrbPrefix("lever-demo", "leveruser"), "501", inner)
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

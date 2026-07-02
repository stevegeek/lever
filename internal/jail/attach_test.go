package jail

import (
	"reflect"
	"testing"
)

func TestAttachArgv(t *testing.T) {
	inner := []string{"scion", "attach", "demo", "-g", "/lever"}
	got := AttachArgv(OrbPrefix("lever-demo", "leveruser"), "501", inner)
	want := []string{
		"orb", "-m", "lever-demo", "-u", "leveruser", "env",
		"XDG_RUNTIME_DIR=/run/user/501",
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"SCION_HUB_ENABLED=true",
		"SCION_FORCE_HOST_NETWORK=1",
		"scion", "attach", "demo", "-g", "/lever",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AttachArgv =\n %v\nwant\n %v", got, want)
	}
}

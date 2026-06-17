package jail

import (
	"reflect"
	"testing"
)

func TestAttachArgv(t *testing.T) {
	inner := []string{"scion", "attach", "assistant", "-g", "/lever"}
	got := AttachArgv("lever-assistant", "leveruser", "501", inner)
	want := []string{
		"orb", "-m", "lever-assistant", "-u", "leveruser", "env",
		"XDG_RUNTIME_DIR=/run/user/501",
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"SCION_HUB_ENABLED=true",
		"SCION_FORCE_HOST_NETWORK=1",
		"scion", "attach", "assistant", "-g", "/lever",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AttachArgv =\n %v\nwant\n %v", got, want)
	}
}

package jail

import (
	"reflect"
	"testing"
)

func TestLoadImageArgs(t *testing.T) {
	got := LoadImageArgs(OrbPrefix("lever-demo", "leveruser"), "501")
	want := []string{
		"orb", "-m", "lever-demo", "-u", "leveruser",
		"env",
		"XDG_RUNTIME_DIR=/run/user/501",
		"podman", "load",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LoadImageArgs:\n got  %v\n want %v", got, want)
	}
}

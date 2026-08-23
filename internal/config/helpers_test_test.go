package config

import (
	"errors"
	"fmt"
	"testing"
)

// recordingTB captures Fatalf instead of stopping the test, so the assertion
// helpers' negative cases can be observed.
type recordingTB struct {
	testing.TB
	fatal string
}

func (r *recordingTB) Helper() {}
func (r *recordingTB) Fatalf(format string, args ...any) {
	r.fatal = fmt.Sprintf(format, args...)
}

func TestWantErrContaining(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		substrs []string
		wantOK  bool
	}{
		{"all fragments present", errors.New(`config: unknown backend "x"`), []string{"unknown backend", "x"}, true},
		{"nil error", nil, []string{"unknown backend"}, false},
		{"one fragment missing", errors.New("config: unknown backend"), []string{"unknown backend", "vmware"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &recordingTB{TB: t}
			wantErrContaining(r, tc.err, tc.substrs...)
			if ok := r.fatal == ""; ok != tc.wantOK {
				t.Fatalf("passed = %v, want %v (fatal: %q)", ok, tc.wantOK, r.fatal)
			}
		})
	}
}

func TestRejectNoHost(t *testing.T) {
	rejectNoHost(t, "name: x\nbackend: vmware\ntree: ./tree\nmanager: {}\n", msgUnknownBackend)
}

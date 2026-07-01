package cli

import (
	"bytes"
	"context"
	"testing"

	"github.com/lever-to/lever/internal/backend"
)

type stubBackend struct{ up, down bool }

func (s *stubBackend) EnsureUp(context.Context, backend.Config) error { s.up = true; return nil }
func (s *stubBackend) DockerHost() string                             { return "unix:///x" }
func (s *stubBackend) HostToolAlias() string                          { return "host.orb.internal" }
func (s *stubBackend) MountDest() string                              { return "/lever" }
func (s *stubBackend) ApplyEgress(context.Context, []int, bool) error { return nil }
func (s *stubBackend) Teardown(context.Context) error                 { s.down = true; return nil }
func (s *stubBackend) Profile() backend.Profile                       { return backend.Profile{Name: "stub"} }

func TestUpCommandCallsEnsureUp(t *testing.T) {
	sb := &stubBackend{}
	root := NewRootWithBackend(func(string) backend.Backend { return sb })
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"provision", "--machine", "lever-jail", "--tree", "/tmp/tree", "--allow-port", "3305"})
	if err := root.Execute(); err != nil {
		t.Fatalf("up: %v", err)
	}
	if !sb.up {
		t.Fatal("EnsureUp not called")
	}
}

func TestDoctorPrintsProfile(t *testing.T) {
	root := NewRootWithBackend(func(string) backend.Backend { return &stubBackend{} })
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"doctor", "--machine", "lever-jail"})
	if err := root.Execute(); err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if out.Len() == 0 {
		t.Fatal("doctor printed nothing")
	}
}

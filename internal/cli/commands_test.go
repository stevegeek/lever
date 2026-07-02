package cli

import (
	"bytes"
	"context"
	"testing"

	"github.com/lever-to/lever/internal/backend"
	leverexec "github.com/lever-to/lever/internal/exec"
)

type stubBackend struct{ up, down bool }

func (s *stubBackend) EnsureUp(context.Context, backend.Config) error { s.up = true; return nil }
func (s *stubBackend) DockerHost() string                             { return "unix:///x" }
func (s *stubBackend) HostToolAlias() string                          { return "host.orb.internal" }
func (s *stubBackend) MountDest() string                              { return "/lever" }
func (s *stubBackend) ApplyEgress(context.Context, []int, bool) error { return nil }
func (s *stubBackend) Teardown(context.Context) error                 { s.down = true; return nil }
func (s *stubBackend) Profile() backend.Profile                       { return backend.Profile{Name: "stub"} }
func (s *stubBackend) HostAliasV4() string                            { return "" }
func (s *stubBackend) MachineName() string                            { return "lever-stub" }
func (s *stubBackend) RunUser() string                                { return "stub" }
func (s *stubBackend) RunUID() string                                 { return "501" }
func (s *stubBackend) JailRunner() leverexec.Runner                   { return leverexec.RealRunner{} }
func (s *stubBackend) AttachArgv(inner []string) []string {
	return append([]string{"stub-attach"}, inner...)
}
func (s *stubBackend) LoadImage(context.Context, string) error                  { return nil }
func (s *stubBackend) InstallGuestBinary(context.Context, string, string) error { return nil }

func TestUpCommandCallsEnsureUp(t *testing.T) {
	sb := &stubBackend{}
	root := NewRootWithBackend(func(string, string) (backend.Backend, error) { return sb, nil })
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
	root := NewRootWithBackend(func(string, string) (backend.Backend, error) { return &stubBackend{}, nil })
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

func TestFactoryReceivesConfiguredBackendName(t *testing.T) {
	var gotName string
	bf := func(name, machine string) (backend.Backend, error) {
		gotName = name
		return &stubBackend{}, nil
	}
	root := NewRootWithBackend(bf)
	root.SetArgs([]string{"doctor", "--machine", "lever-x", "--backend", "orbstack"})
	var out bytes.Buffer
	root.SetOut(&out)
	if err := root.Execute(); err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if gotName != "orbstack" {
		t.Fatalf("factory got name %q, want %q", gotName, "orbstack")
	}
}

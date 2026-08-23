package provision

import (
	"context"
	"testing"

	"github.com/stevegeek/lever/internal/exec"
)

func TestGoBinaryResolvesGOROOT(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("go env GOROOT", exec.Result{Stdout: "/opt/go\n"})
	got, err := GoBinary(context.Background(), f)
	if err != nil {
		t.Fatalf("GoBinary: %v", err)
	}
	if got != "/opt/go/bin/go" {
		t.Fatalf("GoBinary = %q", got)
	}
}

func TestGoBinaryErrorsWithoutGo(t *testing.T) {
	if _, err := GoBinary(context.Background(), exec.NewFakeRunner()); err == nil {
		t.Fatal("expected an error when go is not on PATH")
	}
}

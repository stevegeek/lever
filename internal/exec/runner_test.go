package exec

import (
	"context"
	"testing"
)

func TestFakeRunnerRecordsAndScripts(t *testing.T) {
	f := NewFakeRunner()
	f.Script("orb list", Result{Stdout: "lever-jail running\n"})

	res, err := f.Run(context.Background(), nil, "orb", "list")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Stdout != "lever-jail running\n" {
		t.Fatalf("stdout=%q", res.Stdout)
	}
	if len(f.Calls) != 1 || f.Calls[0].Name != "orb" || f.Calls[0].Args[0] != "list" {
		t.Fatalf("calls=%+v", f.Calls)
	}
}

func TestFakeRunnerUnscriptedErrors(t *testing.T) {
	f := NewFakeRunner()
	if _, err := f.Run(context.Background(), nil, "orb", "boom"); err == nil {
		t.Fatal("expected error for unscripted command")
	}
}

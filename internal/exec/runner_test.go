package exec

import (
	"context"
	"strings"
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

func TestRealRunnerHonorsDir(t *testing.T) {
	dir := t.TempDir()
	r := RealRunner{}
	res, err := r.RunIn(context.Background(), dir, nil, "pwd")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// macOS /tmp symlinks to /private/tmp; compare suffix.
	if got := strings.TrimSpace(res.Stdout); !strings.HasSuffix(got, dir) && !strings.HasSuffix(dir, got) {
		t.Fatalf("pwd=%q want dir=%q", got, dir)
	}
}

func TestFakeRunnerRecordsDir(t *testing.T) {
	f := NewFakeRunner()
	f.Script("scion init", Result{})
	_, _ = f.RunIn(context.Background(), "/work/a", nil, "scion", "init")
	if f.Calls[0].Dir != "/work/a" {
		t.Fatalf("dir=%q", f.Calls[0].Dir)
	}
}

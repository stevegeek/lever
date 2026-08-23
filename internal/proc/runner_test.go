package proc

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

func TestFakeRunnerRecordsStdin(t *testing.T) {
	f := NewFakeRunner()
	f.Script("orb -m m bash -c", Result{})
	_, err := f.RunStdin(context.Background(), strings.NewReader("payload\n"), nil, "orb", "-m", "m", "bash", "-c", "cat > /x")
	if err != nil {
		t.Fatalf("RunStdin: %v", err)
	}
	if len(f.Calls) != 1 || f.Calls[0].Stdin != "payload\n" {
		t.Fatalf("calls=%+v", f.Calls)
	}
	if f.Calls[0].Name != "orb" || f.Calls[0].Args[len(f.Calls[0].Args)-1] != "cat > /x" {
		t.Fatalf("argv not recorded: %+v", f.Calls[0])
	}
}

func TestRealRunnerFeedsStdin(t *testing.T) {
	r := RealRunner{}
	res, err := r.RunStdin(context.Background(), strings.NewReader("hello stdin"), nil, "cat")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Stdout != "hello stdin" {
		t.Fatalf("stdout=%q", res.Stdout)
	}
}

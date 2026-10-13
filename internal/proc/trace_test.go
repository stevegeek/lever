package proc

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestTraceHead(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"limactl", []string{"start", "--tty=false", "lever-dev"}, "limactl start …(+2)"},
		{"/usr/bin/limactl", []string{"shell", "lever-dev", "bash", "-lc", "echo secret"}, "limactl shell lever-dev bash …(+2)"},
		{"scion", []string{"hub", "secret", "set", "NAME", "VALUE"}, "scion hub …"},
		{"scion", []string{"server", "start", "--session-secret=abc"}, "scion server start …(+1)"},
		{"podman", []string{"run", "-e", "TOKEN=sk-ant-oat01-xxxxxxxxxxxx"}, "podman run …(+2)"},
		{"git", []string{"status"}, "git status"},
		{"x", []string{"a", "b", "c", "d", "e", "f"}, "x a b c d …(+2)"},
		{"x", []string{"KEY=VALUE"}, "x …(+1)"},
		{"x", []string{"my api_key"}, "x …(+1)"},
		{"x", nil, "x"},
	}
	for _, c := range cases {
		if got := TraceHead(c.name, c.args); got != c.want {
			t.Errorf("TraceHead(%q, %q) = %q, want %q", c.name, c.args, got, c.want)
		}
	}
}

type recTracer struct {
	heads []string
	codes []int
}

func (r *recTracer) Command(head string, _ time.Duration, code int, _ error) {
	r.heads = append(r.heads, head)
	r.codes = append(r.codes, code)
}

func TestTracedReportsOnlyWithATracer(t *testing.T) {
	tr := &recTracer{}
	f := NewFakeRunner()
	f.Script("git status", Result{Code: 3})
	r := Traced(f)
	r.Run(context.Background(), nil, "git", "status")
	if len(tr.heads) != 0 {
		t.Fatal("reported without a tracer")
	}
	r.Run(WithTracer(context.Background(), tr), nil, "git", "status", "--porcelain")
	if strings.Join(tr.heads, ";") != "git status …(+1)" || tr.codes[0] != 3 {
		t.Fatalf("got %q %v", tr.heads, tr.codes)
	}
}

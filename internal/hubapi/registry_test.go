package hubapi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

type registryDoer struct {
	statuses []int
	calls    int
	bodies   []string
}

func (d *registryDoer) Do(ctx context.Context, method, path string) (int, []byte, error) {
	return d.DoBody(ctx, method, path, nil)
}

func (d *registryDoer) DoBody(_ context.Context, method, path string, body []byte) (int, []byte, error) {
	d.calls++
	d.bodies = append(d.bodies, method+" "+path+" "+string(body))
	st := http.StatusOK
	if len(d.statuses) > 0 {
		st, d.statuses = d.statuses[0], d.statuses[1:]
	}
	if st != http.StatusOK {
		return st, []byte(`{"error":{"code":"conflict"}}`), nil
	}
	return st, []byte(`{"image_registry":"scionlocal"}`), nil
}

func TestSetImageRegistry(t *testing.T) {
	d := &registryDoer{}
	if err := (&Client{T: d}).SetImageRegistry(context.Background(), "scionlocal"); err != nil {
		t.Fatal(err)
	}
	if d.calls != 1 || d.bodies[0] != `PUT /api/v1/system/registry {"image_registry":"scionlocal"}` {
		t.Fatalf("requests = %q", d.bodies)
	}

	// A concurrent writer's 409 is retried; a lasting one fails.
	d = &registryDoer{statuses: []int{409, 409}}
	if err := (&Client{T: d}).SetImageRegistry(context.Background(), "scionlocal"); err != nil || d.calls != 3 {
		t.Fatalf("err = %v after %d calls", err, d.calls)
	}
	d = &registryDoer{statuses: []int{409, 409, 409, 409}}
	if err := (&Client{T: d}).SetImageRegistry(context.Background(), "scionlocal"); err == nil || d.calls != registryConflictRetries {
		t.Fatalf("err = %v after %d calls", err, d.calls)
	}
	d = &registryDoer{statuses: []int{403}}
	if err := (&Client{T: d}).SetImageRegistry(context.Background(), "scionlocal"); err == nil || d.calls != 1 || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v after %d calls", err, d.calls)
	}
}

// runtimeDoer is the hub's runtime route: GET answers current (or getErr),
// PUT takes puts in order (a status, or a transport error) and on 200 stores
// the runtime, or echoes echo when set.
type runtimeDoer struct {
	current string
	getErr  error
	puts    []runtimePut
	echo    string
	bodies  []string
}

type runtimePut struct {
	status int
	err    error
	// applied keeps the write although the request failed (a timeout after
	// the hub stored it).
	applied bool
}

func (d *runtimeDoer) Do(ctx context.Context, method, path string) (int, []byte, error) {
	return d.DoBody(ctx, method, path, nil)
}

func (d *runtimeDoer) DoBody(_ context.Context, method, path string, body []byte) (int, []byte, error) {
	d.bodies = append(d.bodies, method+" "+path+" "+string(body))
	if method == http.MethodGet {
		if d.getErr != nil {
			return 0, nil, d.getErr
		}
		return 200, []byte(`{"detected":"podman","configured":"` + d.current + `","available":true}`), nil
	}
	var req struct {
		Runtime string `json:"runtime"`
	}
	_ = json.Unmarshal(body, &req)
	p := runtimePut{status: 200}
	if len(d.puts) > 0 {
		p, d.puts = d.puts[0], d.puts[1:]
	}
	if p.applied || p.status == 200 {
		d.current = req.Runtime
	}
	if p.err != nil {
		return 0, nil, p.err
	}
	if p.status != 200 {
		return p.status, []byte(`{"error":{"code":"conflict"}}`), nil
	}
	return 200, []byte(`{"configured":"` + cmp.Or(d.echo, req.Runtime) + `"}`), nil
}

func (d *runtimeDoer) putCount() int {
	n := 0
	for _, b := range d.bodies {
		if strings.HasPrefix(b, "PUT ") {
			n++
		}
	}
	return n
}

func noUnansweredDelay(t *testing.T) {
	old := unansweredDelay
	unansweredDelay = 0
	t.Cleanup(func() { unansweredDelay = old })
}

func TestSetRuntime(t *testing.T) {
	d := &runtimeDoer{current: "docker"}
	if err := (&Client{T: d}).SetRuntime(context.Background(), "podman"); err != nil {
		t.Fatal(err)
	}
	if len(d.bodies) != 2 || d.bodies[1] != `PUT /api/v1/system/runtime {"runtime":"podman"}` || d.current != "podman" {
		t.Fatalf("requests = %q", d.bodies)
	}
	d = &runtimeDoer{current: "docker", puts: []runtimePut{{status: 409}}}
	if err := (&Client{T: d}).SetRuntime(context.Background(), "podman"); err != nil || d.putCount() != 2 {
		t.Fatalf("err = %v after %q", err, d.bodies)
	}
	d = &runtimeDoer{current: "docker", echo: "docker"}
	if err := (&Client{T: d}).SetRuntime(context.Background(), "podman"); err == nil || !strings.Contains(err.Error(), "did not echo") {
		t.Fatalf("err = %v", err)
	}
	d = &runtimeDoer{current: "docker", puts: []runtimePut{{status: 403}}}
	if err := (&Client{T: d}).SetRuntime(context.Background(), "podman"); err == nil || d.putCount() != 1 || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a refusal is final: err = %v after %q", err, d.bodies)
	}
}

// Each PUT reloads the broker's runtime, so a runtime the hub already has is
// not written; a failed read falls back to the write.
func TestSetRuntimeSkipsTheWriteWhenTheHubHasIt(t *testing.T) {
	d := &runtimeDoer{current: "podman"}
	if err := (&Client{T: d}).SetRuntime(context.Background(), "podman"); err != nil || d.putCount() != 0 {
		t.Fatalf("err = %v after %q", err, d.bodies)
	}
	d = &runtimeDoer{current: "podman", getErr: errors.New("curl: (28) Operation timed out")}
	if err := (&Client{T: d}).SetRuntime(context.Background(), "podman"); err != nil || d.putCount() != 1 {
		t.Fatalf("err = %v after %q", err, d.bodies)
	}
}

// A PUT that timed out after the hub stored it is read back and not sent
// again; one that did not land is sent again; one that never gets an answer
// fails after a bounded number of tries.
func TestSetRuntimeUnansweredPut(t *testing.T) {
	noUnansweredDelay(t)
	timeout := errors.New("POST: reaching the hub from the jail: exit status 28: curl: (28) Operation timed out")

	d := &runtimeDoer{current: "docker", puts: []runtimePut{{err: timeout, applied: true}}}
	if err := (&Client{T: d}).SetRuntime(context.Background(), "podman"); err != nil || d.putCount() != 1 {
		t.Fatalf("applied but unanswered: err = %v after %q", err, d.bodies)
	}

	d = &runtimeDoer{current: "docker", puts: []runtimePut{{err: timeout}}}
	if err := (&Client{T: d}).SetRuntime(context.Background(), "podman"); err != nil || d.putCount() != 2 || d.current != "podman" {
		t.Fatalf("lost then sent again: err = %v after %q", err, d.bodies)
	}

	d = &runtimeDoer{current: "docker", puts: []runtimePut{{err: timeout}, {err: timeout}, {err: timeout}, {err: timeout}}}
	if err := (&Client{T: d}).SetRuntime(context.Background(), "podman"); err == nil || d.putCount() != unansweredAttempts ||
		!strings.Contains(err.Error(), "no answer") {
		t.Fatalf("never answered: err = %v after %q", err, d.bodies)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d = &runtimeDoer{current: "docker", getErr: context.Canceled, puts: []runtimePut{{err: context.Canceled}}}
	if err := (&Client{T: d}).SetRuntime(ctx, "podman"); err == nil || d.putCount() != 1 {
		t.Fatalf("a cancelled apply is not retried: err = %v after %q", err, d.bodies)
	}
}

// The registry PUT has no read-back route; an unanswered one is sent again.
func TestSetImageRegistryUnansweredPut(t *testing.T) {
	noUnansweredDelay(t)
	d := &flakyDoer{inner: &registryDoer{}, fail: 1}
	if err := (&Client{T: d}).SetImageRegistry(context.Background(), "scionlocal"); err != nil || d.inner.calls != 1 || d.tries != 2 {
		t.Fatalf("err = %v after %d tries", err, d.tries)
	}
}

// flakyDoer fails its first fail requests with a transport error.
type flakyDoer struct {
	inner       *registryDoer
	fail, tries int
}

func (d *flakyDoer) Do(ctx context.Context, method, path string) (int, []byte, error) {
	return d.DoBody(ctx, method, path, nil)
}

func (d *flakyDoer) DoBody(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	d.tries++
	if d.tries <= d.fail {
		return 0, nil, errors.New("curl: (28) Operation timed out")
	}
	return d.inner.DoBody(ctx, method, path, body)
}

func TestRuntime(t *testing.T) {
	d := &runtimeDoer{current: "docker"}
	rt, err := (&Client{T: d}).Runtime(context.Background())
	if err != nil || rt != "docker" || d.bodies[0] != "GET /api/v1/system/runtime " {
		t.Fatalf("runtime %q, err %v, requests %q", rt, err, d.bodies)
	}
	if _, err := (&Client{T: noConfigured{}}).Runtime(context.Background()); err == nil {
		t.Fatal("want an error for an answer with no configured field")
	}
}

type noConfigured struct{}

func (noConfigured) Do(context.Context, string, string) (int, []byte, error) {
	return 200, []byte(`{"detected":"podman"}`), nil
}

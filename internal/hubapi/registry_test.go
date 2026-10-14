package hubapi

import (
	"context"
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

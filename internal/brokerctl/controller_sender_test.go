package brokerctl

import (
	"context"
	"net/http"
	"testing"

	"github.com/stevegeek/lever/internal/hubapi"
)

// meDoer answers GET /api/v1/auth/me with a fixed body.
type meDoer struct{ body string }

func (d meDoer) Do(_ context.Context, method, path string) (int, []byte, error) {
	if method != http.MethodGet || path != "/api/v1/auth/me" {
		return http.StatusNotFound, nil, nil
	}
	return http.StatusOK, []byte(d.body), nil
}

// TestControllerSenderFallsBackToTheUserID: the hub stamps "user:<email>",
// or "user:<id>" for a user with no email; the resolver gives the same
// label, never a bare "user:".
func TestControllerSenderFallsBackToTheUserID(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"id":"u-1","email":"dev@localhost"}`, "user:dev@localhost"},
		{`{"id":"u-1","email":""}`, "user:u-1"},
		{`{"id":"u-1"}`, "user:u-1"},
	} {
		got, err := controllerSenderFrom(&hubapi.Client{T: meDoer{tc.body}})(context.Background())
		if err != nil || got != tc.want {
			t.Fatalf("%s: got %q, %v; want %q", tc.body, got, err, tc.want)
		}
	}
}

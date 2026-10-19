package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/buildstamp"
	"github.com/stevegeek/lever/internal/httpjson"
	"github.com/stevegeek/lever/internal/jailabi"
	"github.com/stevegeek/lever/internal/wire"
)

func TestBuildLine(t *testing.T) {
	want := fmt.Sprintf("lever-agent %s, jail ABI %d", buildstamp.Describe("1.2.3"), jailabi.JailABI)
	if got := BuildLine("lever-agent", "1.2.3"); got != want {
		t.Fatalf("BuildLine = %q, want %q", got, want)
	}
}

func TestVersionReport(t *testing.T) {
	local := buildstamp.Describe("1.2.3")
	abi := jailabi.JailABI
	fix := "rebuild the agent image, then lever up --fresh --keep-conversation"
	notFound := fmt.Errorf("agent: host version: %w", &httpjson.StatusError{Code: http.StatusNotFound})
	for _, c := range []struct {
		name   string
		host   wire.VersionResponse
		err    error
		worker bool
		want   []string
		absent []string
	}{
		{"same build", wire.VersionResponse{Version: local, JailABI: abi}, nil, false,
			[]string{"host lever " + local + ", jail ABI ", "match: the image and the host are the same build"}, []string{"rebuild"}},
		{"same ABI other release", wire.VersionResponse{Version: "1.2.4", JailABI: abi}, nil, false,
			[]string{"host lever 1.2.4", "compatible:", "no rebuild is needed"}, []string{"mismatch"}},
		{"other ABI, manager", wire.VersionResponse{Version: "1.3.0", JailABI: abi + 1}, nil, false,
			[]string{fmt.Sprintf("mismatch: image %s (ABI %d), host 1.3.0 (ABI %d): %s", local, abi, abi+1, fix)}, []string{"ask the operator"}},
		{"other ABI, worker", wire.VersionResponse{Version: "1.1.0", JailABI: abi - 1}, nil, true,
			[]string{fmt.Sprintf("host 1.1.0 (ABI %d): ask the operator to %s", abi-1, fix)}, nil},
		{"host predates the route", wire.VersionResponse{}, notFound, false,
			[]string{"older than this image", "mismatch: image " + local, fix}, nil},
		{"empty host version", wire.VersionResponse{JailABI: abi}, nil, false,
			[]string{"host lever unknown", "compatible:"}, nil},
	} {
		got, err := VersionReport("1.2.3", c.host, c.err, c.worker)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: report %q lacks %q", c.name, got, w)
			}
		}
		for _, a := range c.absent {
			if strings.Contains(got, a) {
				t.Errorf("%s: report %q has %q", c.name, got, a)
			}
		}
	}
	for _, err := range []error{errors.New("dial: refused"), &httpjson.StatusError{Code: http.StatusForbidden}} {
		if _, got := VersionReport("1.2.3", wire.VersionResponse{}, err, false); !errors.Is(got, err) {
			t.Errorf("VersionReport(%v) error = %v, want it returned", err, got)
		}
	}
}

func TestHostVersionGetsTheRoute(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != wire.PathVersion {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"version":"1.2.3 (abc)","jail_abi":7}`))
	}))
	defer srv.Close()
	got, err := HostVersion(context.Background(), srv.URL, srv.Client())
	if err != nil || got != (wire.VersionResponse{Version: "1.2.3 (abc)", JailABI: 7}) {
		t.Fatalf("HostVersion = %+v, %v", got, err)
	}
}

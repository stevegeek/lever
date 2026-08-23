package httpjson

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serve(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestPostRoundTrip(t *testing.T) {
	var gotCT, gotMethod string
	var gotIn map[string]string
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotCT, gotMethod = r.Header.Get("Content-Type"), r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotIn)
		_, _ = io.WriteString(w, `{"token":"abc"}`)
	})
	var out struct {
		Token string `json:"token"`
	}
	if err := Post(context.Background(), srv.Client(), srv.URL+"/request", map[string]string{"tool": "db"}, &out); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotCT != "application/json" || gotIn["tool"] != "db" || out.Token != "abc" {
		t.Fatalf("method=%s ct=%s in=%v out=%+v", gotMethod, gotCT, gotIn, out)
	}
}

func TestGetRoundTripAndNilOut(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Content-Type") != "" {
			t.Errorf("unexpected request %s ct=%q", r.Method, r.Header.Get("Content-Type"))
		}
		_, _ = io.WriteString(w, `{"tools":["a","b"]}`)
	})
	var out struct {
		Tools []string `json:"tools"`
	}
	if err := Get(context.Background(), srv.Client(), srv.URL+"/tools", &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 2 {
		t.Fatalf("tools = %v", out.Tools)
	}
	if err := Get(context.Background(), srv.Client(), srv.URL+"/tools", nil); err != nil {
		t.Fatalf("nil out must discard the body: %v", err)
	}
}

func TestNon200IsStatusError(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "  policy: denied \n")
	})
	err := Post(context.Background(), srv.Client(), srv.URL+"/request", nil, nil)
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("want *StatusError, got %T %v", err, err)
	}
	if se.Code != 403 || se.Body != "policy: denied" || se.Method != "POST" || se.URL != srv.URL+"/request" {
		t.Fatalf("unexpected StatusError %+v", se)
	}
	want := "POST " + srv.URL + "/request: status 403: policy: denied"
	if err.Error() != want {
		t.Fatalf("got %q want %q", err.Error(), want)
	}
	if Status(err) != 403 || Status(errors.New("x")) != 0 {
		t.Fatalf("Status() mismatch: %d", Status(err))
	}
}

func TestNon200EmptyBodyOmitsColon(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	err := Get(context.Background(), srv.Client(), srv.URL+"/x", nil)
	if want := "GET " + srv.URL + "/x: status 404"; err == nil || err.Error() != want {
		t.Fatalf("got %v want %q", err, want)
	}
}

func TestBodyReadIsBounded(t *testing.T) {
	big := strings.Repeat("x", MaxBody+100)
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, big)
	})
	err := Get(context.Background(), srv.Client(), srv.URL, nil)
	var se *StatusError
	if !errors.As(err, &se) || len(se.Body) != MaxBody {
		t.Fatalf("error body must be capped at MaxBody, got %T len=%d", err, len(se.Body))
	}
}

func TestDecodeErrorIsPrefixed(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "not json") })
	var out struct{}
	err := Get(context.Background(), srv.Client(), srv.URL+"/t", &out)
	if err == nil || !strings.HasPrefix(err.Error(), "GET "+srv.URL+"/t: decode: ") {
		t.Fatalf("got %v", err)
	}
}

func TestTransportErrorIsPrefixed(t *testing.T) {
	srv := serve(t, func(http.ResponseWriter, *http.Request) {})
	url := srv.URL
	srv.Close()
	err := Post(context.Background(), srv.Client(), url+"/p", nil, nil)
	if err == nil || !strings.HasPrefix(err.Error(), "POST "+url+"/p: ") || Status(err) != 0 {
		t.Fatalf("got %v", err)
	}
}

package broker

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeBody(t *testing.T) {
	type body struct {
		Worker string `json:"worker"`
	}
	t.Run("decodes within limit", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/x", strings.NewReader(`{"worker":"w"}`))
		var v body
		if err := decodeBody(httptest.NewRecorder(), r, 64, &v); err != nil || v.Worker != "w" {
			t.Fatalf("decodeBody = %v, v = %+v", err, v)
		}
	})
	t.Run("rejects malformed json", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/x", strings.NewReader(`{`))
		var v body
		if err := decodeBody(httptest.NewRecorder(), r, 64, &v); err == nil {
			t.Fatal("malformed body decoded")
		}
	})
	t.Run("rejects a body over the limit", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/x", strings.NewReader(`{"worker":"`+strings.Repeat("a", 100)+`"}`))
		var v body
		if err := decodeBody(httptest.NewRecorder(), r, 16, &v); err == nil {
			t.Fatal("oversized body decoded")
		}
	})
}

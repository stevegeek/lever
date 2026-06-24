package broker

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRegisterAddsTool(t *testing.T) {
	b := New(testConfig(t))
	body, _ := json.Marshal(RegisterRequest{
		Name: "calendar", Backend: "http://127.0.0.1:3202",
		Operations: []OperationSpec{{Name: "list"}},
	})
	r := httptest.NewRequest("POST", "/register", bytes.NewReader(body))
	w := httptest.NewRecorder()
	b.handleRegister(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !b.reg.HasOperation("calendar", "list") {
		t.Fatal("calendar.list should be registered")
	}
}

func TestRegisterRejectsInvalid(t *testing.T) {
	b := New(testConfig(t))
	body, _ := json.Marshal(RegisterRequest{Name: "", Backend: "x"}) // empty name
	r := httptest.NewRequest("POST", "/register", bytes.NewReader(body))
	w := httptest.NewRecorder()
	b.handleRegister(w, r)
	if w.Code == http.StatusOK {
		t.Fatal("registration with empty name must be rejected")
	}
}

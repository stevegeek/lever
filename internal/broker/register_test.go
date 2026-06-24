package broker

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lever-to/lever/internal/cap/token"
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

func TestRegisterSetsFirstParty(t *testing.T) {
	b := New(testConfig(t))
	body, _ := json.Marshal(RegisterRequest{
		Name: "db", Backend: "http://127.0.0.1:3201", FirstParty: true,
		Operations: []OperationSpec{{Name: "read"}},
	})
	r := httptest.NewRequest("POST", "/register", bytes.NewReader(body))
	w := httptest.NewRecorder()
	b.handleRegister(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	tool, _ := b.reg.Lookup("db")
	if !tool.FirstParty {
		t.Fatal("handleRegister must set FirstParty on the registry tool")
	}
}

func TestRegisterResponseCarriesPubKeyAndEpoch(t *testing.T) {
	b := New(testConfig(t))
	b.BumpEpoch() // epoch now 1
	body, _ := json.Marshal(RegisterRequest{Name: "db", Backend: "http://127.0.0.1:3201", Operations: []OperationSpec{{Name: "read"}}})
	r := httptest.NewRequest("POST", "/register", bytes.NewReader(body))
	w := httptest.NewRecorder()
	b.handleRegister(w, r)
	var resp RegisterResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body=%s err=%v", w.Body.String(), err)
	}
	if resp.PublicKey == "" || resp.Epoch != 1 {
		t.Fatalf("resp = %+v", resp)
	}
	// The advertised key must be the broker's actual verification key.
	pub, err := token.DecodePublicKey(resp.PublicKey)
	if err != nil || string(pub) != string(b.keys.Public) {
		t.Fatalf("public key mismatch: err=%v", err)
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

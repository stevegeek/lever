package broker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBootstrapMintsManagerTicketOnce(t *testing.T) {
	b := New(testConfig(t)) // testConfig's ManagerIdentity is "manager"
	call := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		b.AdminHandler().ServeHTTP(w, httptest.NewRequest("POST", "/bootstrap", nil))
		return w
	}
	w1 := call()
	if w1.Code != http.StatusOK {
		t.Fatalf("first /bootstrap = %d", w1.Code)
	}
	var resp struct {
		Ticket string `json:"ticket"`
	}
	_ = json.Unmarshal(w1.Body.Bytes(), &resp)
	if resp.Ticket == "" {
		t.Fatal("no ticket returned")
	}
	if w2 := call(); w2.Code == http.StatusOK {
		t.Fatal("second /bootstrap must be refused (single-latch)")
	}
}

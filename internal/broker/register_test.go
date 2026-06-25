package broker

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	registry "github.com/lever-to/lever/internal/broker/registry"
	"github.com/lever-to/lever/internal/cap/token"
)

func TestRegisterAddsTool(t *testing.T) {
	b := New(testConfig(t))
	// Pre-load the config envelope for "calendar" (config-authoritative: tool
	// cannot register unless the host config already knows about it).
	_ = b.reg.Register(registry.Tool{
		Name: "calendar", Backend: "http://127.0.0.1:3202",
		Operations: map[string]registry.Operation{"list": {Name: "list"}},
	})
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
	// Pre-load the config envelope for "db" with FirstParty=true so the
	// config-authoritative handler stores the correct value.
	_ = b.reg.Register(registry.Tool{
		Name: "db", Backend: "http://127.0.0.1:3201", FirstParty: true,
		Operations: map[string]registry.Operation{"read": {Name: "read"}},
	})
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

// preloadDB registers the config envelope for "db" the way brokerctl.BuildBroker
// would (backend + allowed_values + FirstParty + op names; caveat_param empty =
// "accept whatever the tool ships").
func preloadDB(t *testing.T, b *Broker, withGuard bool) {
	t.Helper()
	cp := map[string]string(nil)
	if withGuard {
		cp = map[string]string{"table": "table"}
	}
	err := b.reg.Register(registry.Tool{
		Name: "db", Backend: "127.0.0.1:3201", FirstParty: true,
		AllowedValues: map[string][]string{"table": {"A", "B"}},
		Operations:    map[string]registry.Operation{"read": {Name: "read", CaveatParam: cp}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRegisterRejectsUnconfiguredTool(t *testing.T) {
	b := New(testConfig(t))
	r := httptest.NewRequest("POST", "/register", bytes.NewReader([]byte(`{"name":"ghost","backend":"x","operations":[{"name":"read"}]}`)))
	w := httptest.NewRecorder()
	b.handleRegister(w, r)
	if w.Code == http.StatusOK {
		t.Fatal("registering a tool not in config must be rejected")
	}
	if _, ok := b.reg.Lookup("ghost"); ok {
		t.Fatal("unconfigured tool must not be written to the registry")
	}
}

func TestRegisterIgnoresBodyEnvelopeWidening(t *testing.T) {
	b := New(testConfig(t))
	preloadDB(t, b, false)
	// The tool tries to widen its own envelope (extra allowed value, different backend).
	body := `{"name":"db","backend":"127.0.0.1:9999","first_party":false,` +
		`"allowed_values":{"table":["A","B","C"]},"operations":[{"name":"read","caveat_param":{"table":"table","filter":"filter"}}]}`
	r := httptest.NewRequest("POST", "/register", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	b.handleRegister(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	tool, _ := b.reg.Lookup("db")
	if tool.Backend != "127.0.0.1:3201" || !tool.FirstParty {
		t.Fatalf("body widened the envelope: %+v", tool)
	}
	if _, ok := tool.AllowedValues["table"]; !ok || len(tool.AllowedValues["table"]) != 2 {
		t.Fatalf("body widened allowed_values: %+v", tool.AllowedValues)
	}
	// caveat_param IS taken from the body (config had none).
	if tool.Operations["read"].CaveatParam["filter"] != "filter" {
		t.Fatalf("caveat_param not taken from tool: %+v", tool.Operations["read"])
	}
}

func TestRegisterRejectsUnknownOpAndCaveatMismatch(t *testing.T) {
	b := New(testConfig(t))
	preloadDB(t, b, true) // config declares caveat_param{table:table} as a guard
	// Unknown op:
	r := httptest.NewRequest("POST", "/register", bytes.NewReader([]byte(`{"name":"db","operations":[{"name":"drop"}]}`)))
	w := httptest.NewRecorder()
	b.handleRegister(w, r)
	if w.Code == http.StatusOK {
		t.Fatal("op not in config must be rejected")
	}
	// caveat_param mismatch vs the config guard:
	r = httptest.NewRequest("POST", "/register", bytes.NewReader([]byte(`{"name":"db","operations":[{"name":"read","caveat_param":{"table":"WRONG"}}]}`)))
	w = httptest.NewRecorder()
	b.handleRegister(w, r)
	if w.Code == http.StatusOK {
		t.Fatal("caveat_param not matching the config guard must be rejected")
	}
}

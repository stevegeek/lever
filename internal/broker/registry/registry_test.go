package registry

import "testing"

func dbTool() Tool {
	return Tool{
		Name:    "db",
		Backend: "http://127.0.0.1:3201",
		Operations: map[string]Operation{
			"read": {Name: "read", CaveatParam: map[string]string{"table": "schema.table"}},
		},
		AllowedValues: map[string][]string{"table": {"A", "B"}},
	}
}

func TestRegisterAndLookup(t *testing.T) {
	r := New()
	if err := r.Register(dbTool()); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, ok := r.Lookup("db")
	if !ok || got.Backend != "http://127.0.0.1:3201" {
		t.Fatalf("Lookup(db) = %+v, %v", got, ok)
	}
	if _, ok := r.Lookup("nope"); ok {
		t.Error("unexpected lookup for unknown tool")
	}
}

func TestRegisterReplacesByName(t *testing.T) {
	r := New()
	_ = r.Register(dbTool())
	updated := dbTool()
	updated.Backend = "http://127.0.0.1:9999"
	if err := r.Register(updated); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Lookup("db")
	if got.Backend != "http://127.0.0.1:9999" {
		t.Errorf("re-register should replace; backend = %q", got.Backend)
	}
}

func TestRegisterRejectsInvalid(t *testing.T) {
	r := New()
	if err := r.Register(Tool{Backend: "x", Operations: map[string]Operation{"read": {Name: "read"}}}); err == nil {
		t.Error("expected error: empty name")
	}
	if err := r.Register(Tool{Name: "db", Operations: map[string]Operation{"read": {Name: "read"}}}); err == nil {
		t.Error("expected error: empty backend")
	}
	if err := r.Register(Tool{Name: "db", Backend: "x"}); err == nil {
		t.Error("expected error: no operations")
	}
}

func TestHasOperation(t *testing.T) {
	r := New()
	_ = r.Register(dbTool())
	if !r.HasOperation("db", "read") {
		t.Error("db.read should be registered")
	}
	if r.HasOperation("db", "write") {
		t.Error("db.write was never registered")
	}
	if r.HasOperation("ghost", "read") {
		t.Error("unknown tool must report no operation")
	}
}

func TestMapParamsIdentityAndRename(t *testing.T) {
	r := New()
	_ = r.Register(dbTool()) // op read maps constraint "table" -> arg "schema.table"
	out, err := r.MapParams("db", "read", map[string]string{"schema.table": "A", "filter": "Y"})
	if err != nil {
		t.Fatal(err)
	}
	// Renamed: constraint key "table" gets the value of arg "schema.table".
	if out["table"] != "A" {
		t.Errorf(`out["table"] = %q, want "A" (renamed from schema.table)`, out["table"])
	}
	// Identity: "filter" passes through unchanged.
	if out["filter"] != "Y" {
		t.Errorf(`out["filter"] = %q, want "Y" (identity)`, out["filter"])
	}
}

func TestMapParamsMissingRenamedArgProducesNoBinding(t *testing.T) {
	r := New()
	_ = r.Register(dbTool())
	out, err := r.MapParams("db", "read", map[string]string{"filter": "Y"}) // no schema.table
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out["table"]; ok {
		t.Error(`out should have no "table" binding when the renamed arg is absent (a table constraint then fails closed at verify)`)
	}
}

func TestMapParamsUnknownToolOrOp(t *testing.T) {
	r := New()
	_ = r.Register(dbTool())
	if _, err := r.MapParams("ghost", "read", nil); err == nil {
		t.Error("expected error for unknown tool")
	}
	if _, err := r.MapParams("db", "write", nil); err == nil {
		t.Error("expected error for unknown operation")
	}
}

func TestValidateConstraintsAllowsPermittedValue(t *testing.T) {
	r := New()
	_ = r.Register(dbTool()) // table ∈ {A,B}
	if err := r.ValidateConstraints("db", map[string]string{"table": "A"}); err != nil {
		t.Fatalf("table=A is permitted: %v", err)
	}
}

func TestValidateConstraintsRejectsForbiddenValue(t *testing.T) {
	r := New()
	_ = r.Register(dbTool())
	if err := r.ValidateConstraints("db", map[string]string{"table": "C"}); err == nil {
		t.Fatal("table=C must be rejected (not in {A,B})")
	}
}

func TestValidateConstraintsUnrestrictedKeyPasses(t *testing.T) {
	r := New()
	_ = r.Register(dbTool()) // "filter" has no AllowedValues entry
	if err := r.ValidateConstraints("db", map[string]string{"filter": "anything"}); err != nil {
		t.Fatalf("unrestricted key should pass: %v", err)
	}
}

func TestValidateConstraintsUnknownTool(t *testing.T) {
	r := New()
	if err := r.ValidateConstraints("ghost", map[string]string{"table": "A"}); err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestValidateConstraintsEmptyAllowedSliceRejectsAll(t *testing.T) {
	r := New()
	tool := dbTool()
	tool.AllowedValues = map[string][]string{"table": {}} // restricted to nothing
	_ = r.Register(tool)
	if err := r.ValidateConstraints("db", map[string]string{"table": "A"}); err == nil {
		t.Fatal("an empty AllowedValues slice must reject every value (fail-closed), not pass")
	}
}

func TestNamesReturnsBothRegisteredTools(t *testing.T) {
	r := New()
	if err := r.Register(dbTool()); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(Tool{
		Name: "calendar", Backend: "http://127.0.0.1:3202",
		Operations: map[string]Operation{"list": {Name: "list"}},
	}); err != nil {
		t.Fatal(err)
	}
	names := r.Names()
	if len(names) != 2 {
		t.Fatalf("Names() returned %d names, want 2: %v", len(names), names)
	}
	found := map[string]bool{}
	for _, n := range names {
		found[n] = true
	}
	if !found["db"] {
		t.Error("Names() missing \"db\"")
	}
	if !found["calendar"] {
		t.Error("Names() missing \"calendar\"")
	}
}

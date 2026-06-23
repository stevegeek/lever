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

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
	if err := r.ValidateConstraints("db", "read", map[string]string{"table": "A"}); err != nil {
		t.Fatalf("table=A is permitted: %v", err)
	}
}

func TestValidateConstraintsRejectsForbiddenValue(t *testing.T) {
	r := New()
	_ = r.Register(dbTool())
	if err := r.ValidateConstraints("db", "read", map[string]string{"table": "C"}); err == nil {
		t.Fatal("table=C must be rejected (not in {A,B})")
	}
}

func TestValidateConstraintsUnrestrictedKeyPasses(t *testing.T) {
	r := New()
	_ = r.Register(dbTool()) // "filter" has no AllowedValues entry
	if err := r.ValidateConstraints("db", "read", map[string]string{"filter": "anything"}); err != nil {
		t.Fatalf("unrestricted key should pass: %v", err)
	}
}

func TestValidateConstraintsUnknownTool(t *testing.T) {
	r := New()
	if err := r.ValidateConstraints("ghost", "read", map[string]string{"table": "A"}); err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestValidateConstraintsEmptyAllowedSliceRejectsAll(t *testing.T) {
	r := New()
	tool := dbTool()
	tool.AllowedValues = map[string][]string{"table": {}} // restricted to nothing
	_ = r.Register(tool)
	if err := r.ValidateConstraints("db", "read", map[string]string{"table": "A"}); err == nil {
		t.Fatal("an empty AllowedValues slice must reject every value (fail-closed), not pass")
	}
}

// Declared params (#21): when an op declares its parameter set, a constraint
// key outside Params ∪ CaveatParam keys is rejected AT MINT — a typo'd key
// would otherwise mint an over-narrowed token that fails closed only at call
// time, far from the mistake. Undeclared ops stay permissive.
func TestValidateConstraintsDeclaredParams(t *testing.T) {
	r := New()
	tool := dbTool()
	op := tool.Operations["read"]
	op.Params = []string{"query", "schema.table"}
	tool.Operations["read"] = op
	_ = r.Register(tool)

	if err := r.ValidateConstraints("db", "read", map[string]string{"query": "x"}); err != nil {
		t.Fatalf("declared param must be accepted: %v", err)
	}
	if err := r.ValidateConstraints("db", "read", map[string]string{"table": "A"}); err != nil {
		t.Fatalf("caveat_param constraint key must be accepted: %v", err)
	}
	if err := r.ValidateConstraints("db", "read", map[string]string{"tabel": "A"}); err == nil {
		t.Fatal("a typo'd constraint key must be rejected at mint when params are declared")
	}
	if err := r.ValidateConstraints("db", "read", map[string]string{"agent": "worker"}); err == nil {
		t.Fatal("a stray reserved-ish key must be rejected at mint when params are declared")
	}

	// Undeclared op (no Params) keeps today's permissive behavior.
	perm := dbTool()
	perm.Name = "db2"
	_ = r.Register(perm)
	if err := r.ValidateConstraints("db2", "read", map[string]string{"anything": "goes"}); err != nil {
		t.Fatalf("undeclared params must stay permissive: %v", err)
	}
}

func TestRegisterPreservesFirstParty(t *testing.T) {
	r := New()
	if err := r.Register(Tool{
		Name: "db", Backend: "http://127.0.0.1:3201", FirstParty: true,
		Operations: map[string]Operation{"read": {Name: "read"}},
	}); err != nil {
		t.Fatal(err)
	}
	tool, ok := r.Lookup("db")
	if !ok || !tool.FirstParty {
		t.Fatalf("FirstParty not preserved: ok=%v tool=%+v", ok, tool)
	}
}

func TestRegisterRoundTripsExternalCoarse(t *testing.T) {
	r := New()
	err := r.Register(Tool{
		Name: "things3", Backend: "127.0.0.1:3300", External: true, Coarse: true,
		Operations: map[string]Operation{WildcardOp: {Name: WildcardOp}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := r.Lookup("things3")
	if !ok || !got.External || !got.Coarse || got.FirstParty {
		t.Fatalf("lookup = %+v ok=%v; want External+Coarse, not FirstParty", got, ok)
	}
	if !r.HasOperation("things3", WildcardOp) {
		t.Fatal("coarse tool must expose the wildcard op (mint path relies on HasOperation)")
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

func TestCloneAllowedValues(t *testing.T) {
	if got := CloneAllowedValues(nil); got != nil {
		t.Fatalf("CloneAllowedValues(nil) = %v, want nil", got)
	}
	src := map[string][]string{"table": {"A", "B"}, "db": nil}
	got := CloneAllowedValues(src)
	if len(got) != 2 || got["table"][0] != "A" || got["table"][1] != "B" {
		t.Fatalf("CloneAllowedValues = %v, want copy of %v", got, src)
	}
	// Deep copy: mutating the source slice must not affect the clone.
	src["table"][0] = "MUTATED"
	if got["table"][0] == "MUTATED" {
		t.Fatal("clone aliased the source slice — must deep-copy")
	}
	// And mutating the clone's map must not affect the source.
	got["extra"] = []string{"x"}
	if _, ok := src["extra"]; ok {
		t.Fatal("clone aliased the source map")
	}
}

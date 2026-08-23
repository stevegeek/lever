package wire

import (
	"encoding/json"
	"testing"
)

// TestTypesKeepTheirWireShape pins every wire type to the JSON each side used
// to build by hand (maps, anonymous structs, per-package copies) before the
// types were declared here. A changed tag would break a peer binary that was
// built before the change, so the literal shapes are the contract.
func TestTypesKeepTheirWireShape(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want string
	}{
		{"EnrolRequest", EnrolRequest{Ticket: "t", CSR: "c"}, `{"ticket":"t","csr":"c"}`},
		{"EnrolResponse", EnrolResponse{Cert: "pem"}, `{"cert":"pem"}`},
		{"RenewRequest", RenewRequest{CSR: "c"}, `{"csr":"c"}`},
		{"RenewResponse", RenewResponse{Cert: "pem"}, `{"cert":"pem"}`},
		{"ProvisionRequest", ProvisionRequest{Worker: "w"}, `{"worker":"w"}`},
		{"ProvisionResponse", ProvisionResponse{Ticket: "t"}, `{"ticket":"t"}`},
		{"CapRequest", CapRequest{Tool: "db", Op: "read", BoundTo: "a", Constraints: map[string]string{"k": "v"}},
			`{"tool":"db","op":"read","bound_to":"a","constraints":{"k":"v"}}`},
		{"CapRequest/no constraints", CapRequest{Tool: "db", Op: "read", BoundTo: "a"}, `{"tool":"db","op":"read","bound_to":"a"}`},
		{"CapResponse", CapResponse{Token: "tok"}, `{"token":"tok"}`},
		{"ToolsResponse", ToolsResponse{Tools: []string{"a", "b"}}, `{"tools":["a","b"]}`},
		{"WorkerStartRequest", WorkerStartRequest{Worker: "w", Task: "go"}, `{"worker":"w","task":"go"}`},
		{"WorkerRequest", WorkerRequest{Worker: "w"}, `{"worker":"w"}`},
		{"WorkerResponse", WorkerResponse{Worker: "w", Phase: "running"}, `{"worker":"w","phase":"running"}`},
		{"MsgSendRequest", MsgSendRequest{To: "a", Body: "hi", Interrupt: true}, `{"to":"a","body":"hi","interrupt":true}`},
		{"MsgSendResponse", MsgSendResponse{OK: true}, `{"ok":true}`},
		{"MsgListRequest", MsgListRequest{All: true, Worker: "w"}, `{"all":true,"worker":"w"}`},
		{"DirectiveIDRequest", DirectiveIDRequest{ID: "d"}, `{"id":"d"}`},
		{"DirectiveConsumeResponse/instruction", DirectiveConsumeResponse{ID: "d", Kind: "instruction", AdvisoryText: "x", Note: "n"},
			`{"id":"d","kind":"instruction","advisory_text":"x","note":"n"}`},
		{"DirectiveConsumeResponse/bound", DirectiveConsumeResponse{ID: "d", Kind: "tool_call", Action: map[string]string{"kind": "tool_call"}},
			`{"id":"d","kind":"tool_call","action":{"kind":"tool_call"}}`},
		{"DirectiveCheckResponse", DirectiveCheckResponse{ID: "d", State: "active"}, `{"id":"d","state":"active"}`},
		{"DirectiveSubmitRequest", DirectiveSubmitRequest{Statement: "s", Signature: "g"}, `{"statement":"s","signature":"g"}`},
		{"DirectiveEnvelopeRequest", DirectiveEnvelopeRequest{Envelope: "e", Signature: "g"}, `{"envelope":"e","signature":"g"}`},
		{"DirectiveSendResponse", DirectiveSendResponse{ID: "d", Delivered: true}, `{"id":"d","delivered":true}`},
		{"DirectiveResolveResponse", DirectiveResolveResponse{CN: "c", Slug: "s", Generation: 2}, `{"cn":"c","slug":"s","generation":2}`},
		{"DirectiveListResponse", DirectiveListResponse[json.RawMessage]{Directives: []json.RawMessage{json.RawMessage(`{"id":"a"}`)}},
			`{"directives":[{"id":"a"}]}`},
		{"DirectiveRevokeResponse", DirectiveRevokeResponse{Revoked: true}, `{"revoked":true}`},
		{"DirectiveSelftestResponse", DirectiveSelftestResponse{OK: true}, `{"ok":true}`},
		{"ErrorResponse", ErrorResponse{Error: "not found"}, `{"error":"not found"}`},
		{"OperationSpec", OperationSpec{Name: "read", CaveatParam: map[string]string{"p": "q"}}, `{"name":"read","caveat_param":{"p":"q"}}`},
		{"RegisterRequest", RegisterRequest{Name: "db", Backend: "sqlite", Operations: []OperationSpec{{Name: "read"}},
			AllowedValues: map[string][]string{"k": {"v"}}, FirstParty: true},
			`{"name":"db","backend":"sqlite","operations":[{"name":"read"}],"allowed_values":{"k":["v"]},"first_party":true}`},
		{"RegisterResponse", RegisterResponse{PublicKey: "pk", Epoch: 3}, `{"public_key":"pk","epoch":3}`},
		{"EpochResponse", EpochResponse{Epoch: 1, Version: "v", ConfigHash: "h"}, `{"epoch":1,"version":"v","config_hash":"h"}`},
		{"EpochResponse/old broker", EpochResponse{Epoch: 1}, `{"epoch":1}`},
		{"RevokeRequest", RevokeRequest{Agent: "a"}, `{"agent":"a"}`},
		{"BootstrapResponse", BootstrapResponse{Ticket: "t"}, `{"ticket":"t"}`},
		{"Bootstrap", Bootstrap{Ticket: "t", BrokerCA: "ca", BrokerURL: "u", AgentCN: "cn"},
			`{"ticket":"t","broker_ca":"ca","broker_url":"u","agent_cn":"cn"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.v)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestListResponsesJSONTags(t *testing.T) {
	got, err := json.Marshal(WorkerListResponse[string]{Agents: []string{"a"}})
	if err != nil || string(got) != `{"agents":["a"]}` {
		t.Fatalf("WorkerListResponse = %s, %v", got, err)
	}
	got, err = json.Marshal(MsgListResponse[int]{Events: []int{1}})
	if err != nil || string(got) != `{"events":[1]}` {
		t.Fatalf("MsgListResponse = %s, %v", got, err)
	}
	var empty MsgListResponse[int]
	if err := json.Unmarshal([]byte(`{}`), &empty); err != nil || empty.Events != nil {
		t.Fatalf("absent events = %v, %v", empty.Events, err)
	}
}

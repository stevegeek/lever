package hubapi

import (
	"context"
	"strings"
	"testing"
)

func TestProjectProviders(t *testing.T) {
	const path = "GET /api/v1/projects/" + leverUUID + "/providers"
	for _, tc := range []struct {
		name    string
		r       reply
		want    int
		wantErr string
	}{
		{"one", reply{status: 200, body: `{"providers":[{"projectId":"p","brokerId":"b1","brokerName":"host","status":"online"}]}`}, 1, ""},
		{"none", reply{status: 200, body: `{"providers":[]}`}, 0, ""},
		{"no list", reply{status: 200, body: `{}`}, 0, "changed shape"},
		{"refused", reply{status: 403, body: `{"error":{"code":"forbidden"}}`}, 0, "HTTP 403"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{T: &fakeDoer{replies: map[string]reply{path: tc.r}}}
			got, err := c.ProjectProviders(context.Background(), leverUUID)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || len(got) != tc.want {
				t.Fatalf("got %v, %v; want %d providers", got, err, tc.want)
			}
			if tc.want == 1 && got[0].BrokerID != "b1" {
				t.Fatalf("broker id = %q", got[0].BrokerID)
			}
		})
	}
}

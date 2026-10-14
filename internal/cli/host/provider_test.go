package host

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/hubapi"
)

// providerHub answers the project list, its providers, the broker list and the
// provider link. admin marks the client built over it as lever-admin: only
// that one may link, as on scion.
type providerHub struct {
	listed    bool
	providers []string
	brokers   []string
	linkErr   int
	// noEffect answers the link without linking, as a hub that drops it would.
	noEffect bool
	links    []string
}

type providerCaller struct {
	h     *providerHub
	admin bool
}

func (c providerCaller) Do(ctx context.Context, method, path string) (int, []byte, error) {
	return c.DoBody(ctx, method, path, nil)
}

func (c providerCaller) DoBody(_ context.Context, method, path string, body []byte) (int, []byte, error) {
	h := c.h
	switch {
	case method == "GET" && path == "/api/v1/projects":
		if !h.listed {
			return 200, []byte(`{"projects":[]}`), nil
		}
		return 200, []byte(`{"projects":[{"id":"p1","name":"lever","slug":"lever"}]}`), nil
	case method == "GET" && path == "/api/v1/projects/p1/providers":
		var ps []map[string]string
		for _, id := range h.providers {
			ps = append(ps, map[string]string{"brokerId": id, "brokerName": "jail"})
		}
		b, _ := json.Marshal(map[string]any{"providers": append([]map[string]string{}, ps...)})
		return 200, b, nil
	case method == "GET" && path == "/api/v1/runtime-brokers" && c.admin:
		var bs []map[string]string
		for _, id := range h.brokers {
			bs = append(bs, map[string]string{"id": id, "name": "jail", "status": "online"})
		}
		b, _ := json.Marshal(map[string]any{"brokers": append([]map[string]string{}, bs...)})
		return 200, b, nil
	case method == "POST" && path == "/api/v1/projects/p1/providers":
		if !c.admin {
			return 403, []byte(`{"error":{"message":"only the broker's owner or a super-admin may associate this broker with a project"}}`), nil
		}
		if h.linkErr != 0 {
			return h.linkErr, []byte(`{}`), nil
		}
		var req struct {
			BrokerID  string `json:"brokerId"`
			LocalPath string `json:"localPath"`
		}
		_ = json.Unmarshal(body, &req)
		h.links = append(h.links, req.BrokerID+" "+req.LocalPath)
		if !h.noEffect {
			h.providers = append(h.providers, req.BrokerID)
		}
		b, _ := json.Marshal(map[string]any{"provider": map[string]string{"brokerId": req.BrokerID}})
		return 201, b, nil
	}
	return 404, nil, nil
}

func TestEnsureProjectProvider(t *testing.T) {
	for _, tc := range []struct {
		name      string
		hub       providerHub
		wantLinks []string
		wantErr   string
	}{
		{name: "linked already: nothing to do", hub: providerHub{listed: true, providers: []string{"b1"}, brokers: []string{"b1"}}},
		{name: "unlisted project left alone", hub: providerHub{brokers: []string{"b1"}}},
		{name: "none: lever-admin links the jail's broker at the tree's path",
			hub: providerHub{listed: true, brokers: []string{"b1"}}, wantLinks: []string{"b1 /lever"}},
		{name: "no broker registered", hub: providerHub{listed: true}, wantErr: "lists 0 brokers"},
		{name: "two brokers: refused, not guessed", hub: providerHub{listed: true, brokers: []string{"b1", "b2"}}, wantErr: "lists 2 brokers"},
		{name: "link refused", hub: providerHub{listed: true, brokers: []string{"b1"}, linkErr: 403}, wantErr: "HTTP 403"},
		{name: "link without effect", hub: providerHub{listed: true, brokers: []string{"b1"}, noEffect: true},
			wantLinks: []string{"b1 /lever"}, wantErr: "after lever-admin linked broker b1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.hub
			err := ensureProjectProvider(context.Background(), providerLink{
				live:  &hubapi.Client{T: providerCaller{h: &h}},
				admin: &hubapi.Client{T: providerCaller{h: &h, admin: true}},
				jp:    "/lever",
				log:   func(string, ...any) {},
			})
			if strings.Join(h.links, ";") != strings.Join(tc.wantLinks, ";") {
				t.Fatalf("links = %q, want %q", h.links, tc.wantLinks)
			}
			if tc.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

package host

import (
	"context"
	"errors"
	"strings"
	"testing"

	scionpkg "github.com/stevegeek/lever/internal/scion"
)

func TestCheckOrphanRecords(t *testing.T) {
	list := func(agents ...scionpkg.Agent) agentLister {
		return func(context.Context, string) ([]scionpkg.Agent, error) { return agents, nil }
	}
	for _, tc := range []struct {
		name       string
		list       agentLister
		warn       bool
		wantDetail []string
	}{
		{"none", list(scionpkg.Agent{Slug: "assistant"}, scionpkg.Agent{Slug: "scratch"}), false, []string{"none"}},
		{"no list", nil, false, []string{"not checked"}},
		{"list fails", func(context.Context, string) ([]scionpkg.Agent, error) { return nil, errors.New("hub down\nmore") }, false, []string{"not checked", "hub down"}},
		{"orphans", list(scionpkg.Agent{Slug: "assistant"}, scionpkg.Agent{Slug: "zed", Phase: "stopped"}, scionpkg.Agent{Slug: "old", Phase: "running"}), true,
			[]string{"old (running), zed (stopped)", "max_running_workers"}},
		{"an unnamed one", list(scionpkg.Agent{Slug: "Bad\x1b[2J"}), true, []string{"1 hub record(s)", "whose name is not an agent name"}},
		{"named and unnamed", list(scionpkg.Agent{Slug: "old"}, scionpkg.Agent{Slug: "Bad"}), true, []string{"old (", "and 1 whose name is not an agent name"}},
	} {
		r := checkOrphanRecords(context.Background(), "/lever", "assistant", []string{"scratch"}, tc.list)
		if !r.ok || (r.fix != "") != tc.warn {
			t.Errorf("%s: ok=%v fix=%q, want a warning=%v", tc.name, r.ok, r.fix, tc.warn)
		}
		for _, w := range tc.wantDetail {
			if !strings.Contains(r.detail, w) {
				t.Errorf("%s: detail %q lacks %q", tc.name, r.detail, w)
			}
		}
		if strings.Contains(r.detail, "\x1b") {
			t.Errorf("%s: hub text in the detail: %q", tc.name, r.detail)
		}
		if tc.warn && !strings.Contains(r.fix, "lever worker purge NAME --orphan --force") {
			t.Errorf("%s: fix %q", tc.name, r.fix)
		}
	}
}

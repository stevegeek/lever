package broker

import "testing"

func msgBroker(g2g bool) *Broker {
	b := New(Config{ManagerIdentity: "assistant", GroveToGrove: g2g, ManagerProject: "/lever",
		Groves: []GroveSpec{{Name: "scratch", JailProject: "/lever/groves/scratch"},
			{Name: "worker", JailProject: "/lever/groves/worker"}}})
	return b
}

func TestResolveMsgTarget(t *testing.T) {
	cases := []struct {
		name, caller, to string
		g2g              bool
		wantTo, wantProj string
		wantErr          bool
	}{
		{"manager to grove bare", "assistant", "scratch", true, "agent:scratch", "/lever/groves/scratch", false},
		{"manager to grove prefixed", "assistant", "agent:scratch", true, "agent:scratch", "/lever/groves/scratch", false},
		{"manager to user", "assistant", "user:stephen", true, "user:stephen", "", false},
		{"manager to unknown grove", "assistant", "nope", true, "", "", true},
		{"grove to manager agent", "scratch", "agent:assistant", true, "agent:assistant", "/lever", false},
		{"grove to user", "scratch", "user:manager", true, "user:manager", "", false},
		{"grove to grove allowed", "scratch", "worker", true, "agent:worker", "/lever/groves/worker", false},
		{"grove to grove disabled", "scratch", "worker", false, "", "", true},
		{"grove to itself", "scratch", "scratch", true, "agent:scratch", "/lever/groves/scratch", false},
		{"unknown caller", "mallory", "assistant", true, "", "", true},
		{"grove to unknown", "scratch", "nope", true, "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tgt, err := msgBroker(c.g2g).resolveMsgTarget(c.caller, c.to)
			if c.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if err == nil && (tgt.scionTo != c.wantTo || tgt.project != c.wantProj) {
				t.Fatalf("got (%q,%q), want (%q,%q)", tgt.scionTo, tgt.project, c.wantTo, c.wantProj)
			}
		})
	}
}

func TestResolveListProject(t *testing.T) {
	cases := []struct {
		name, caller, grove string
		want                string
		wantErr             bool
	}{
		{"manager own inbox", "assistant", "", "", false},
		{"manager reads grove", "assistant", "scratch", "/lever/groves/scratch", false},
		{"manager unknown grove", "assistant", "nope", "", true},
		{"grove own inbox", "scratch", "", "/lever/groves/scratch", false},
		{"grove may not target others", "scratch", "worker", "", true},
		{"unknown caller", "mallory", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := msgBroker(true).resolveListProject(c.caller, c.grove)
			if c.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if err == nil && got != c.want {
				t.Fatalf("project = %q, want %q", got, c.want)
			}
		})
	}
}

package lima

import (
	"context"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/backendtest"
	"github.com/stevegeek/lever/internal/backend/common"
	"github.com/stevegeek/lever/internal/backend/guest"
	"github.com/stevegeek/lever/internal/egress"
	"github.com/stevegeek/lever/internal/proc"
)

var warmPorts = []int{3305}

// warmVM is a running VM whose guest answers the warm-path probe with items,
// everything else scripted as for the full path.
func warmVM(items map[string]backendtest.ProbeItem) *backendtest.ProbeRunner {
	r := &backendtest.ProbeRunner{FakeRunner: proc.NewFakeRunner(), Host: "limactl", Items: items}
	scriptedVM(r.FakeRunner)
	return r
}

// convergedItems is the probe of a converged, open-posture guest whose
// chain accepts the LIMADNS targets.
func convergedItems() map[string]backendtest.ProbeItem {
	fw := egress.ParseDNATTargets(backendtest.LimaDNSChain, backendtest.HostAliasV4)
	return backendtest.WarmItems(guest.ProvisionStampKey("leveruser"), warmPorts, fw, backendtest.LimaDNSChain)
}

func warmUp(t *testing.T, r *backendtest.ProbeRunner, cfg backend.Config) *Lima {
	t.Helper()
	cfg.MachineName, cfg.ProjectTree = vm, tree
	if cfg.AllowedPorts == nil {
		cfg.AllowedPorts = warmPorts
	}
	l := New(r, vm, common.Options{})
	if err := l.EnsureUp(context.Background(), cfg); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	return l
}

// guestCalls counts the `limactl shell` round trips.
func guestCalls(f *proc.FakeRunner) int {
	n := 0
	for _, c := range f.Calls {
		if c.Name == "limactl" && len(c.Args) > 0 && c.Args[0] == "shell" {
			n++
		}
	}
	return n
}

// TestEnsureUpWarmPathLeavesConvergedGuest: with the stamp matching and the
// chains exactly right, EnsureUp makes the two hardening calls and the two
// probes, and nothing else in the guest: no runtime scripts, no alias lookup,
// no commit, no nested-virt scripts.
func TestEnsureUpWarmPathLeavesConvergedGuest(t *testing.T) {
	r := warmVM(convergedItems())
	l := warmUp(t, r, backend.Config{})
	if r.Probes != 2 {
		t.Fatalf("probes = %d, want 2", r.Probes)
	}
	if bins, _ := backendtest.RestoreCommits(r.FakeRunner); len(bins) != 0 {
		t.Fatalf("a verified chain was committed again: %v", bins)
	}
	for _, c := range r.Calls {
		argv := c.Argv()
		for _, bad := range []string{"bash -lc", " whoami", " getent ", " uname ", "iptables -N", "iptables -C", "lever-stamp"} {
			if strings.Contains(argv, bad) && !strings.Contains(argv, "lever-probe") {
				t.Errorf("warm path ran %q: %s", strings.TrimSpace(bad), argv)
			}
		}
	}
	if n := guestCalls(r.FakeRunner); n != 4 {
		t.Errorf("guest round trips = %d, want 4 (2 hardening + 2 probes)", n)
	}
	if l.HostAliasV4() != backendtest.HostAliasV4 || l.RunUser() != "leveruser" || l.RunUID() != "501" {
		t.Errorf("alias %q user %q uid %q", l.HostAliasV4(), l.RunUser(), l.RunUID())
	}
}

// The full path, for the comparison the warm path exists for.
func TestEnsureUpFullPathRoundTrips(t *testing.T) {
	r := warmVM(map[string]backendtest.ProbeItem{}) // a probe that reads nothing usable
	warmUp(t, r, backend.Config{})
	n := guestCalls(r.FakeRunner)
	t.Logf("full path: %d guest round trips (warm path: 4)", n)
	if n < 15 {
		t.Errorf("full path guest round trips = %d; expected the per-check calls", n)
	}
}

// Every way the live egress state can differ from the ruleset sends the
// bring-up down the full path, which commits both families.
func TestEnsureUpWarmPathRebuildsAnyEgressDifference(t *testing.T) {
	cases := map[string]func(map[string]backendtest.ProbeItem){
		"no v4 chain": func(m map[string]backendtest.ProbeItem) { m["chain4"] = backendtest.ProbeItem{Code: 1} },
		"no v6 chain": func(m map[string]backendtest.ProbeItem) { delete(m, "chain6") },
		"no v4 jump":  func(m map[string]backendtest.ProbeItem) { m["out4"] = backendtest.ProbeItem{Out: "-P OUTPUT ACCEPT\n"} },
		"no v6 jump":  func(m map[string]backendtest.ProbeItem) { m["out6"] = backendtest.ProbeItem{Out: "-P OUTPUT ACCEPT\n"} },
		"rule added": func(m map[string]backendtest.ProbeItem) {
			m["chain4"] = backendtest.ProbeItem{Out: m["chain4"].Out + "-A LEVER_EGRESS -j ACCEPT\n"}
		},
		"accept first": func(m map[string]backendtest.ProbeItem) {
			m["chain4"] = backendtest.ProbeItem{Out: strings.Replace(m["chain4"].Out, "-N LEVER_EGRESS\n", "-N LEVER_EGRESS\n-A LEVER_EGRESS -j ACCEPT\n", 1)}
		},
		"rule removed": func(m map[string]backendtest.ProbeItem) {
			m["chain6"] = backendtest.ProbeItem{Out: strings.Replace(m["chain6"].Out, "-A LEVER_EGRESS -d fc00::/7 -j DROP\n", "", 1)}
		},
		"alias moved": func(m map[string]backendtest.ProbeItem) {
			m["ahosts"] = backendtest.ProbeItem{Out: "0.250.250.9 STREAM \nfd07::fe STREAM \n"}
		},
		"lookup failed": func(m map[string]backendtest.ProbeItem) { m["ahosts"] = backendtest.ProbeItem{Code: 2} },
		"dns port moved": func(m map[string]backendtest.ProbeItem) {
			m["dnschain"] = backendtest.ProbeItem{Out: strings.Replace(backendtest.LimaDNSChain, ":41234", ":41299", 1)}
		},
		"stamp other boot": func(m map[string]backendtest.ProbeItem) { m["boot"] = backendtest.ProbeItem{Out: "another-boot\n42\n"} },
		"stamp other key": func(m map[string]backendtest.ProbeItem) {
			m["stamp"] = backendtest.ProbeItem{Out: "0123 4c1d7a64-0b1e-4c55-9d2e-d0e1f2a3b4c5:42\n"}
		},
		"stamp missing":     func(m map[string]backendtest.ProbeItem) { delete(m, "stamp") },
		"probe has no user": func(m map[string]backendtest.ProbeItem) { delete(m, "user") },
		"pasta drop-in off": func(m map[string]backendtest.ProbeItem) { m["pasta"] = backendtest.ProbeItem{Out: "[network]\n"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			items := convergedItems()
			mutate(items)
			r := warmVM(items)
			warmUp(t, r, backend.Config{})
			backendtest.AssertAtomicCommitOrder(t, r.FakeRunner)
		})
	}
}

// A stamp that does not match provisions in full and stamps the guest with
// the key of the scripts it ran.
func TestEnsureUpStampMismatchProvisionsAndStamps(t *testing.T) {
	items := convergedItems()
	items["boot"] = backendtest.ProbeItem{Out: "rebooted\n7\n"}
	r := warmVM(items)
	warmUp(t, r, backend.Config{})
	if !r.Called(proc.ArgvContains("bash -lc", "dpkg -s")) {
		t.Fatal("a stamp from another boot must provision the runtimes")
	}
	if len(r.Stamps) != 1 || r.Stamps[0] != guest.ProvisionStampKey("leveruser") {
		t.Fatalf("stamp writes = %v, want the key for leveruser", r.Stamps)
	}
}

// The closed posture is verified against the aliases its chains name, with
// no lookup: a closed chain blocks DNS.
func TestEnsureUpWarmPathClosedPosture(t *testing.T) {
	ports := []int{8443}
	items := backendtest.WarmItems(guest.ProvisionStampKey("leveruser"), ports, nil, "")
	delete(items, "ahosts")
	items["chain4"] = backendtest.ProbeItem{Out: backendtest.ClosedChain}
	items["chain6"] = backendtest.ProbeItem{Out: backendtest.ClosedChain6}
	r := warmVM(items)
	l := warmUp(t, r, backend.Config{AllowedPorts: ports, ClosedInternet: true})
	if bins, _ := backendtest.RestoreCommits(r.FakeRunner); len(bins) != 0 {
		t.Fatalf("a verified closed chain was committed again: %v", bins)
	}
	if l.HostAliasV4() != backendtest.HostAliasV4 {
		t.Fatalf("alias = %q", l.HostAliasV4())
	}
	for _, c := range r.Calls {
		if strings.Contains(c.Argv(), "getent") || strings.Contains(c.Argv(), "LIMADNS") {
			t.Fatalf("the closed posture must not resolve or read the resolver: %s", c.Argv())
		}
	}

	// A port added to the config is a difference: rebuilt.
	r = warmVM(items)
	warmUp(t, r, backend.Config{AllowedPorts: []int{8443, 9000}, ClosedInternet: true})
	backendtest.AssertAtomicCommitOrder(t, r.FakeRunner)

	// An open chain under a closed config: rebuilt.
	r = warmVM(convergedItems())
	warmUp(t, r, backend.Config{AllowedPorts: warmPorts, ClosedInternet: true})
	backendtest.AssertAtomicCommitOrder(t, r.FakeRunner)
}

func TestEnsureUpWarmPathNestedVirt(t *testing.T) {
	old, oldArch := readHostFile, hostArch
	readHostFile = func(string) ([]byte, error) { return []byte("1\n"), nil }
	hostArch = "amd64"
	t.Cleanup(func() { readHostFile, hostArch = old, oldArch })
	on := func(m map[string]backendtest.ProbeItem) {
		m["kvmchar"] = backendtest.ProbeItem{}
		m["kvmmode"] = backendtest.ProbeItem{Out: "666\n"}
		m["udev"] = backendtest.ProbeItem{Out: `KERNEL=="kvm", MODE="0666"` + "\n"}
		m["udevexists"] = backendtest.ProbeItem{}
	}
	cases := []struct {
		name   string
		want   bool // nested_virt
		mutate func(map[string]backendtest.ProbeItem)
		runs   bool // EnsureNestedVirt runs
	}{
		{"off converged", false, func(map[string]backendtest.ProbeItem) {}, false},
		{"off, kvm 0660", false, func(m map[string]backendtest.ProbeItem) {
			m["kvmchar"], m["kvmmode"] = backendtest.ProbeItem{}, backendtest.ProbeItem{Out: "660\n"}
		}, false},
		{"off, kvm 0666", false, func(m map[string]backendtest.ProbeItem) {
			m["kvmchar"], m["kvmmode"] = backendtest.ProbeItem{}, backendtest.ProbeItem{Out: "666\n"}
		}, true},
		{"off, rule left", false, func(m map[string]backendtest.ProbeItem) { m["udevexists"] = backendtest.ProbeItem{} }, true},
		{"off, legacy drop-in", false, func(m map[string]backendtest.ProbeItem) { m["kvmlegacy"] = backendtest.ProbeItem{} }, true},
		{"on converged", true, on, false},
		{"on, mode 0660", true, func(m map[string]backendtest.ProbeItem) { on(m); m["kvmmode"] = backendtest.ProbeItem{Out: "660\n"} }, true},
		{"on, no rule", true, func(m map[string]backendtest.ProbeItem) { on(m); m["udev"] = backendtest.ProbeItem{Code: 1} }, true},
		{"on, no device", true, func(m map[string]backendtest.ProbeItem) { on(m); m["kvmchar"] = backendtest.ProbeItem{Code: 1} }, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			items := convergedItems()
			c.mutate(items)
			r := warmVM(items)
			r.Script("limactl shell "+vm+" bash -lc", proc.Result{})
			r.Script("limactl shell "+vm+" sudo bash -lc", proc.Result{})
			warmUp(t, r, backend.Config{NestedVirt: c.want})
			ran := r.Called(proc.ArgvContains("bash -lc", "/dev/kvm"))
			if ran != c.runs {
				t.Errorf("EnsureNestedVirt ran = %v, want %v", ran, c.runs)
			}
		})
	}
}

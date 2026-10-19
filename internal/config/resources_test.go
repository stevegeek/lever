package config

import (
	"strings"
	"testing"
)

func TestLoadResourcesAndWorkerCap(t *testing.T) {
	p := writeConfig(t, `name: demo
backend: lima
tree: ./tree
cpus: 2
memory: 4GiB
guest_profile: small
manager:
  image: lever.local/lever-claude:latest
  resources:
    memory: 1536MiB
    cpu: 1.5
workers:
  - name: appa
    dir: workers/appa
    resources:
      memory: 1Gi
      cpu: 500m
  - name: appb
    dir: workers/appb
    resources:
      cpu: 2
broker:
  llm_auth: subscription
  max_running_workers: 1
`)
	app, err := LoadNoHostChecks(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := app.Manager.Resources; got != (Resources{Memory: "1536MiB", CPU: "1.5"}) {
		t.Errorf("manager resources = %+v", got)
	}
	if got := app.Workers[0].Resources; got != (Resources{Memory: "1Gi", CPU: "500m"}) {
		t.Errorf("worker appa resources = %+v", got)
	}
	// An unquoted YAML integer reads as the same string.
	if got := app.Workers[1].Resources.CPU; got != "2" {
		t.Errorf("worker appb cpu = %q, want \"2\"", got)
	}
	if app.Broker.MaxRunningWorkers != 1 {
		t.Errorf("max_running_workers = %d", app.Broker.MaxRunningWorkers)
	}
	if app.EffectiveGuestProfile() != GuestProfileSmall {
		t.Errorf("guest profile = %q", app.EffectiveGuestProfile())
	}
}

func TestValidateResources(t *testing.T) {
	cases := []struct {
		name    string
		app     App
		r       Resources
		wantErr string
	}{
		{"unset", App{}, Resources{}, ""},
		{"MiB", App{}, Resources{Memory: "1536MiB"}, ""},
		{"GiB", App{}, Resources{Memory: "2GiB"}, ""},
		{"Gi", App{}, Resources{Memory: "2Gi"}, ""},
		{"Mi floor", App{}, Resources{Memory: "512Mi"}, ""},
		{"below floor", App{}, Resources{Memory: "256MiB"}, "too small: at least 512MiB"},
		{"decimal unit", App{}, Resources{Memory: "2GB"}, `resources.memory "2GB" invalid`},
		{"fraction", App{}, Resources{Memory: "1.5GiB"}, `resources.memory "1.5GiB" invalid`},
		{"no unit", App{}, Resources{Memory: "2048"}, "invalid"},
		{"over VM", App{Memory: "4GiB"}, Resources{Memory: "5GiB"}, `more than the VM's memory "4GiB"`},
		{"equal VM", App{Memory: "4GiB"}, Resources{Memory: "4096MiB"}, ""},
		{"cpu int", App{}, Resources{CPU: "1"}, ""},
		{"cpu decimal", App{}, Resources{CPU: "1.25"}, ""},
		{"cpu millis", App{}, Resources{CPU: "250m"}, ""},
		{"cpu zero", App{}, Resources{CPU: "0"}, "must be more than 0"},
		{"cpu zero millis", App{}, Resources{CPU: "0m"}, "must be more than 0"},
		{"cpu negative", App{}, Resources{CPU: "-1"}, "invalid"},
		{"cpu word", App{}, Resources{CPU: "two"}, "invalid"},
		{"cpu flag-shaped", App{}, Resources{CPU: "1 --privileged"}, "invalid"},
		{"cpu over VM", App{CPUs: 2}, Resources{CPU: "2.5"}, "more than the VM's cpus (2)"},
		{"cpu millis over VM", App{CPUs: 2}, Resources{CPU: "2001m"}, "more than the VM's cpus (2)"},
		{"cpu equal VM", App{CPUs: 2}, Resources{CPU: "2"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.app.validateResources("worker \"w\"", c.r)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("want error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}

func TestScionLimits(t *testing.T) {
	cases := []struct {
		in        Resources
		mem, cpus string
	}{
		{Resources{}, "", ""},
		{Resources{Memory: "1536MiB", CPU: "1.5"}, "1536Mi", "1.5"},
		{Resources{Memory: "2GiB"}, "2Gi", ""},
		{Resources{Memory: "2Gi", CPU: "500m"}, "2Gi", "500m"},
	}
	for _, c := range cases {
		mem, cpu := c.in.ScionLimits()
		if mem != c.mem || cpu != c.cpus {
			t.Errorf("%+v: got (%q, %q), want (%q, %q)", c.in, mem, cpu, c.mem, c.cpus)
		}
	}
}

func TestRejectBadResourcesAndCap(t *testing.T) {
	base := `name: demo
backend: lima
tree: ./tree
manager:
  image: lever.local/lever-claude:latest
`
	rejectNoHost(t, base+"  resources:\n    memory: 100MiB\n", "manager resources.memory", "at least 512MiB")
	rejectNoHost(t, base+"workers:\n  - name: w\n    dir: workers/w\n    resources:\n      cpu: lots\n", `worker "w" resources.cpu "lots" invalid`)
	rejectNoHost(t, base+"broker:\n  llm_auth: subscription\n  max_running_workers: -1\n", "broker.max_running_workers -1 out of range")
	rejectNoHost(t, base+"broker:\n  llm_auth: subscription\n  max_running_workers: 1001\n", "out of range")
	rejectNoHost(t, base+"guest_profile: tiny\n", `guest_profile "tiny" invalid`)
}

func TestEffectiveGuestProfile(t *testing.T) {
	cases := []struct{ backend, set, want string }{
		{BackendLima, "", GuestProfileStandard},
		{BackendLima, GuestProfileSmall, GuestProfileSmall},
		{BackendLima, GuestProfileStandard, GuestProfileStandard},
		{BackendOrbstack, "", ""},
	}
	for _, c := range cases {
		a := &App{Backend: c.backend, GuestProfile: c.set}
		if got := a.EffectiveGuestProfile(); got != c.want {
			t.Errorf("%s set=%q: got %q, want %q", c.backend, c.set, got, c.want)
		}
	}
}

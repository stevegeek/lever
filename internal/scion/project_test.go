package scion

import (
	"context"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

func TestInitProjectRunsInDir(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("scion init", proc.Result{})
	c := New(f, Options{})
	if err := c.InitProject(context.Background(), "/g/a"); err != nil {
		t.Fatalf("InitProject: %v", err)
	}
	if f.Calls[0].Dir != "/g/a" || f.Calls[0].Args[0] != "init" {
		t.Fatalf("call=%+v", f.Calls[0])
	}
}

func TestHubLinkArgvAndDir(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("scion hub link", proc.Result{})
	c := New(f, Options{HubEndpoint: "http://127.0.0.1:8080"})
	if err := c.HubLink(context.Background(), "/g/a"); err != nil {
		t.Fatalf("HubLink: %v", err)
	}
	got := strings.Join(f.Calls[0].Args, " ")
	for _, want := range []string{"hub link", "--hub http://127.0.0.1:8080", "-y", "--non-interactive"} {
		if !strings.Contains(got, want) {
			t.Fatalf("argv %q missing %q", got, want)
		}
	}
	if f.Calls[0].Dir != "/g/a" {
		t.Fatalf("dir=%q", f.Calls[0].Dir)
	}
}

// b41cd6fe's `scion hub projects --format json`, trimmed.
const hubProjectsJSON = `[
  {"id": "8b28ef17-fe1e-4a94-ba47-d8d4cb690fcd", "name": "lever", "slug": "lever", "projectType": "hub-managed"},
  {"id": "11111111-0000-0000-0000-000000000000", "name": "other", "slug": "other"}
]`

func TestHubProjectID(t *testing.T) {
	for _, tc := range []struct {
		name, out, want string
		wantErr         bool
	}{
		{"one match", hubProjectsJSON, "8b28ef17-fe1e-4a94-ba47-d8d4cb690fcd", false},
		{"no match", `[{"id": "x", "name": "other"}]`, "", false},
		{"nothing listed", "[]", "", false},
		{"two matches", `[{"id": "a", "name": "lever"}, {"id": "b", "name": "lever"}]`, "", true},
		{"not JSON", `[{"id": `, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := proc.NewFakeRunner()
			f.Script("scion hub projects", proc.Result{Stdout: tc.out})
			c := New(f, Options{HubEndpoint: "http://127.0.0.1:8080"})
			got, err := c.HubProjectID(context.Background(), "lever")
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("HubProjectID = %q, %v; want %q, err %v", got, err, tc.want, tc.wantErr)
			}
			if argv := strings.Join(f.Calls[0].Args, " "); argv != "hub projects --format json --non-interactive" {
				t.Fatalf("argv = %q", argv)
			}
		})
	}
}

func TestPinHubProjectSetsTheProjectsHubID(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("scion config set", proc.Result{})
	c := New(f, Options{})
	if err := c.PinHubProject(context.Background(), "/lever", "8b28ef17"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.Calls[0].Args, " "); got != "config set --non-interactive hub.projectId 8b28ef17" {
		t.Fatalf("argv = %q", got)
	}
	if f.Calls[0].Dir != "/lever" {
		t.Fatalf("dir = %q, want the project dir (project scope)", f.Calls[0].Dir)
	}
}

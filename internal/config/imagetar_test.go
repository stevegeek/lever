package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const tarHeader = "name: demo\nbackend: orbstack\ntree: ws\n"

// image_tar is host-owned boot material like prompt_file: it decides which
// bytes run as the agent, so it must stay inside the instance root.
func TestValidateRejectsUnconfinedImageTar(t *testing.T) {
	cases := map[string]string{
		"manager traversal": tarHeader + "manager:\n  image: lever.local/x\n  image_tar: ../x.tar\n",
		"manager absolute":  tarHeader + "manager:\n  image: lever.local/x\n  image_tar: /srv/x.tar\n",
		"worker traversal":  tarHeader + "manager: {}\nworkers:\n  - name: w\n    dir: ws/w\n    image: lever.local/w\n    image_tar: ../w.tar\n",
		"worker absolute":   tarHeader + "manager: {}\nworkers:\n  - name: w\n    dir: ws/w\n    image: lever.local/w\n    image_tar: /srv/w.tar\n",
	}
	for label, body := range cases {
		_, err := LoadNoHostChecks(writeConfig(t, body))
		if err == nil || !strings.Contains(err.Error(), "image_tar") {
			t.Fatalf("%s: want rejection naming image_tar, got %v", label, err)
		}
	}
}

// An agent inside the mounted tree must not be able to point the next `up`
// at a tar it wrote.
func TestLoadRejectsImageTarInsideTheTree(t *testing.T) {
	cases := map[string]string{
		"manager": tarHeader + "manager:\n  image: lever.local/x\n  image_tar: ws/x.tar\n",
		"worker":  tarHeader + "manager: {}\nworkers:\n  - name: w\n    dir: ws/w\n    image: lever.local/w\n    image_tar: ws/w/w.tar\n",
	}
	for label, body := range cases {
		_, err := LoadNoHostChecks(writeConfig(t, body))
		if err == nil || !strings.Contains(err.Error(), "inside the mounted tree") || !strings.Contains(err.Error(), "image_tar") {
			t.Fatalf("%s: want rejection naming the tree and image_tar, got %v", label, err)
		}
	}
}

// A tar only makes sense beside the image it carries: without `image:` there
// is no ref to match the tar's tags against (a worker's inherited image
// already inherits the manager's tar), and a digest-pinned ref cannot be
// matched by tag at all.
func TestValidateRejectsImageTarWithoutMatchableImage(t *testing.T) {
	cases := map[string]string{
		"manager tar without image": tarHeader + "manager:\n  image_tar: x.tar\n",
		"worker tar without image":  tarHeader + "manager:\n  image: lever.local/x\nworkers:\n  - name: w\n    dir: ws/w\n    image_tar: w.tar\n",
		"manager digest-pinned":     tarHeader + "manager:\n  image: lever.local/x@sha256:" + strings.Repeat("a", 64) + "\n  image_tar: x.tar\n",
	}
	for label, body := range cases {
		_, err := LoadNoHostChecks(writeConfig(t, body))
		if err == nil || !strings.Contains(err.Error(), "image_tar") {
			t.Fatalf("%s: want rejection naming image_tar, got %v", label, err)
		}
	}
}

// One image ref must map to one tar: the bring-up loads each distinct ref
// once, so two tars for the same ref would be an ambiguous source.
func TestValidateRejectsOneImageFromTwoTars(t *testing.T) {
	body := tarHeader + "manager:\n  image: lever.local/x\n  image_tar: a.tar\nworkers:\n  - name: w\n    dir: ws/w\n    image: lever.local/x\n    image_tar: b.tar\n"
	_, err := LoadNoHostChecks(writeConfig(t, body))
	if err == nil || !strings.Contains(err.Error(), "a.tar") || !strings.Contains(err.Error(), "b.tar") {
		t.Fatalf("want rejection naming both tars, got %v", err)
	}
}

func TestImageTarPathsAreRootRelativeAndInheritWithImage(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, CanonicalName)
	body := "name: demo\nbackend: orbstack\ntree: workspace\nbroker:\n  llm_auth: subscription\n" +
		"manager:\n  image: lever.local/x\n  image_tar: images/x.tar\n" +
		"workers:\n" +
		"  - name: inherits\n    dir: workers/a\n" +
		"  - name: own\n    dir: workers/b\n    image: lever.local/y\n    image_tar: images/y.tar\n" +
		"  - name: docker\n    dir: workers/c\n    image: lever.local/z\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	app, err := LoadNoHostChecks(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	abs := func(rel string) string { s, _ := filepath.Abs(filepath.Join(dir, rel)); return s }
	if got := app.ManagerImageTarPath(); got != abs("images/x.tar") {
		t.Fatalf("manager tar = %q, want %q (root-relative)", got, abs("images/x.tar"))
	}
	// No image ⇒ the worker runs the manager image, so it ships in the
	// manager's tar too.
	if got := app.WorkerImageTarPath(app.Workers[0]); got != abs("images/x.tar") {
		t.Fatalf("inheriting worker tar = %q, want the manager's", got)
	}
	if got := app.WorkerImageTarPath(app.Workers[1]); got != abs("images/y.tar") {
		t.Fatalf("own worker tar = %q, want %q", got, abs("images/y.tar"))
	}
	// Own image, no tar ⇒ that image comes from host docker, not the
	// manager's tar (which does not carry it).
	if got := app.WorkerImageTarPath(app.Workers[2]); got != "" {
		t.Fatalf("own-image worker without image_tar must resolve to \"\", got %q", got)
	}
}

func TestNoImageTarMeansNoPath(t *testing.T) {
	app, err := LoadNoHostChecks(writeConfig(t, tarHeader+"manager:\n  image: lever.local/x\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := app.ManagerImageTarPath(); got != "" {
		t.Fatalf("unset image_tar must resolve to \"\", got %q", got)
	}
}

// ImageTagPolicy is nil without a registry allowlist, else the same prefix
// rule validateImage applies to the config's own refs.
func TestImageTagPolicy(t *testing.T) {
	var s Security
	if s.ImageTagPolicy() != nil {
		t.Fatal("no allowed_image_registries: want a nil policy (no check)")
	}
	s.AllowedImageRegistries = []string{"lever.local", "ghcr.io/stevegeek"}
	pol := s.ImageTagPolicy()
	if pol == nil {
		t.Fatal("allowlist configured: want a policy")
	}
	for _, ok := range []string{"lever.local/lever-claude:arm64", "lever.local", "ghcr.io/stevegeek/x:v1"} {
		if err := pol(ok); err != nil {
			t.Errorf("%q: want allowed, got %v", ok, err)
		}
	}
	for _, bad := range []string{"lever.localevil/x:latest", "alpine:latest", "docker.io/lever.local/x", "ghcr.io/other/x"} {
		err := pol(bad)
		if err == nil || !strings.Contains(err.Error(), bad) || !strings.Contains(err.Error(), "allowed registry") {
			t.Errorf("%q: want a refusal naming the ref and the allowlist, got %v", bad, err)
		}
	}
}

// Agents run a configured image under a qualified name the hub's image
// registry setting never rewrites; an unqualified ref and its lever.local/
// form are one name, so a config may not use both.
func TestAgentImageNames(t *testing.T) {
	app, err := LoadNoHostChecks(writeConfig(t, tarHeader+"manager:\n  image: x:1\nworkers:\n  - name: w\n    dir: ws/w\n    image: ghcr.io/o/w:2\n  - name: v\n    dir: ws/v\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := app.ManagerAgentImage(); got != "lever.local/x:1" {
		t.Errorf("ManagerAgentImage = %q", got)
	}
	if got := app.WorkerAgentImage(app.Workers[0]); got != "ghcr.io/o/w:2" {
		t.Errorf("WorkerAgentImage(w) = %q", got)
	}
	if got := app.WorkerAgentImage(app.Workers[1]); got != "lever.local/x:1" {
		t.Errorf("WorkerAgentImage(v) = %q, want the manager's", got)
	}
	body := tarHeader + "manager:\n  image: x:1\nworkers:\n  - name: w\n    dir: ws/w\n    image: lever.local/x:1\n"
	if _, err := LoadNoHostChecks(writeConfig(t, body)); err == nil || !strings.Contains(err.Error(), "both run in the jail as lever.local/x:1") {
		t.Fatalf("want the two names refused, got %v", err)
	}
}

// scionlocal/ is not an alias of lever.local/: a config naming it is refused
// with the name to use, for the manager and a worker alike.
func TestScionlocalImageRefused(t *testing.T) {
	for name, body := range map[string]string{
		"manager": tarHeader + "manager:\n  image: scionlocal/lever-claude:arm64\n",
		"worker":  tarHeader + "manager: {}\nworkers:\n  - name: w\n    dir: ws/w\n    image: scionlocal/lever-claude\n",
		"digest":  tarHeader + "manager:\n  image: scionlocal/x@sha256:" + strings.Repeat("a", 64) + "\n",
	} {
		_, err := LoadNoHostChecks(writeConfig(t, body))
		if err == nil || !strings.Contains(err.Error(), "use lever.local/") {
			t.Errorf("%s: want the scionlocal/ name refused, got %v", name, err)
		}
	}
	for _, ok := range []string{"lever.local/lever-claude:arm64", "scionlocalevil/x:1", "docker.io/scionlocal/x:1"} {
		if _, err := LoadNoHostChecks(writeConfig(t, tarHeader+"manager:\n  image: "+ok+"\n")); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
}

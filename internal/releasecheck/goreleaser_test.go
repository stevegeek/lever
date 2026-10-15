// Package releasecheck holds tests of the release configuration, so a
// mistake there fails CI before a tag instead of at the end of a release.
package releasecheck

import (
	"os"
	"regexp"
	"testing"

	"gopkg.in/yaml.v3"
)

// envOnly is GoReleaser's rule for repository tokens (internal/tmpl,
// ApplySingleEnvOnly): exactly one {{ .Env.NAME }} and nothing else. It is
// enforced only when the cask is uploaded, the last step of a release.
var envOnly = regexp.MustCompile(`^{{\s*\.Env\.([^.\s}]+)\s*}}$`)

type repository struct {
	Token       string `yaml:"token"`
	PullRequest struct {
		Token string `yaml:"token"`
	} `yaml:"pull_request"`
}

type publisher struct {
	Name       string     `yaml:"name"`
	Repository repository `yaml:"repository"`
	SkipUpload string     `yaml:"skip_upload"`
}

type goreleaserConfig struct {
	Casks []publisher `yaml:"homebrew_casks"`
	Brews []publisher `yaml:"brews"`
}

func TestGoreleaserRepositoryTokens(t *testing.T) {
	raw, err := os.ReadFile("../../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg goreleaserConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	pubs := append(cfg.Casks, cfg.Brews...)
	if len(pubs) == 0 {
		t.Fatal("no homebrew_casks or brews entry in .goreleaser.yaml")
	}
	for _, p := range pubs {
		for field, tok := range map[string]string{
			"repository.token":              p.Repository.Token,
			"repository.pull_request.token": p.Repository.PullRequest.Token,
		} {
			if tok == "" {
				continue
			}
			m := envOnly.FindStringSubmatch(tok)
			if m == nil {
				t.Errorf("%s: %s is %q; GoReleaser accepts only {{ .Env.NAME }}", p.Name, field, tok)
				continue
			}
			// .Env.NAME fails on an unset variable, so the upload must be
			// skipped when it is unset.
			want := `isEnvSet "` + m[1] + `"`
			if !regexp.MustCompile(regexp.QuoteMeta(want)).MatchString(p.SkipUpload) {
				t.Errorf("%s: skip_upload %q does not skip when %s is unset (want %s)", p.Name, p.SkipUpload, m[1], want)
			}
		}
	}
}

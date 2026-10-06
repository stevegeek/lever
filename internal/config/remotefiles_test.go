package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/chatfiles"
)

func filesApp() *App {
	return &App{Name: "boss", Tree: "/t", Workers: []Worker{{Name: "w1", Dir: "workers/w1"}},
		Remote: Remote{Enabled: true, BaseURL: "https://mac.ts.net", Landing: RemoteLandingChat,
			AllowedUsers: []RemoteUser{{Login: "op@x"}, {Login: "c@x", Tier: TierContact, Agents: []string{"w1"}}}}}
}

func TestRemoteFilesValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		mut  func(a *App)
		want string
	}{
		"off":              {func(a *App) {}, ""},
		"on":               {func(a *App) { a.Remote.Files.Enabled = true }, ""},
		"on console":       {func(a *App) { a.Remote.Files.Enabled = true; a.Remote.Landing = "" }, "landing: chat"},
		"max 100 MiB":      {func(a *App) { a.Remote.Files.MaxBytes = 100 << 20 }, ""},
		"max over":         {func(a *App) { a.Remote.Files.MaxBytes = 100<<20 + 1 }, "max_bytes"},
		"max negative":     {func(a *App) { a.Remote.Files.MaxBytes = -1 }, "max_bytes"},
		"ext ok":           {func(a *App) { a.Remote.Files.Extensions = []string{"pdf", "xlsm"} }, ""},
		"ext dot":          {func(a *App) { a.Remote.Files.Extensions = []string{".pdf"} }, "extensions"},
		"ext upper":        {func(a *App) { a.Remote.Files.Extensions = []string{"PDF"} }, "extensions"},
		"ext dup":          {func(a *App) { a.Remote.Files.Extensions = []string{"pdf", "pdf"} }, "twice"},
		"ext html":         {func(a *App) { a.Remote.Files.Extensions = []string{"pdf", "html"} }, "active content"},
		"ext svg":          {func(a *App) { a.Remote.Files.Extensions = []string{"svg"} }, "active content"},
		"ext mjs":          {func(a *App) { a.Remote.Files.Extensions = []string{"mjs"} }, "active content"},
		"read_only upper":  {func(a *App) { a.Remote.Files.Enabled = true; a.Manager.ReadOnly = []string{".LEVER-FILES"} }, "read_only"},
		"read_only inside": {func(a *App) { a.Remote.Files.Enabled = true; a.Manager.ReadOnly = []string{".lever-files/out"} }, "read_only"},
		"read_only root":   {func(a *App) { a.Remote.Files.Enabled = true; a.Manager.ReadOnly = []string{"."} }, "read_only"},
		"read_only other":  {func(a *App) { a.Remote.Files.Enabled = true; a.Manager.ReadOnly = []string{"kb"} }, ""},
		"read_only off":    {func(a *App) { a.Manager.ReadOnly = []string{".lever-files"} }, ""},
		"worker in files":  {func(a *App) { a.Remote.Files.Enabled = true; a.Workers[0].Dir = ".Lever-Files/w1" }, "overlaps"},
		"read_only files":  {func(a *App) { a.Remote.Files.Enabled = true; a.Manager.ReadOnly = []string{".lever-files"} }, "read_only"},
		"worker off files": {func(a *App) { a.Workers[0].Dir = ".lever-files/w1" }, ""},
	} {
		t.Run(name, func(t *testing.T) {
			a := filesApp()
			tc.mut(a)
			err := a.validateRemote()
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestFilesGetters(t *testing.T) {
	a := filesApp()
	if a.FilesOn() || a.EffectiveFilesMaxBytes() != 25<<20 || !slices.Equal(a.EffectiveFilesExtensions(), chatfiles.DefaultExtensions) {
		t.Fatal("defaults")
	}
	a.Remote.Files = Files{Enabled: true, MaxBytes: 1 << 20, Extensions: []string{"pdf"}}
	if !a.FilesOn() || a.EffectiveFilesMaxBytes() != 1<<20 || !slices.Equal(a.EffectiveFilesExtensions(), []string{"pdf"}) {
		t.Fatal("set")
	}
	a.EffectiveFilesExtensions()[0] = "x"
	if a.Remote.Files.Extensions[0] != "pdf" {
		t.Fatal("the getter must return a copy")
	}
	a.Remote.Landing = ""
	if a.FilesOn() {
		t.Fatal("files need landing chat")
	}
	ws := a.AgentWorkspaces()
	if ws["boss"] != "." || ws["w1"] != "workers/w1" || len(ws) != 2 {
		t.Fatalf("%v", ws)
	}
}

func TestDefaultFilesExtensionsHoldNoActiveContent(t *testing.T) {
	for _, e := range chatfiles.DefaultExtensions {
		if slices.Contains(filesActiveExts, e) || !filesExtRE.MatchString(e) {
			t.Errorf("default extension %q", e)
		}
	}
}

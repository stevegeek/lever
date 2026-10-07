// Command lever-tool-fizzy is the host-side broker tool for the Lever Fizzy
// board: six operations (list, show, comments, comment, move, create) on ONE
// board, through the official fizzy CLI with the token in its environment.
// Spec: docs/superpowers/specs/2026-10-07-lever-dev-on-desktop-design.md §4b.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/stevegeek/lever/captool"
	"github.com/stevegeek/lever/internal/fizzytool"
)

// Version is stamped by the Makefile (-X main.Version=...).
var Version = "dev"

var (
	boardRE   = regexp.MustCompile(`^[a-z0-9]{10,40}$`)
	accountRE = regexp.MustCompile(`^[0-9]{1,12}$`)
)

type opts struct{ name, backend, admin, fizzy, tokenFile, account, board, state, prefix string }

func parseFlags(args []string) (opts, error) {
	var o opts
	fs := flag.NewFlagSet("lever-tool-fizzy", flag.ContinueOnError)
	fs.StringVar(&o.name, "name", "fizzy", "tool/registry name")
	fs.StringVar(&o.backend, "backend", "127.0.0.1:3211", "MCP listen address (set by the broker)")
	fs.StringVar(&o.admin, "admin", "", "broker admin base URL (set by the broker)")
	fs.StringVar(&o.fizzy, "fizzy", "", "absolute path to the fizzy CLI")
	fs.StringVar(&o.tokenFile, "token-file", "", "Fizzy personal access token (0600)")
	fs.StringVar(&o.account, "account", "", "Fizzy account id")
	fs.StringVar(&o.board, "board", "", "the only board id this tool may touch")
	fs.StringVar(&o.state, "state", "", "absolute private state dir")
	fs.StringVar(&o.prefix, "prefix", "[lever-dev agent] ", "text prepended to every comment and description")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	for _, r := range []struct{ f, v string }{{"-fizzy", o.fizzy}, {"-token-file", o.tokenFile}, {"-account", o.account}, {"-board", o.board}, {"-state", o.state}} {
		if r.v == "" {
			return o, fmt.Errorf("%s is required", r.f)
		}
	}
	for _, r := range []struct{ f, v string }{{"-fizzy", o.fizzy}, {"-token-file", o.tokenFile}, {"-state", o.state}} {
		if !filepath.IsAbs(r.v) {
			return o, fmt.Errorf("%s must be an absolute path, got %q", r.f, r.v)
		}
	}
	if !boardRE.MatchString(o.board) {
		return o, fmt.Errorf("-board %q invalid", o.board)
	}
	if !accountRE.MatchString(o.account) {
		return o, fmt.Errorf("-account %q invalid", o.account)
	}
	if strings.TrimSpace(o.prefix) == "" {
		return o, fmt.Errorf("-prefix must not be empty: every comment and description is marked as written by the agent")
	}
	return o, nil
}

// loadToken reads the token file: regular, 0600, owned by this user.
func loadToken(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		return "", fmt.Errorf("token file %s: want a regular file with mode 0600", path)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return "", fmt.Errorf("token file %s: not owned by the running user", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	return tok, nil
}

// asResult turns a refusal into a result the agent can read (captool maps a
// Handler error to a bare "tool error"); the tool's audit line still records
// decision=deny.
func asResult(v any, err error) (any, error) {
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}, nil
	}
	return v, nil
}

func op(name, desc string, params []captool.ParamSpec, h func(captool.ValidatedContext, map[string]string) (any, error)) captool.Operation {
	return captool.Operation{Name: name, Description: desc, Params: params,
		Handler: func(c captool.ValidatedContext, a map[string]string) (any, error) { return asResult(h(c, a)) }}
}

func main() {
	o, err := parseFlags(os.Args[1:])
	if err != nil {
		log.Fatal(err)
	}
	tok, err := loadToken(o.tokenFile)
	if err != nil {
		log.Fatal(err)
	}
	// A SIGKILLed previous run (the supervisor's Stop) can leave body files.
	_ = os.RemoveAll(filepath.Join(o.state, "tmp"))
	dirs := map[string]string{"home": "", "work": "", "tmp": ""}
	for d := range dirs {
		p := filepath.Join(o.state, d)
		if err := os.MkdirAll(p, 0o700); err != nil {
			log.Fatal(err)
		}
		if err := os.Chmod(p, 0o700); err != nil {
			log.Fatal(err)
		}
		dirs[d] = p
	}
	if err := fizzytool.CheckNoLocalConfig(dirs["work"]); err != nil {
		log.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	t := &fizzytool.Tool{
		CLI:   fizzytool.CLI{Bin: o.fizzy, Account: o.account, Token: tok, Home: dirs["home"], Work: dirs["work"], APIURL: "https://app.fizzy.do", Timeout: 30 * time.Second, MaxOut: 1 << 20},
		Board: o.board, Prefix: o.prefix, TmpDir: dirs["tmp"], Log: logger,
	}
	ctx := context.Background()
	if err := fizzytool.CheckVersion(ctx, t.CLI); err != nil {
		log.Fatal(err)
	}
	num := captool.ParamSpec{Name: "number", Type: "string", Description: "card number"}
	srv, err := captool.New(captool.Config{
		Name: o.name, Version: Version, Backend: o.backend, AdminURL: o.admin, Log: logger,
		Operations: []captool.Operation{
			op("list_cards", "list cards on the Lever board, optionally by column id (or not-now/maybe/done) and search text",
				[]captool.ParamSpec{{Name: "column", Type: "string"}, {Name: "search", Type: "string"}, {Name: "page", Type: "string", Description: "page number (digits), default 1"}},
				func(c captool.ValidatedContext, a map[string]string) (any, error) {
					return t.ListCards(ctx, c.Caller, a["column"], a["search"], a["page"])
				}),
			op("show_card", "show one card on the Lever board", []captool.ParamSpec{num},
				func(c captool.ValidatedContext, a map[string]string) (any, error) {
					return t.ShowCard(ctx, c.Caller, a["number"])
				}),
			op("list_comments", "list a card's comments", []captool.ParamSpec{num},
				func(c captool.ValidatedContext, a map[string]string) (any, error) {
					return t.ListComments(ctx, c.Caller, a["number"])
				}),
			op("comment", "add a comment (markdown) to a card; it is shown with the "+strings.TrimSpace(o.prefix)+" prefix",
				[]captool.ParamSpec{num, {Name: "body", Type: "string"}},
				func(c captool.ValidatedContext, a map[string]string) (any, error) {
					return t.Comment(ctx, c.Caller, a["number"], a["body"])
				}),
			op("move_card", "move a card to a column of the Lever board (column id)", []captool.ParamSpec{num, {Name: "column", Type: "string"}},
				func(c captool.ValidatedContext, a map[string]string) (any, error) {
					return t.MoveCard(ctx, c.Caller, a["number"], a["column"])
				}),
			op("create_card", "create a card on the Lever board", []captool.ParamSpec{{Name: "title", Type: "string"}, {Name: "description", Type: "string"}},
				func(c captool.ValidatedContext, a map[string]string) (any, error) {
					return t.CreateCard(ctx, c.Caller, a["title"], a["description"])
				}),
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := srv.Register(ctx); err != nil {
		log.Fatalf("register with broker: %v", err)
	}
	log.Printf("lever-tool-fizzy %q serving MCP on %s (board %s)", o.name, o.backend, o.board)
	log.Fatal(http.ListenAndServe(o.backend, srv.Handler()))
}

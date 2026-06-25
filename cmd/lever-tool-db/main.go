// Command lever-tool-db is the reference first-party capability-aware tool: a
// SQLite-backed read-only DB exposed over MCP via captool, with a hard
// table-allowlist backstop. It is the acceptance vehicle.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"

	"github.com/lever-to/lever/captool"
)

// readBackstop enforces the tool's hard invariants regardless of the token:
// read-only and table ∈ {A,B}.
func readBackstop(ctx captool.ValidatedContext, args map[string]string) error {
	if ctx.Operation != "read" {
		return fmt.Errorf("ref: backstop: only read is permitted")
	}
	if !allowedTables[args["table"]] {
		return fmt.Errorf("ref: backstop: table %q is forbidden", args["table"])
	}
	return nil
}

// buildServer declares the read operation over store and returns the captool server.
func buildServer(store *Store, name, backend, adminURL string) (*captool.Server, error) {
	return captool.New(captool.Config{
		Name: name, Backend: backend, AdminURL: adminURL,
		Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
		Operations: []captool.Operation{{
			Name: "read", Description: "read rows from an allowed table filtered by owner",
			Params: []captool.ParamSpec{
				{Name: "table", Type: "string", Description: "table name (A or B)"},
				{Name: "filter", Type: "string", Description: "owner to filter by"},
			},
			CaveatParam: map[string]string{"table": "table", "filter": "filter"},
			Backstop:    readBackstop,
			Handler: func(_ captool.ValidatedContext, a map[string]string) (any, error) {
				return store.Read(a["table"], a["filter"])
			},
		}},
	})
}

func main() {
	name := flag.String("name", "db", "tool/registry name")
	backend := flag.String("backend", "127.0.0.1:3201", "MCP listen address")
	admin := flag.String("admin", "http://127.0.0.1:3401", "broker admin base URL")
	dsn := flag.String("dsn", "file:ref.db", "sqlite DSN")
	flag.Parse()

	store, err := OpenStore(*dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	srv, err := buildServer(store, *name, *backend, *admin)
	if err != nil {
		log.Fatal(err)
	}
	if err := srv.Register(context.Background()); err != nil {
		log.Fatalf("register with broker: %v", err)
	}
	log.Printf("lever-tool-db %q serving MCP on %s", *name, *backend)
	log.Fatal(http.ListenAndServe(*backend, srv.Handler()))
}

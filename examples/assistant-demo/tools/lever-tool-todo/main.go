// Command lever-tool-todo is the demo's FIRST-PARTY capability tool: a tiny
// read-only MCP server, built with the captool SDK, that lists todos from a CSV.
// It shows the internal capability path end to end — the broker spawns it as a
// supervised subprocess, registers it, and an agent reaches it only with a
// broker-minted `todo/list` capability token (verified here, independently of
// the gateway, by captool). Contrast tools/weather-stub, which is fronted as an
// EXTERNAL tool the broker only proxies. See the example README.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"

	"github.com/stevegeek/lever/captool"
)

func buildServer(store *Store, name, backend, adminURL string) (*captool.Server, error) {
	return captool.New(captool.Config{
		Name: name, Backend: backend, AdminURL: adminURL,
		Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
		Operations: []captool.Operation{{
			Name:        "list",
			Description: "list todo items; pass pending=true for only unfinished ones",
			Params: []captool.ParamSpec{
				{Name: "pending", Type: "string", Description: `"true" to list only not-done items`},
			},
			// Backstop: read-only tool, the only operation is list — reject anything else
			// regardless of what a token might claim.
			Backstop: func(ctx captool.ValidatedContext, _ map[string]string) error {
				if ctx.Operation != "list" {
					return fmt.Errorf("todo: only list is permitted")
				}
				return nil
			},
			Handler: func(_ captool.ValidatedContext, a map[string]string) (any, error) {
				return store.List(a["pending"] == "true")
			},
		}},
	})
}

func main() {
	name := flag.String("name", "todo", "tool/registry name")
	backend := flag.String("backend", "127.0.0.1:3210", "MCP listen address")
	admin := flag.String("admin", "http://127.0.0.1:3401", "broker admin base URL")
	csvPath := flag.String("csv", "data/todos.csv", "path to the todos CSV")
	flag.Parse()

	srv, err := buildServer(OpenStore(*csvPath), *name, *backend, *admin)
	if err != nil {
		log.Fatal(err)
	}
	if err := srv.Register(context.Background()); err != nil {
		log.Fatalf("register with broker: %v", err)
	}
	log.Printf("lever-tool-todo %q serving MCP on %s (csv=%s)", *name, *backend, *csvPath)
	log.Fatal(http.ListenAndServe(*backend, srv.Handler()))
}

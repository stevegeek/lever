// Command lever-tool-todo is the demo's first-party capability tool: a
// read-only MCP server, built with the captool SDK, that lists todos from a
// CSV. The broker spawns and supervises it, and an agent reaches it only with
// a broker-minted `todo/list` capability token, which captool verifies here
// independently of the gateway. Contrast tools/weather-stub, an external tool
// the broker only proxies.
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

// Version is what the MCP serverInfo reports; stamp it with
// -ldflags "-X main.Version=..." if you want more than "dev".
var Version = "dev"

func buildServer(store *Store, name, backend, adminURL string) (*captool.Server, error) {
	return captool.New(captool.Config{
		Name: name, Version: Version, Backend: backend, AdminURL: adminURL,
		Log: slog.Default(),
		Operations: []captool.Operation{{
			Name:        "list",
			Description: "list todo items; pass pending=true for only unfinished ones",
			Params: []captool.ParamSpec{
				{Name: "pending", Type: "string", Description: `"true" to list only not-done items`},
			},
			// Backstop: the only operation is list; reject anything else whatever a
			// token claims.
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
	// Also the default logger, so standard log lines take lever's log shape.
	slog.SetDefault(captool.NewLogger(os.Stderr, "lever-tool-todo"))

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

// Command weather-stub is a stand-in for an EXTERNAL MCP server the broker
// FRONTS (proxies) rather than owns — the same shape as a real host service like
// a calendar or a weather API you already run. It speaks plain MCP over HTTP and
// returns deterministic demo data, so the example runs with no API key and no
// internet. In the config it is registered `external: true`: the broker does not
// spawn it (you start it yourself), it only capability-gates and proxies it, and
// strips the token before forwarding. Contrast tools/lever-tool-todo, a
// first-party tool the broker supervises. See the example README.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
)

// one canned forecast — deterministic so the demo is reproducible.
var forecast = map[string]any{
	"location":     "Pisa",
	"conditions":   "clear sky",
	"temp_c":       26.4,
	"feels_like_c": 28.1,
	"humidity_pct": 51,
	"wind_kmh":     9.0,
	"high_c":       31.0,
	"summary":      "Clear and warm — a good day to be outside.",
}

func main() {
	addr := flag.String("addr", "127.0.0.1:3211", "listen address")
	flag.Parse()

	http.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		var msg map[string]any
		_ = json.NewDecoder(r.Body).Decode(&msg)
		id := msg["id"]
		method, _ := msg["method"].(string)
		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "initialize":
			writeResult(w, id, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "weather-stub", "version": "0.1.0"},
			})
		case "tools/list":
			writeResult(w, id, map[string]any{"tools": []any{map[string]any{
				"name":        "get_weather",
				"description": "current conditions and today's forecast for the configured location",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
			}}})
		case "tools/call":
			b, _ := json.Marshal(forecast)
			writeResult(w, id, map[string]any{"content": []any{map[string]any{"type": "text", "text": string(b)}}})
		default:
			writeResult(w, id, map[string]any{})
		}
	})

	log.Printf("weather-stub serving MCP on %s/mcp (deterministic demo data)", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func writeResult(w http.ResponseWriter, id, result any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

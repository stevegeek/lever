// Command fakeupstream is a deterministic stand-in for api.anthropic.com used by
// the api-key live test suite. Point the broker's /llm proxy at it
// (broker.llm_upstream: http://127.0.0.1:<port>) and it records, for every
// request it receives, the method/path and the auth headers — so a test can
// assert that the real Console key arrived (injected by the proxy) and the
// inbound capability token did NOT (stripped by the proxy). It returns a minimal
// non-streaming Messages response so `claude` completes its turn.
//
// It NEVER prints secrets it wasn't given: it records exactly the header values
// the broker forwarded, which is the point of the test (the real key is supplied
// by the operator running the suite, not embedded here).
//
// Usage: fakeupstream -addr 127.0.0.1:8098 -log /path/to/requests.log
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8098", "listen address")
	logPath := flag.String("log", "", "append a one-line record per request here (also stdout)")
	flag.Parse()

	var mu sync.Mutex
	record := func(line string) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Println(line)
		if *logPath != "" {
			f, err := os.OpenFile(*logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err == nil {
				fmt.Fprintln(f, line)
				f.Close()
			}
		}
	}

	h := func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		record(fmt.Sprintf("HIT %s %s | authorization=%q | x-api-key=%q | anthropic-version=%q",
			r.Method, r.URL.RequestURI(),
			r.Header.Get("Authorization"), r.Header.Get("x-api-key"), r.Header.Get("anthropic-version")))
		body, _ := json.Marshal(map[string]any{
			"id": "msg_fake", "type": "message", "role": "assistant", "model": "fake",
			"content":     []map[string]any{{"type": "text", "text": "ok"}},
			"stop_reason": "end_turn",
			"usage":       map[string]int{"input_tokens": 1, "output_tokens": 1},
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	}

	log.Printf("fakeupstream listening on %s (log=%s)", *addr, *logPath)
	if err := http.ListenAndServe(*addr, http.HandlerFunc(h)); err != nil {
		log.Fatal(err)
	}
}

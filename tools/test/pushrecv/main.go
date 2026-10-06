// Command pushrecv is a TEST-ONLY fake push service for lever's e2e: it
// listens on a loopback address, hands out one subscription per id, and
// decrypts and logs every push it gets. A lever remote proxy reaches it
// only with LEVER_PUSH_TEST_HOSTS=<that address> in its environment. Never
// part of a release (.goreleaser.yaml builds ./cmd/lever only).
package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/webpush"
)

type sub struct {
	key  *ecdh.PrivateKey
	auth []byte
}

type receiver struct {
	addr, out string
	subs      map[string]sub
	gone      []string
	mu        sync.Mutex
}

func newReceiver(addr, out string, ids, gone []string) (*receiver, error) {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil || !ap.Addr().IsLoopback() {
		return nil, fmt.Errorf("pushrecv: %s is not a loopback address", addr)
	}
	r := &receiver{addr: addr, out: out, subs: map[string]sub{}, gone: gone}
	for _, id := range ids {
		k, err := ecdh.P256().GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		a := make([]byte, 16)
		rand.Read(a)
		r.subs[id] = sub{k, a}
		b, _ := json.Marshal(map[string]any{"endpoint": "http://" + addr + "/push/" + id,
			"keys": map[string]string{"p256dh": base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()), "auth": base64.RawURLEncoding.EncodeToString(a)}})
		if err := os.WriteFile(filepath.Join(out, "sub-"+id+".json"), b, 0o600); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	id, ok := strings.CutPrefix(req.URL.Path, "/push/")
	s, known := r.subs[id]
	if !ok || !known || req.Method != http.MethodPost {
		http.NotFound(w, req)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(req.Body, 8192))
	status := http.StatusCreated
	if slices.Contains(r.gone, id) {
		status = http.StatusGone
	}
	vapid := "ok"
	if err := webpush.VerifyVAPID(req.Header.Get("Authorization"), "http://"+r.addr, time.Now()); err != nil {
		vapid = err.Error()
	}
	payload := ""
	if pt, err := webpush.Decrypt(s.key, s.auth, body); err == nil {
		payload = string(pt)
	} else {
		payload = "DECRYPT FAILED: " + err.Error()
	}
	line, _ := json.Marshal(map[string]any{"at": time.Now().UTC(), "id": id, "status": status, "payload": payload, "vapid": vapid,
		"encoding": req.Header.Get("Content-Encoding"), "ttl": req.Header.Get("TTL"), "urgency": req.Header.Get("Urgency"), "bytes": len(body)})
	r.mu.Lock()
	f, err := os.OpenFile(filepath.Join(r.out, "received.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err == nil {
		f.Write(append(line, '\n'))
		f.Close()
	}
	r.mu.Unlock()
	w.WriteHeader(status)
}

func main() {
	listen := flag.String("listen", "127.0.0.1:9447", "loopback address to listen on")
	ids := flag.String("ids", "op,c", "subscription ids")
	gone := flag.String("gone", "", "ids answered 410 Gone")
	out := flag.String("out", ".", "directory for sub-<id>.json and received.jsonl")
	flag.Parse()
	r, err := newReceiver(*listen, *out, strings.Split(*ids, ","), strings.Split(*gone, ","))
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("pushrecv (TEST ONLY) on http://%s, subscriptions in %s", *listen, *out)
	log.Fatal(http.ListenAndServe(*listen, r))
}

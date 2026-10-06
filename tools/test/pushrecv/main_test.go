package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stevegeek/lever/internal/webpush"
)

func TestReceiverDecryptsAndLogs(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	out := t.TempDir()
	r, err := newReceiver(ln.Addr().String(), out, []string{"op", "gone"}, []string{"gone"})
	if err != nil {
		t.Fatal(err)
	}
	go http.Serve(ln, r)
	var sub struct {
		Endpoint string                        `json:"endpoint"`
		Keys     struct{ P256DH, Auth string } `json:"keys"`
	}
	b, _ := os.ReadFile(filepath.Join(out, "sub-op.json"))
	json.Unmarshal(b, &sub)
	th, _ := webpush.ParseTestHosts(ln.Addr().String())
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	s := &webpush.Sender{Key: key, Subject: "mailto:x@y", Test: th, Client: webpush.NewClient(th)}
	if err := s.Send(context.Background(), webpush.Subscription{Endpoint: sub.Endpoint, P256DH: sub.Keys.P256DH, Auth: sub.Keys.Auth}, []byte(`{"v":1,"agent":"w1"}`)); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(filepath.Join(out, "received.jsonl"))
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan()
	var line map[string]any
	json.Unmarshal(sc.Bytes(), &line)
	if line["id"] != "op" || line["payload"] != `{"v":1,"agent":"w1"}` || line["vapid"] != "ok" || line["status"] != float64(201) {
		t.Fatalf("%v", line)
	}
}

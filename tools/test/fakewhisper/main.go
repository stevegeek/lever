// Command fakewhisper is a TEST-ONLY stand-in for whisper.cpp's
// whisper-server: it takes the flags lever passes (internal/voice/whisper.go)
// and answers POST /inference with a fixed transcript that names what it
// was sent, so a test can check the form without a model or a GPU. Never
// part of a release (.goreleaser.yaml builds ./cmd/lever only).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

func main() {
	host := flag.String("host", "127.0.0.1", "listen address")
	port := flag.Int("port", 0, "listen port")
	model := flag.String("m", "", "model path (must exist)")
	noGPU := flag.Bool("no-gpu", false, "accepted, as whisper-server does")
	exitAfter := flag.Duration("exit-after", 0, "TEST: exit with status 3 after this long (0 = never)")
	fail := flag.Bool("fail", false, "TEST: answer every request with HTTP 500")
	flag.Parse()
	if _, err := os.Stat(*model); err != nil {
		log.Fatalf("fakewhisper: model: %v", err)
	}
	if *exitAfter > 0 {
		time.AfterFunc(*exitAfter, func() { os.Exit(3) })
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(*host, strconv.Itoa(*port)))
	if err != nil {
		log.Fatalf("fakewhisper: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /inference", func(w http.ResponseWriter, r *http.Request) {
		if *fail {
			http.Error(w, "failed", http.StatusInternalServerError)
			return
		}
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "no file"})
			return
		}
		defer f.Close()
		head := make([]byte, 4)
		n, _ := f.Read(head)
		size, _ := f.Seek(0, 2)
		text := fmt.Sprintf(" heard %s %d bytes; language %s; prompt %s; format %s; gpu %v \n", head[:n], size,
			r.FormValue("language"), r.FormValue("prompt"), r.FormValue("response_format"), !*noGPU)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"text": text})
	})
	log.Fatal(http.Serve(ln, mux))
}

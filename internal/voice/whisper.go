package voice

// The whisper.cpp adapter: every assumption lever makes about whisper.cpp's
// server (examples/server, built as whisper-server) is in this file. Check
// them against the whisper.cpp version in use:
//
//  1. Flags: --host <addr>, --port <n>, -m <model path>, --no-gpu. Nothing
//     else; in particular not --convert, so the server never runs ffmpeg on
//     what it is sent (lever sends only PCM WAV it has checked).
//  2. Transcription: POST /inference, multipart/form-data, audio in field
//     "file", text fields "response_format" ("json"), "prompt" (optional)
//     and "language" (always sent, the code or "auto": the server's own
//     default is a fixed language).
//  3. A json answer's "text" field is the transcript. A failure is a
//     non-200 status, or a JSON object with an "error" field.
//  4. The server answers one request at a time; lever queues in front of it
//     anyway.
//  5. The server prints what it transcribes to its console, so lever
//     discards the child's stdout and stderr: a transcript must never reach
//     the tool's log.
//  6. Readiness: the server binds --port after loading the model; lever
//     probes with a connect.
//  7. The server also has routes lever never uses: /load (loads a model
//     from any path the request names, and exits when that fails), /health,
//     and static files from --public, relative to its working directory.
//     Only host processes reach its loopback port (config load keeps
//     -whisper-port out of every port the jail may reach), and lever runs it
//     in the model directory (refused inside the tree), so the static path
//     resolves nowhere an agent can write.
//  8. --request-path P puts every route under P. The server answers CORS
//     requests from any origin and checks no Host header, so without a
//     secret prefix any web page on the host could post to /load. lever
//     passes a fresh random prefix per tool start and never logs it.
//  9. The server stops transcribing a clip whose client connection closed,
//     so lever frees its GPU slot when the browser gives up.
//
// Checked against whisper.cpp's examples/server/server.cpp (master,
// 2026-10).

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ServerArgs is the whisper-server argument list for model on
// 127.0.0.1:port (assumption 1). requestPath is the secret route prefix
// (assumption 8), fresh per tool start, so a browser page on the host or a
// DNS-rebound name cannot reach /load or /inference.
func ServerArgs(modelPath string, port int, gpu bool, requestPath string) []string {
	args := []string{"--host", "127.0.0.1", "--port", strconv.Itoa(port), "-m", modelPath}
	if requestPath != "" {
		args = append(args, "--request-path", requestPath)
	}
	if !gpu {
		args = append(args, "--no-gpu")
	}
	return args
}

// ErrNotSent: the clip never reached whisper-server. No work was done, so
// the clip's use of the limits (the proxy's per-login, the tool's per-agent)
// is given back.
var ErrNotSent = errors.New("the clip did not reach whisper-server")

// ErrServer: whisper-server answered, but not with a transcript.
var ErrServer = errors.New("whisper-server")

const maxAnswer = 1 << 20

// Request is one transcription of a WAV clip lever has checked.
type Request struct {
	// Path is the server's secret route prefix (ServerArgs); "" = none.
	Path     string
	WAV      []byte
	Prompt   string
	Language string // "" = auto-detect
}

// Transcribe sends r to the whisper-server at addr and returns the transcript
// as the server gave it (assumptions 2 and 3).
func Transcribe(ctx context.Context, client *http.Client, addr string, r Request) (string, error) {
	// The form is built around the clip rather than copying it: text
	// fields and the file part's header, the clip, then the closing
	// boundary.
	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	fields := [][2]string{{"response_format", "json"}, {"language", r.Language}}
	if r.Language == "" {
		fields[1][1] = "auto"
	}
	if r.Prompt != "" {
		fields = append(fields, [2]string{"prompt", r.Prompt})
	}
	for _, f := range fields {
		if err := mw.WriteField(f[0], f[1]); err != nil {
			return "", err
		}
	}
	if _, err := mw.CreateFormFile("file", "audio.wav"); err != nil {
		return "", err
	}
	tail := "\r\n--" + mw.Boundary() + "--\r\n"
	body := io.MultiReader(&head, bytes.NewReader(r.WAV), strings.NewReader(tail))
	size := int64(head.Len()) + int64(len(r.WAV)) + int64(len(tail))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+r.Path+"/inference", body)
	if err != nil {
		return "", err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		// Never the URL: it carries the secret route prefix, and this error
		// reaches the audit log. A failed dial is ErrNotSent.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		var oe *net.OpError
		if errors.As(err, &oe) && oe.Op == "dial" {
			return "", fmt.Errorf("%w: %v", ErrNotSent, err)
		}
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: HTTP %d", ErrServer, resp.StatusCode)
	}
	var ans struct {
		Text  *string `json:"text"`
		Error any     `json:"error"`
	}
	if err := json.Unmarshal(raw, &ans); err != nil {
		return "", fmt.Errorf("%w: the answer is not JSON", ErrServer)
	}
	if ans.Error != nil || ans.Text == nil {
		// Never the server's text: it may quote what it heard.
		return "", fmt.Errorf("%w: no transcript in the answer", ErrServer)
	}
	return *ans.Text, nil
}

// NewRequestPath is a fresh secret route prefix: "/" and 32 hex digits from
// crypto/rand.
func NewRequestPath() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "/" + hex.EncodeToString(b[:]), nil
}

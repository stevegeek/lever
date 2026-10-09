package voice

// The whisper.cpp adapter: every assumption lever makes about whisper.cpp's
// server (examples/server, built as whisper-server) is in this file. Check
// them against the whisper.cpp version in use:
//
//  1. Flags: --host <addr>, --port <n>, -m <model path>, --no-gpu. Nothing
//     else is passed; in particular not --convert, so the server never runs
//     ffmpeg on what it is sent (lever sends only PCM WAV it has checked).
//  2. Transcription: POST /inference, multipart/form-data, with the audio in
//     the field "file" and the text fields "response_format" ("json"),
//     "prompt" (optional: the vocabulary) and "language" (always sent: the
//     configured code, or "auto" to detect it, since the server's own
//     default is a fixed language).
//  3. The answer to a json request is a JSON object whose "text" field is
//     the transcript. A failure is a non-200 status, or a JSON object with
//     an "error" field.
//  4. The server answers one request at a time; lever queues in front of it
//     anyway (remoteproxy's voice slots).
//  5. The server prints what it transcribes to its console. lever therefore
//     discards the child's stdout and stderr (supervisor.go): a transcript
//     must never reach remote.log.
//  6. Readiness: the server accepts TCP connections on --port once the model
//     is loaded. lever probes with a connect, not an HTTP route.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
)

// ServerArgs is the whisper-server argument list for model on
// 127.0.0.1:port (assumption 1).
func ServerArgs(modelPath string, port int, gpu bool) []string {
	args := []string{"--host", "127.0.0.1", "--port", strconv.Itoa(port), "-m", modelPath}
	if !gpu {
		args = append(args, "--no-gpu")
	}
	return args
}

// ErrServer: whisper-server answered, but not with a transcript.
var ErrServer = errors.New("whisper-server")

// maxAnswer bounds the server's answer that lever reads.
const maxAnswer = 1 << 20

// Request is one transcription: a WAV clip lever has checked, and the
// optional prompt and language from the host config.
type Request struct {
	WAV      []byte
	Prompt   string
	Language string // "" = auto-detect
}

// Transcribe sends r to the whisper-server at addr (host:port) and returns
// its transcript, as the server gave it (assumptions 2 and 3).
func Transcribe(ctx context.Context, client *http.Client, addr string, r Request) (string, error) {
	// The form is built around the clip, not copied with it: the text
	// fields and the file part's header, then the clip itself, then the
	// closing boundary (what multipart.Writer.Close writes).
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/inference", body)
	if err != nil {
		return "", err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
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

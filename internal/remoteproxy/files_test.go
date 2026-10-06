package remoteproxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/chatfiles"
	"github.com/stevegeek/lever/internal/fileledger"
)

// filesCfg: chatConfig (boss; w1 w2 w3; contact c@x message [w1], see [w2])
// plus contact d@x [w1] and files on, 64 bytes, pdf/xlsm/txt.
func filesCfg(t *testing.T, hub *pageHub) (Config, string, string) {
	cfg := chatConfig(t, hub)
	cfg.AllowedUsers = append(cfg.AllowedUsers, "d@x")
	cfg.Contacts["d@x"] = []string{"w1"}
	tree, state := t.TempDir(), t.TempDir()
	cfg.Files = &FilesConfig{Tree: tree, MaxBytes: 64, Extensions: []string{"pdf", "xlsm", "txt"},
		LedgerDir:  filepath.Join(state, "files-ledger"),
		Workspaces: map[string]string{"boss": ".", "w1": "workers/w1", "w2": "workers/w2", "w3": "workers/w3"}}
	return cfg, tree, cfg.Files.LedgerDir
}

// multipartBody is one form with the given parts (field, filename, content).
func multipartBody(t *testing.T, parts ...[3]string) (string, *bytes.Buffer) {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	for _, p := range parts {
		var w io.Writer
		var err error
		if p[1] == "" {
			w, err = mw.CreateFormField(p[0])
		} else {
			w, err = mw.CreateFormFile(p[0], p[1])
		}
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, p[2])
	}
	_ = mw.Close()
	return mw.FormDataContentType(), &b
}

func uploadDo(t *testing.T, h http.Handler, login, agent string, ctype string, body io.Reader, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := proxyRequest("POST", "/lever/api/files/"+agent, body)
	req.Header.Set("Tailscale-User-Login", login)
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Origin", "https://"+testServeHost)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i+1] == "" {
			req.Header.Del(hdr[i])
		} else {
			req.Header.Set(hdr[i], hdr[i+1])
		}
	}
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	return rw
}

func upload(t *testing.T, h http.Handler, login, agent, name, content string, hdr ...string) *httptest.ResponseRecorder {
	ct, b := multipartBody(t, [3]string{"file", name, content})
	return uploadDo(t, h, login, agent, ct, b, hdr...)
}

func TestFilesRoutesAbsentWhenOff(t *testing.T) {
	hub := newPageHub(t)
	h := NewHandler(chatConfig(t, hub))
	for _, m := range []string{"GET", "POST"} {
		if rw := chatDo(h, "c@x", m, "/lever/api/files/w1"); rw.Code != http.StatusNotFound {
			t.Fatalf("%s off = %d", m, rw.Code)
		}
	}
	if len(hub.reached()) != 0 {
		t.Fatalf("the hub was asked: %v", hub.reached())
	}
}

func TestUploadStoresTheFileAndRecord(t *testing.T) {
	hub := newPageHub(t)
	cfg, tree, ledger := filesCfg(t, hub)
	var lines lockedLines
	cfg.Audit = lines.add
	rw := upload(t, NewHandler(cfg), "c@x", "w1", "Report (final).pdf", "%PDF-1.7")
	if rw.Code != http.StatusCreated {
		t.Fatalf("%d %s", rw.Code, rw.Body)
	}
	for k, v := range map[string]string{"Content-Type": "application/json", "Content-Security-Policy": "sandbox", "Cache-Control": "no-store"} {
		if got := rw.Header().Get(k); got != v {
			t.Errorf("%s = %q", k, got)
		}
	}
	dir := filepath.Join(tree, "workers/w1", chatfiles.Dir, "in", chatfiles.Key("c@x"))
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 || !strings.HasSuffix(ents[0].Name(), "-Report _final_.pdf") {
		t.Fatalf("stored %v", ents)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ents[0].Name())); string(b) != "%PDF-1.7" {
		t.Fatalf("content %q", b)
	}
	l, _ := fileledger.Open(ledger)
	recs, _ := l.List("w1")
	if len(recs) != 1 || recs[0].Op != fileledger.OpUpload || recs[0].Login != "c@x" || recs[0].Name != "Report _final_.pdf" || recs[0].Size != 8 {
		t.Fatalf("%+v", recs)
	}
	if !strings.Contains(rw.Body.String(), `"id":"`+recs[0].ID+`"`) {
		t.Fatalf("answer %s", rw.Body)
	}
	all := lines.all()
	if last := all[len(all)-1]; last.Decision != DecisionFileUpload || last.Status != http.StatusCreated {
		t.Fatalf("audit %+v", last)
	}
	if len(hub.reached()) != 0 {
		t.Fatalf("the hub was asked: %v", hub.reached())
	}
}

func TestUploadGate(t *testing.T) {
	hub := newPageHub(t)
	cfg, _, _ := filesCfg(t, hub)
	stale := cfg
	stale.ContactSession = func(string) error { return errors.New("old skill") }
	for name, tc := range map[string]struct {
		cfg          Config
		login, agent string
		hdr          []string
		status       int
		word         string
	}{
		"see-only":          {cfg, "c@x", "w2", nil, 403, "not-allowed"},
		"hidden":            {cfg, "c@x", "w3", nil, 403, "not-allowed"},
		"manager unlisted":  {cfg, "c@x", "boss", nil, 403, "not-allowed"},
		"operator any":      {cfg, chatOp, "w3", nil, 201, ""},
		"operator boss":     {cfg, chatOp, "boss", nil, 201, ""},
		"no origin":         {cfg, "c@x", "w1", []string{"Origin", ""}, 403, "origin"},
		"not fresh":         {stale, "c@x", "w1", nil, 409, "not-fresh"},
		"operator stale ok": {stale, chatOp, "w1", nil, 201, ""},
	} {
		t.Run(name, func(t *testing.T) {
			rw := upload(t, NewHandler(tc.cfg), tc.login, tc.agent, "a.pdf", "x", tc.hdr...)
			if rw.Code != tc.status || tc.word != "" && !strings.Contains(rw.Body.String(), `"error":"`+tc.word+`"`) {
				t.Fatalf("%d %s", rw.Code, rw.Body)
			}
		})
	}
}

func TestUploadFormRules(t *testing.T) {
	hub := newPageHub(t)
	cfg, tree, _ := filesCfg(t, hub)
	h := NewHandler(cfg)
	raw := func(ctype, body string) func() (string, io.Reader) {
		return func() (string, io.Reader) { return ctype, strings.NewReader(body) }
	}
	form := func(parts ...[3]string) func() (string, io.Reader) {
		return func() (string, io.Reader) { return multipartBody(t, parts...) }
	}
	for name, tc := range map[string]struct {
		make   func() (string, io.Reader)
		status int
		word   string
	}{
		"json":        {raw("application/json", `{}`), 415, "bad-form"},
		"no boundary": {raw("multipart/form-data", ""), 415, "bad-form"},
		"field first": {form([3]string{"note", "", "hi"}, [3]string{"file", "a.pdf", "x"}), 400, "bad-form"},
		"wrong field": {form([3]string{"upload", "a.pdf", "x"}), 400, "bad-form"},
		"extension":   {form([3]string{"file", "a.exe", "x"}), 415, "extension"},
	} {
		ctype, body := tc.make()
		rw := uploadDo(t, h, "c@x", "w1", ctype, body)
		if rw.Code != tc.status || !strings.Contains(rw.Body.String(), `"error":"`+tc.word+`"`) {
			t.Errorf("%s: %d %s", name, rw.Code, rw.Body)
		}
	}
	if ents, _ := os.ReadDir(filepath.Join(tree, "workers/w1", chatfiles.Dir, "in", chatfiles.Key("c@x"))); len(ents) != 0 {
		t.Fatalf("left behind: %v", ents)
	}
}

func TestUploadOneFileOnly(t *testing.T) {
	hub := newPageHub(t)
	cfg, tree, ledger := filesCfg(t, hub)
	ct, b := multipartBody(t, [3]string{"file", "a.pdf", "x"}, [3]string{"file", "b.pdf", "y"})
	rw := uploadDo(t, NewHandler(cfg), "c@x", "w1", ct, b)
	if rw.Code != 400 || !strings.Contains(rw.Body.String(), "one-file") {
		t.Fatalf("%d %s", rw.Code, rw.Body)
	}
	if ents, _ := os.ReadDir(filepath.Join(tree, "workers/w1", chatfiles.Dir, "in", chatfiles.Key("c@x"))); len(ents) != 0 {
		t.Fatalf("left behind: %v", ents)
	}
	l, _ := fileledger.Open(ledger)
	if recs, _ := l.List("w1"); len(recs) != 0 {
		t.Fatalf("recorded %+v", recs)
	}
}

func TestUploadSizeLimits(t *testing.T) {
	hub := newPageHub(t)
	cfg, tree, _ := filesCfg(t, hub)
	h := NewHandler(cfg)
	// Over max_bytes inside the part (the body itself fits the allowance).
	if rw := upload(t, h, "c@x", "w1", "a.pdf", strings.Repeat("x", 65)); rw.Code != 413 {
		t.Fatalf("part over max = %d %s", rw.Code, rw.Body)
	}
	// A declared length over the allowance is refused before reading.
	ct, b := multipartBody(t, [3]string{"file", "a.pdf", "x"})
	req := proxyRequest("POST", "/lever/api/files/w1", b)
	req.ContentLength = 64 + 64<<10 + 1
	for k, v := range map[string]string{"Tailscale-User-Login": "c@x", "Content-Type": ct, "Origin": "https://" + testServeHost} {
		req.Header.Set(k, v)
	}
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != 413 {
		t.Fatalf("declared over = %d", rw.Code)
	}
	// A chunked body (no length) far over the allowance stops at it.
	ct, _ = multipartBody(t, [3]string{"file", "a.pdf", "x"})
	big := io.MultiReader(strings.NewReader("--"+strings.TrimPrefix(ct, "multipart/form-data; boundary=")+"\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.pdf\"\r\n\r\n"),
		bytes.NewReader(make([]byte, 1<<20)))
	req = proxyRequest("POST", "/lever/api/files/w1", big)
	req.ContentLength = -1
	for k, v := range map[string]string{"Tailscale-User-Login": "c@x", "Content-Type": ct, "Origin": "https://" + testServeHost} {
		req.Header.Set(k, v)
	}
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != 413 {
		t.Fatalf("chunked over = %d %s", rw.Code, rw.Body)
	}
	if ents, _ := os.ReadDir(filepath.Join(tree, "workers/w1", chatfiles.Dir, "in", chatfiles.Key("c@x"))); len(ents) != 0 {
		t.Fatalf("left behind: %v", ents)
	}
}

func TestUploadNeverFollowsALink(t *testing.T) {
	for name, at := range map[string]string{".lever-files": chatfiles.Dir, "in": chatfiles.Dir + "/in", "key": chatfiles.Dir + "/in/" + chatfiles.Key("c@x")} {
		t.Run(name, func(t *testing.T) {
			hub := newPageHub(t)
			cfg, tree, _ := filesCfg(t, hub)
			outside := t.TempDir()
			link := filepath.Join(tree, "workers/w1", at)
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			rw := upload(t, NewHandler(cfg), "c@x", "w1", "a.pdf", "x")
			if rw.Code != http.StatusConflict || !strings.Contains(rw.Body.String(), "workspace") {
				t.Fatalf("%d %s", rw.Code, rw.Body)
			}
			if ents, _ := os.ReadDir(outside); len(ents) != 0 {
				t.Fatalf("wrote through the link: %v", ents)
			}
		})
	}
}

func TestUploadRateAndQuota(t *testing.T) {
	hub := newPageHub(t)
	cfg, _, _ := filesCfg(t, hub)
	h := NewHandler(cfg).(*gate)
	now := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	h.files.now = func() time.Time { return now }
	for i := 0; i < uploadsPerHour; i++ {
		now = now.Add(time.Second)
		if rw := upload(t, h, "c@x", "w1", fmt.Sprintf("f%d.pdf", i), "x"); rw.Code != 201 {
			t.Fatalf("%d: %d %s", i, rw.Code, rw.Body)
		}
	}
	if rw := upload(t, h, "c@x", "w1", "late.pdf", "x"); rw.Code != 429 || !strings.Contains(rw.Body.String(), `"rate"`) {
		t.Fatalf("31st: %d %s", rw.Code, rw.Body)
	}
	if rw := upload(t, h, "d@x", "w1", "d.pdf", "x"); rw.Code != 201 {
		t.Fatalf("another login has its own rate: %d", rw.Code)
	}
	h.files.bytesPerDay = 10
	now = now.Add(2 * time.Hour)
	if rw := upload(t, h, "d@x", "w1", "q.pdf", strings.Repeat("x", 20)); rw.Code != 429 || !strings.Contains(rw.Body.String(), `"quota"`) {
		t.Fatalf("quota: %d %s", rw.Code, rw.Body)
	}
}

func TestUploadConcurrencyCap(t *testing.T) {
	s := newFilesState(FilesConfig{})
	var dones []func()
	for i := 0; i < uploadsPerLogin; i++ {
		d, ok := s.beginUpload("c@x")
		if !ok {
			t.Fatal(i)
		}
		dones = append(dones, d)
	}
	if _, ok := s.beginUpload("c@x"); ok {
		t.Fatal("a third upload from one login")
	}
	for i := 0; i < uploadsAtOnce-uploadsPerLogin; i++ {
		d, ok := s.beginUpload(fmt.Sprintf("o%d@x", i))
		if !ok {
			t.Fatal(i)
		}
		dones = append(dones, d)
	}
	if _, ok := s.beginUpload("z@x"); ok {
		t.Fatal("over the total")
	}
	for _, d := range dones {
		d()
	}
	if _, ok := s.beginUpload("c@x"); !ok {
		t.Fatal("slots not given back")
	}
	var wg sync.WaitGroup // race detector: concurrent begin/done
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d, ok := s.beginUpload("r@x"); ok {
				d()
			}
		}()
	}
	wg.Wait()
}

func TestFileListPerLogin(t *testing.T) {
	hub := newPageHub(t)
	cfg, _, _ := filesCfg(t, hub)
	h := NewHandler(cfg)
	for _, u := range [][2]string{{"c@x", "c.pdf"}, {"d@x", "d.pdf"}, {chatOp, "op.pdf"}} {
		if rw := upload(t, h, u[0], "w1", u[1], "x"); rw.Code != 201 {
			t.Fatal(u, rw.Code, rw.Body)
		}
	}
	rw := chatDo(h, "c@x", "GET", "/lever/api/files/w1")
	if rw.Code != 200 || !strings.Contains(rw.Body.String(), `"name":"c.pdf"`) || strings.Contains(rw.Body.String(), "d.pdf") ||
		strings.Contains(rw.Body.String(), "op.pdf") || !strings.Contains(rw.Body.String(), `"direction":"sent"`) {
		t.Fatalf("%d %s", rw.Code, rw.Body)
	}
	if rw := chatDo(h, "c@x", "GET", "/lever/api/files/w2"); rw.Code != 403 {
		t.Fatalf("see-only list = %d", rw.Code)
	}
	if rw := chatDo(h, "c@x", "GET", "/lever/api/files/w1/zz"); rw.Code != 404 {
		t.Fatalf("bad id = %d", rw.Code)
	}
	if rw := chatDo(h, "c@x", "GET", "/lever/api/files/W1"); rw.Code != 404 {
		t.Fatalf("bad agent = %d", rw.Code)
	}
}

func TestAgentsAnswerCarriesFilesLimits(t *testing.T) {
	hub := newPageHub(t)
	cfg, _, _ := filesCfg(t, hub)
	rw := chatDo(NewHandler(cfg), "c@x", "GET", "/lever/api/agents")
	if !strings.Contains(rw.Body.String(), `"files":{"maxBytes":64,"extensions":["pdf","xlsm","txt"]}`) {
		t.Fatalf("%s", rw.Body)
	}
	rw = chatDo(NewHandler(chatConfig(t, hub)), "c@x", "GET", "/lever/api/agents")
	if strings.Contains(rw.Body.String(), `"files"`) {
		t.Fatalf("off: %s", rw.Body)
	}
}

func TestContactStillCannotUseHubAttachments(t *testing.T) {
	hub := newPageHub(t)
	cfg, _, _ := filesCfg(t, hub)
	rw := chatDo(NewHandler(cfg), "c@x", "POST", "/api/v1/chat/attachments", "Origin", "https://"+testServeHost)
	if rw.Code != http.StatusForbidden {
		t.Fatalf("%d %s", rw.Code, rw.Body)
	}
}

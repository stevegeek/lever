package remoteproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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
	req.Header.Set(uploadHeader, "1")
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
	for k, v := range map[string]string{"Tailscale-User-Login": "c@x", "Content-Type": ct, "Origin": "https://" + testServeHost, uploadHeader: "1"} {
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
	for k, v := range map[string]string{"Tailscale-User-Login": "c@x", "Content-Type": ct, "Origin": "https://" + testServeHost, uploadHeader: "1"} {
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
	take := func(login string, op bool) bool {
		d, ok := s.beginUpload(login, op)
		if ok {
			dones = append(dones, d)
		}
		return ok
	}
	if !take("c@x", false) || !take("c@x", false) {
		t.Fatal("two for one login")
	}
	if take("c@x", false) {
		t.Fatal("a third upload from one login")
	}
	if !take("d@x", false) {
		t.Fatal("a second login")
	}
	if take("e@x", false) {
		t.Fatal("a contact took the operator's slot")
	}
	if !take("op@x", true) {
		t.Fatal("the operator's slot")
	}
	if take("op2@x", true) {
		t.Fatal("over the total")
	}
	for _, d := range dones {
		d()
	}
	if _, ok := s.beginUpload("c@x", false); !ok {
		t.Fatal("slots not given back")
	}
	var wg sync.WaitGroup // race detector: concurrent begin/done
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d, ok := s.beginUpload("r@x", i%2 == 0); ok {
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
	if !strings.Contains(rw.Body.String(), `"files":{"maxBytes":64,"extensions":["pdf","xlsm","txt"],"uploads":true,"shares":true}`) {
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

// shareRec writes content into agent's out/<key of login>/name and records
// it as a share, like the broker's share_file.
func shareRec(t *testing.T, cfg Config, agent, login, name, content string) (string, string) {
	t.Helper()
	ws := cfg.Files.Workspaces[agent]
	st, err := chatfiles.Store(cfg.Files.Tree, chatfiles.OutDir(ws, login), name, strings.NewReader(content), cfg.Files.MaxBytes, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	l, _ := fileledger.Open(cfg.Files.LedgerDir)
	id, _ := fileledger.NewID()
	if err := l.Add(fileledger.Record{V: 1, Op: fileledger.OpShare, ID: id, Agent: agent, Login: login, Name: name, Rel: st.Rel,
		SHA256: st.SHA256, Size: st.Size, At: time.Now()}, nil); err != nil {
		t.Fatal(err)
	}
	return id, filepath.Join(cfg.Files.Tree, st.Rel)
}

func TestDownloadHeaders(t *testing.T) {
	hub := newPageHub(t)
	cfg, _, _ := filesCfg(t, hub)
	html := "<html><script>alert(1)</script></html>"
	id, _ := shareRec(t, cfg, "w1", "c@x", "v3 final.xlsm", html)
	var lines lockedLines
	cfg.Audit = lines.add
	rw := chatDo(NewHandler(cfg), "c@x", "GET", "/lever/api/files/w1/"+id)
	if rw.Code != 200 || rw.Body.String() != html {
		t.Fatalf("%d %q", rw.Code, rw.Body)
	}
	for k, v := range map[string]string{
		"Content-Type":                 "application/octet-stream",
		"Content-Disposition":          `attachment; filename="v3 final.xlsm"; filename*=UTF-8''v3%20final.xlsm`,
		"X-Content-Type-Options":       "nosniff",
		"Content-Security-Policy":      "sandbox",
		"Cache-Control":                "no-store",
		"X-Frame-Options":              "DENY",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Content-Length":               strconv.Itoa(len(html)),
	} {
		if got := rw.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	all := lines.all()
	if last := all[len(all)-1]; last.Decision != DecisionFileDownload || last.Status != 200 {
		t.Fatalf("audit %+v", last)
	}
	if strings.Contains(fmt.Sprint(all), "alert") {
		t.Fatal("file content in the audit")
	}
}

func TestDownloadRefusesSwappedBytes(t *testing.T) {
	hub := newPageHub(t)
	cfg, _, _ := filesCfg(t, hub)
	h := NewHandler(cfg)
	for name, tc := range map[string]struct {
		swap   func(t *testing.T, p string)
		status int
		word   string
	}{
		"same size": {func(t *testing.T, p string) { _ = os.WriteFile(p, []byte("SWAPPED!"), 0o644) }, 409, "changed"},
		"appended": {func(t *testing.T, p string) {
			f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
			_, _ = f.WriteString("x")
			f.Close()
		}, 409, "changed"},
		"link":    {func(t *testing.T, p string) { _ = os.Remove(p); _ = os.Symlink("/etc/hosts", p) }, 409, "changed"},
		"deleted": {func(t *testing.T, p string) { _ = os.Remove(p) }, 410, "gone"},
	} {
		t.Run(name, func(t *testing.T) {
			id, p := shareRec(t, cfg, "w1", "c@x", "v.xlsm", "ORIGINAL")
			tc.swap(t, p)
			rw := chatDo(h, "c@x", "GET", "/lever/api/files/w1/"+id)
			if rw.Code != tc.status || !strings.Contains(rw.Body.String(), tc.word) || strings.Contains(rw.Body.String(), "SWAPPED") {
				t.Fatalf("%d %s", rw.Code, rw.Body)
			}
		})
	}
}

func TestDownloadCrossLogin(t *testing.T) {
	hub := newPageHub(t)
	cfg, _, _ := filesCfg(t, hub)
	h := NewHandler(cfg)
	id, _ := shareRec(t, cfg, "w1", "c@x", "c.xlsm", "for c")
	if rw := chatDo(h, "d@x", "GET", "/lever/api/files/w1/"+id); rw.Code != 404 {
		t.Fatalf("another contact = %d %s", rw.Code, rw.Body)
	}
	unknown := strings.Repeat("0", 32)
	a := chatDo(h, "d@x", "GET", "/lever/api/files/w1/"+id)
	b := chatDo(h, "d@x", "GET", "/lever/api/files/w1/"+unknown)
	if a.Code != b.Code || a.Body.String() != b.Body.String() {
		t.Fatal("a foreign id and an unknown id must answer the same")
	}
	if rw := chatDo(h, "c@x", "GET", "/lever/api/files/w2/"+id); rw.Code != 403 {
		t.Fatalf("through a see-only agent = %d", rw.Code)
	}
	if rw := chatDo(h, "c@x", "GET", "/lever/api/files/boss/"+id); rw.Code != 403 {
		t.Fatalf("through an unlisted agent = %d", rw.Code)
	}
	if rw := chatDo(h, chatOp, "GET", "/lever/api/files/w1/"+id); rw.Code != 200 || rw.Body.String() != "for c" {
		t.Fatalf("operator (spec 5 view) = %d", rw.Code)
	}
}

func TestDownloadMethods(t *testing.T) {
	hub := newPageHub(t)
	cfg, _, _ := filesCfg(t, hub)
	h := NewHandler(cfg)
	id, _ := shareRec(t, cfg, "w1", "c@x", "c.xlsm", "x")
	for _, m := range []string{"POST", "HEAD", "DELETE", "PUT"} {
		if rw := chatDo(h, "c@x", m, "/lever/api/files/w1/"+id, "Origin", "https://"+testServeHost); rw.Code != 405 {
			t.Errorf("%s = %d", m, rw.Code)
		}
	}
}

func TestDownloadOwnUpload(t *testing.T) {
	hub := newPageHub(t)
	cfg, _, _ := filesCfg(t, hub)
	h := NewHandler(cfg)
	rw := upload(t, h, "c@x", "w1", "mine.pdf", "%PDF")
	var ans struct{ ID string }
	_ = json.Unmarshal(rw.Body.Bytes(), &ans)
	if got := chatDo(h, "c@x", "GET", "/lever/api/files/w1/"+ans.ID); got.Code != 200 || got.Body.String() != "%PDF" {
		t.Fatalf("%d %s", got.Code, got.Body)
	}
}

func TestDownloadRefusesAHardLink(t *testing.T) {
	hub := newPageHub(t)
	cfg, _, _ := filesCfg(t, hub)
	id, p := shareRec(t, cfg, "w1", "c@x", "v.xlsm", "ORIGINAL")
	if err := os.Link(p, p+".2"); err != nil {
		t.Fatal(err)
	}
	if rw := chatDo(NewHandler(cfg), "c@x", "GET", "/lever/api/files/w1/"+id); rw.Code != 409 || !strings.Contains(rw.Body.String(), "changed") {
		t.Fatalf("%d %s", rw.Code, rw.Body)
	}
}

func TestContentDisposition(t *testing.T) {
	for name, want := range map[string]string{
		"a.pdf":      `attachment; filename="a.pdf"; filename*=UTF-8''a.pdf`,
		"a b-c.xlsm": `attachment; filename="a b-c.xlsm"; filename*=UTF-8''a%20b-c.xlsm`,
		"_CON.pdf":   `attachment; filename="_CON.pdf"; filename*=UTF-8''_CON.pdf`,
	} {
		if got := contentDisposition(name); got != want {
			t.Errorf("%q: %s", name, got)
		}
	}
}

// A ledger line naming a file outside the login's own directory (written by
// anything but lever) is not served.
func TestDownloadRefusesARecordOutsideItsDirectory(t *testing.T) {
	hub := newPageHub(t)
	cfg, _, _ := filesCfg(t, hub)
	st, err := chatfiles.Store(cfg.Files.Tree, chatfiles.OutDir("workers/w1", "d@x"), "d.xlsm", strings.NewReader("for d"), 64, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	l, _ := fileledger.Open(cfg.Files.LedgerDir)
	id, _ := fileledger.NewID()
	_ = l.Add(fileledger.Record{V: 1, Op: fileledger.OpShare, ID: id, Agent: "w1", Login: "c@x", Name: "d.xlsm", Rel: st.Rel,
		SHA256: st.SHA256, Size: st.Size, At: time.Now()}, nil)
	if rw := chatDo(NewHandler(cfg), "c@x", "GET", "/lever/api/files/w1/"+id); rw.Code != 404 {
		t.Fatalf("%d %s", rw.Code, rw.Body)
	}
}

func TestContentDispositionRefusesAnUnsanitizedName(t *testing.T) {
	if got := contentDisposition(`a"b.pdf`); got != `attachment; filename="file"; filename*=UTF-8''file` {
		t.Fatal(got)
	}
}

func TestOperatorViewListsAContactsFiles(t *testing.T) {
	hub := newPageHub(t)
	cfg, _, _ := filesCfg(t, hub)
	var lines lockedLines
	cfg.Audit = lines.add
	h := NewHandler(cfg)
	if rw := upload(t, h, "c@x", "w1", "c.pdf", "x"); rw.Code != 201 {
		t.Fatal(rw.Code)
	}
	if rw := upload(t, h, "d@x", "w1", "d.pdf", "x"); rw.Code != 201 {
		t.Fatal(rw.Code)
	}
	id, _ := shareRec(t, cfg, "w1", "c@x", "v3.xlsm", "wb")
	rw := chatDo(h, chatOp, "GET", "/lever/api/contacts/c@x/agents/w1/files")
	body := rw.Body.String()
	if rw.Code != 200 || !strings.Contains(body, `"name":"c.pdf"`) || !strings.Contains(body, `"id":"`+id+`"`) ||
		strings.Contains(body, "d.pdf") || rw.Header().Get("Content-Security-Policy") != "sandbox" {
		t.Fatalf("%d %s", rw.Code, body)
	}
	all := lines.all()
	if last := all[len(all)-1]; last.Decision != DecisionOperatorView || last.Contact != "c@x" || last.Agent != "w1" || last.Count == nil || *last.Count != 2 {
		t.Fatalf("audit %+v", last)
	}
	if rw := chatDo(h, "c@x", "GET", "/lever/api/contacts/c@x/agents/w1/files"); rw.Code != 403 && rw.Code != 404 {
		t.Fatalf("a contact reached the operator view: %d", rw.Code)
	}
	for _, p := range []string{"/lever/api/contacts/c@x/agents/w2/files", "/lever/api/contacts/z@x/agents/w1/files", "/lever/api/contacts/c@x/agents/w1/files/x"} {
		if rw := chatDo(h, chatOp, "GET", p); rw.Code != 404 {
			t.Fatalf("%s = %d", p, rw.Code)
		}
	}
	if rw := chatDo(h, chatOp, "GET", "/lever/api/files/w1/"+id); rw.Code != 200 || rw.Body.String() != "wb" {
		t.Fatalf("the view's link = %d", rw.Code)
	}
	if rw := chatDo(NewHandler(chatConfig(t, hub)), chatOp, "GET", "/lever/api/contacts/c@x/agents/w1/files"); rw.Code != 404 {
		t.Fatalf("files off = %d", rw.Code)
	}
}

func TestUploadNeedsTheUploadHeader(t *testing.T) {
	hub := newPageHub(t)
	cfg, tree, _ := filesCfg(t, hub)
	h := NewHandler(cfg)
	for _, v := range []string{"", "0", "true"} {
		if rw := upload(t, h, "c@x", "w1", "a.pdf", "x", uploadHeader, v); rw.Code != 403 || !strings.Contains(rw.Body.String(), `"origin"`) {
			t.Fatalf("%q: %d %s", v, rw.Code, rw.Body)
		}
	}
	if _, err := os.Stat(filepath.Join(tree, "workers/w1", chatfiles.Dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused upload touched the tree: %v", err)
	}
}

// A refusal under the ledger lock (quota, rate) comes before the copy into
// the tree: nothing of the refused upload is left there.
func TestUploadRefusedByTheLedgerLeavesNothing(t *testing.T) {
	hub := newPageHub(t)
	cfg, tree, _ := filesCfg(t, hub)
	h := NewHandler(cfg).(*gate)
	h.files.bytesPerDay = 10
	if rw := upload(t, h, "c@x", "w1", "q.pdf", strings.Repeat("x", 20)); rw.Code != 429 || !strings.Contains(rw.Body.String(), `"quota"`) {
		t.Fatalf("%d %s", rw.Code, rw.Body)
	}
	if ents, _ := os.ReadDir(filepath.Join(tree, "workers/w1", chatfiles.Dir, "in", chatfiles.Key("c@x"))); len(ents) != 0 {
		t.Fatalf("left behind: %v", ents)
	}
	if _, err := os.Stat(filepath.Join(tree, "workers/w1", chatfiles.Dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refused upload created the exchange: %v", err)
	}
}

func TestUploadBusyThroughTheHandler(t *testing.T) {
	hub := newPageHub(t)
	cfg, tree, _ := filesCfg(t, hub)
	h := NewHandler(cfg).(*gate)
	for i := 0; i < uploadsPerLogin; i++ {
		if _, ok := h.files.beginUpload("c@x", false); !ok {
			t.Fatal(i)
		}
	}
	rw := upload(t, h, "c@x", "w1", "a.pdf", "x")
	if rw.Code != 429 || !strings.Contains(rw.Body.String(), `"busy"`) || rw.Header().Get("Retry-After") == "" {
		t.Fatalf("%d %s", rw.Code, rw.Body)
	}
	if _, err := os.Stat(filepath.Join(tree, "workers/w1", chatfiles.Dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a busy upload touched the tree: %v", err)
	}
	if rw := upload(t, h, "d@x", "w1", "d.pdf", "x"); rw.Code != 201 {
		t.Fatalf("another login = %d", rw.Code)
	}
	// Three slots held (c@x twice, e@x): the last one is the operator's.
	if _, ok := h.files.beginUpload("e@x", false); !ok {
		t.Fatal("a third slot")
	}
	if rw := upload(t, h, "d@x", "w1", "d2.pdf", "x"); rw.Code != 429 || !strings.Contains(rw.Body.String(), `"busy"`) {
		t.Fatalf("a contact on the last slot = %d %s", rw.Code, rw.Body)
	}
	if rw := upload(t, h, chatOp, "w1", "op.pdf", "x"); rw.Code != 201 {
		t.Fatalf("the operator on the last slot = %d %s", rw.Code, rw.Body)
	}
}

// The append fails after the copy (the agent's ledger file cannot be
// written): the copy is removed, in/<key>/ is left empty.
func TestUploadRemovesTheCopyWhenTheRecordFails(t *testing.T) {
	hub := newPageHub(t)
	cfg, tree, ledger := filesCfg(t, hub)
	h := NewHandler(cfg)
	if rw := upload(t, h, "d@x", "w1", "first.pdf", "x"); rw.Code != 201 {
		t.Fatal(rw.Code)
	}
	if err := os.Chmod(filepath.Join(ledger, "w1.jsonl"), 0o400); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Join(ledger, "w1.jsonl"), 0o600)
	rw := upload(t, h, "c@x", "w1", "a.pdf", "x")
	if rw.Code != 503 || !strings.Contains(rw.Body.String(), `"unavailable"`) {
		t.Fatalf("%d %s", rw.Code, rw.Body)
	}
	if ents, _ := os.ReadDir(filepath.Join(tree, "workers/w1", chatfiles.Dir, "in", chatfiles.Key("c@x"))); len(ents) != 0 {
		t.Fatalf("left behind: %v", ents)
	}
}

// The staged file changes before the copy: the copy does not hash to what
// was staged, so it is removed and nothing is recorded.
func TestUploadComparesTheCopyWithTheStagedFile(t *testing.T) {
	hub := newPageHub(t)
	cfg, tree, ledger := filesCfg(t, hub)
	setAfterStage(t, func(f *os.File) {
		_, _ = f.WriteAt([]byte("Z"), 0)
	})
	rw := upload(t, NewHandler(cfg), "c@x", "w1", "a.pdf", "abc")
	if rw.Code != 500 || !strings.Contains(rw.Body.String(), `"failed"`) {
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

// Reservations count: with one upload reserved and not yet recorded, a
// second that would pass only without it is refused.
func TestUploadReservationsCount(t *testing.T) {
	hub := newPageHub(t)
	cfg, _, _ := filesCfg(t, hub)
	h := NewHandler(cfg).(*gate)
	h.files.bytesPerDay = 10
	led, err := h.files.led()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	release, word, err := h.files.reserve(led, "w1", "c@x", now, 6)
	if err != nil || word != "" {
		t.Fatal(word, err)
	}
	if _, word, _ := h.files.reserve(led, "w1", "C@X", now, 6); word != "quota" {
		t.Fatalf("second = %q", word)
	}
	release()
	r2, word, _ := h.files.reserve(led, "w1", "c@x", now, 6)
	if word != "" {
		t.Fatalf("after release = %q", word)
	}
	r2()
	if len(h.files.pending) != 0 {
		t.Fatalf("pending %v", h.files.pending)
	}
}

// Refused attempts count: past uploadTriesPerHour a login gets rate even
// for uploads every other check would refuse.
func TestUploadAttemptsAreLimited(t *testing.T) {
	hub := newPageHub(t)
	cfg, _, _ := filesCfg(t, hub)
	h := NewHandler(cfg)
	for i := 0; i < uploadTriesPerHour; i++ {
		if rw := upload(t, h, "c@x", "w1", "a.exe", "x"); rw.Code != 415 {
			t.Fatalf("%d: %d", i, rw.Code)
		}
	}
	if rw := upload(t, h, "c@x", "w1", "a.pdf", "x"); rw.Code != 429 || !strings.Contains(rw.Body.String(), `"rate"`) {
		t.Fatalf("%d %s", rw.Code, rw.Body)
	}
	if rw := upload(t, h, "d@x", "w1", "a.pdf", "x"); rw.Code != 201 {
		t.Fatalf("another login = %d", rw.Code)
	}
}

func TestListAndDownloadRates(t *testing.T) {
	hub := newPageHub(t)
	cfg, _, _ := filesCfg(t, hub)
	h := NewHandler(cfg).(*gate)
	now := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	h.files.now = func() time.Time { return now }
	for i := 0; i < listsPerMinute; i++ {
		if rw := chatDo(h, "c@x", "GET", "/lever/api/files/w1"); rw.Code != 200 {
			t.Fatal(i, rw.Code)
		}
	}
	if rw := chatDo(h, "c@x", "GET", "/lever/api/files/w1"); rw.Code != 429 || rw.Header().Get("Retry-After") == "" {
		t.Fatalf("list over the rate = %d", rw.Code)
	}
	id, _ := shareRec(t, cfg, "w1", "c@x", "a.xlsm", "x")
	for i := 0; i < downloadsPerMinute; i++ {
		if rw := chatDo(h, "c@x", "GET", "/lever/api/files/w1/"+id); rw.Code != 200 {
			t.Fatal(i, rw.Code)
		}
	}
	if rw := chatDo(h, "c@x", "GET", "/lever/api/files/w1/"+id); rw.Code != 429 {
		t.Fatalf("download over the rate = %d", rw.Code)
	}
	if rw := chatDo(h, chatOp, "GET", "/lever/api/files/w1/"+id); rw.Code != 200 {
		t.Fatalf("another login = %d", rw.Code)
	}
	now = now.Add(time.Minute)
	if rw := chatDo(h, "c@x", "GET", "/lever/api/files/w1/"+id); rw.Code != 200 {
		t.Fatalf("after the window = %d", rw.Code)
	}
}

func TestDownloadSlots(t *testing.T) {
	s := newFilesState(FilesConfig{})
	d1, ok1 := s.beginDownload("c@x", false)
	d2, ok2 := s.beginDownload("c@x", false)
	if !ok1 || !ok2 {
		t.Fatal("two for one login")
	}
	if _, ok := s.beginDownload("c@x", false); ok {
		t.Fatal("a third for one login")
	}
	d3, ok3 := s.beginDownload("d@x", false)
	if !ok3 {
		t.Fatal("a second login")
	}
	if _, ok := s.beginDownload("e@x", false); ok {
		t.Fatal("a contact took the operator's slot")
	}
	d4, ok4 := s.beginDownload("op@x", true)
	if !ok4 {
		t.Fatal("the operator's slot")
	}
	if _, ok := s.beginDownload("op2@x", true); ok {
		t.Fatal("over the total")
	}
	for _, d := range []func(){d1, d2, d3, d4} {
		d()
	}
	if _, ok := s.beginDownload("c@x", false); !ok {
		t.Fatal("slots not given back")
	}
}

func TestDownloadBusyThroughTheHandler(t *testing.T) {
	hub := newPageHub(t)
	cfg, _, _ := filesCfg(t, hub)
	h := NewHandler(cfg).(*gate)
	id, _ := shareRec(t, cfg, "w1", "c@x", "a.xlsm", "x")
	for i := 0; i < downloadsPerLogin; i++ {
		if _, ok := h.files.beginDownload("c@x", false); !ok {
			t.Fatal(i)
		}
	}
	if rw := chatDo(h, "c@x", "GET", "/lever/api/files/w1/"+id); rw.Code != 429 || !strings.Contains(rw.Body.String(), `"busy"`) {
		t.Fatalf("%d %s", rw.Code, rw.Body)
	}
	if rw := chatDo(h, chatOp, "GET", "/lever/api/files/w1/"+id); rw.Code != 200 {
		t.Fatalf("the operator = %d", rw.Code)
	}
}

// setAfterStage sets the afterStage seam for one test and restores it.
// It is a package variable: a test that sets it must not call t.Parallel.
func setAfterStage(t *testing.T, f func(*os.File)) {
	t.Helper()
	prev := afterStage
	afterStage = f
	t.Cleanup(func() { afterStage = prev })
}

// A copy that cannot be removed after a refusal is named in the audit
// line (its path, never its content).
func TestUploadAuditsAnOrphanCopy(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root removes from a read-only directory")
	}
	hub := newPageHub(t)
	cfg, tree, _ := filesCfg(t, hub)
	var lines lockedLines
	cfg.Audit = lines.add
	dir := filepath.Join(tree, "workers/w1", chatfiles.Dir, "in", chatfiles.Key("c@x"))
	// The staged file changes (the copy is refused), and the directory
	// cannot be written once the copy is in it (its removal fails).
	setAfterStage(t, func(f *os.File) {
		_, _ = f.WriteAt([]byte("Z"), 0)
	})
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(cfg).(*gate)
	h.files.afterCopy = func() { _ = os.Chmod(dir, 0o500) }
	defer os.Chmod(dir, 0o700)
	rw := upload(t, h, "c@x", "w1", "a.pdf", "secret-bytes")
	if rw.Code != 500 {
		t.Fatalf("%d %s", rw.Code, rw.Body)
	}
	all := lines.all()
	last := all[len(all)-1]
	if !strings.Contains(last.Error, "was not removed") || !strings.Contains(last.Error, "-a.pdf") || strings.Contains(fmt.Sprint(all), "secret-bytes") {
		t.Fatalf("audit %+v", last)
	}
}

func TestUploadsOff(t *testing.T) {
	hub := newPageHub(t)
	cfg, tree, _ := filesCfg(t, hub)
	h := NewHandler(cfg)
	if rw := upload(t, h, "c@x", "w1", "before.pdf", "x"); rw.Code != 201 {
		t.Fatal(rw.Code)
	}
	var ans struct{ ID string }
	cfg.Files.NoUploads = true
	off := NewHandler(cfg)
	rw := upload(t, off, "c@x", "w1", "a.pdf", "x")
	if rw.Code != 403 || !strings.Contains(rw.Body.String(), `"uploads-off"`) {
		t.Fatalf("%d %s", rw.Code, rw.Body)
	}
	if ents, _ := os.ReadDir(filepath.Join(tree, "workers/w1", chatfiles.Dir, "in", chatfiles.Key("c@x"))); len(ents) != 1 {
		t.Fatalf("%v", ents)
	}
	// The upload already made stays listed and downloadable by its owner.
	list := chatDo(off, "c@x", "GET", "/lever/api/files/w1")
	if list.Code != 200 || !strings.Contains(list.Body.String(), "before.pdf") {
		t.Fatalf("%d %s", list.Code, list.Body)
	}
	var body struct{ Files []struct{ ID string } }
	_ = json.Unmarshal(list.Body.Bytes(), &body)
	ans.ID = body.Files[0].ID
	if rw := chatDo(off, "c@x", "GET", "/lever/api/files/w1/"+ans.ID); rw.Code != 200 {
		t.Fatalf("download = %d", rw.Code)
	}
	if a := chatDo(off, "c@x", "GET", "/lever/api/agents"); !strings.Contains(a.Body.String(), `"uploads":false,"shares":true`) {
		t.Fatalf("%s", a.Body)
	}
}

// The uploads-off answer comes before the body is read: a body far over
// the limit still gets uploads-off, not too-large.
func TestUploadsOffBeforeTheBody(t *testing.T) {
	hub := newPageHub(t)
	cfg, _, _ := filesCfg(t, hub)
	cfg.Files.NoUploads = true
	if rw := upload(t, NewHandler(cfg), "c@x", "w1", "a.pdf", strings.Repeat("x", 1000)); rw.Code != 403 || !strings.Contains(rw.Body.String(), `"uploads-off"`) {
		t.Fatalf("%d %s", rw.Code, rw.Body)
	}
}

func TestSharesOffRefusesShareDownloads(t *testing.T) {
	hub := newPageHub(t)
	cfg, _, _ := filesCfg(t, hub)
	id, _ := shareRec(t, cfg, "w1", "c@x", "v.xlsm", "wb")
	if rw := upload(t, NewHandler(cfg), "c@x", "w1", "mine.pdf", "x"); rw.Code != 201 {
		t.Fatal(rw.Code)
	}
	cfg.Files.NoShares = true
	h := NewHandler(cfg)
	list := chatDo(h, "c@x", "GET", "/lever/api/files/w1")
	if !strings.Contains(list.Body.String(), "v.xlsm") || !strings.Contains(list.Body.String(), "mine.pdf") {
		t.Fatalf("the records stay listed: %s", list.Body)
	}
	if rw := chatDo(h, "c@x", "GET", "/lever/api/files/w1/"+id); rw.Code != 403 || !strings.Contains(rw.Body.String(), `"shares-off"`) {
		t.Fatalf("%d %s", rw.Code, rw.Body)
	}
	if rw := chatDo(h, "d@x", "GET", "/lever/api/files/w1/"+id); rw.Code != 404 {
		t.Fatalf("another login's share id still answers like an unknown one: %d", rw.Code)
	}
	var body struct{ Files []struct{ ID, Name string } }
	_ = json.Unmarshal(list.Body.Bytes(), &body)
	for _, f := range body.Files {
		if f.Name == "mine.pdf" {
			if rw := chatDo(h, "c@x", "GET", "/lever/api/files/w1/"+f.ID); rw.Code != 200 {
				t.Fatalf("an upload stays downloadable: %d", rw.Code)
			}
		}
	}
	if a := chatDo(h, "c@x", "GET", "/lever/api/agents"); !strings.Contains(a.Body.String(), `"uploads":true,"shares":false`) {
		t.Fatalf("%s", a.Body)
	}
}

// A login with files: false gets what files off gives: every file route a
// 404 like an unknown path, and no files object in its agents answer.
func TestExcludedLoginHasNoFiles(t *testing.T) {
	hub := newPageHub(t)
	cfg, _, _ := filesCfg(t, hub)
	id, _ := shareRec(t, cfg, "w1", "c@x", "v.xlsm", "wb")
	cfg.Files.Excluded = []string{"C@X"}
	h := NewHandler(cfg)
	offBody := chatDo(NewHandler(chatConfig(t, hub)), "c@x", "GET", "/lever/api/files/w1").Body.String()
	for _, req := range []struct{ method, path string }{{"GET", "/lever/api/files/w1"}, {"GET", "/lever/api/files/w1/" + id}} {
		if rw := chatDo(h, "c@x", req.method, req.path); rw.Code != 404 || rw.Body.String() != offBody {
			t.Fatalf("%s %s = %d %s", req.method, req.path, rw.Code, rw.Body)
		}
	}
	if rw := upload(t, h, "c@x", "w1", "a.pdf", "x"); rw.Code != 404 {
		t.Fatalf("upload = %d %s", rw.Code, rw.Body)
	}
	if a := chatDo(h, "c@x", "GET", "/lever/api/agents"); strings.Contains(a.Body.String(), `"files"`) {
		t.Fatalf("%s", a.Body)
	}
	if rw := upload(t, h, "d@x", "w1", "d.pdf", "x"); rw.Code != 201 {
		t.Fatalf("another login keeps its files: %d", rw.Code)
	}
	// The operator view: the excluded contact's files are a 404, and the
	// contacts list marks it, so the page asks nothing.
	if rw := chatDo(h, chatOp, "GET", "/lever/api/contacts/c@x/agents/w1/files"); rw.Code != 404 {
		t.Fatalf("view = %d", rw.Code)
	}
	if rw := chatDo(h, chatOp, "GET", "/lever/api/contacts"); !strings.Contains(rw.Body.String(), `"login":"c@x","signedIn":false,"agents":[{"name":"w1","label":"","state":"running"}],"noFiles":true`) {
		t.Fatalf("%s", rw.Body)
	}
	// An operator with files: false has none either, nor the view's files.
	cfg.Files.Excluded = []string{chatOp}
	h = NewHandler(cfg)
	if rw := chatDo(h, chatOp, "GET", "/lever/api/files/w1/"+id); rw.Code != 404 {
		t.Fatalf("operator download = %d", rw.Code)
	}
	if rw := chatDo(h, chatOp, "GET", "/lever/api/contacts/c@x/agents/w1/files"); rw.Code != 404 {
		t.Fatalf("operator view = %d", rw.Code)
	}
}

// An operator cannot fetch an excluded contact's record by a known id: the
// same 404 as an unknown id.
func TestOperatorCannotDownloadAnExcludedLoginsRecord(t *testing.T) {
	hub := newPageHub(t)
	cfg, _, _ := filesCfg(t, hub)
	id, _ := shareRec(t, cfg, "w1", "c@x", "v.xlsm", "wb")
	if rw := chatDo(NewHandler(cfg), chatOp, "GET", "/lever/api/files/w1/"+id); rw.Code != 200 {
		t.Fatalf("before = %d", rw.Code)
	}
	cfg.Files.Excluded = []string{"c@x"}
	h := NewHandler(cfg)
	got := chatDo(h, chatOp, "GET", "/lever/api/files/w1/"+id)
	unknown := chatDo(h, chatOp, "GET", "/lever/api/files/w1/"+strings.Repeat("0", 32))
	if got.Code != 404 || got.Body.String() != unknown.Body.String() {
		t.Fatalf("%d %s", got.Code, got.Body)
	}
}

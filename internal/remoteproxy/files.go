package remoteproxy

// Files in the chat (remote.files): /lever/api/files/<agent> lists the
// login's exchange with the agent (GET) and takes one upload (POST);
// /lever/api/files/<agent>/<id> downloads one recorded file (GET).
//
// Uploads are written host-side into the agent's own workspace
// (.lever-files/in/<key>/, package chatfiles) through the no-link walk,
// O_EXCL, and recorded in the files ledger (package fileledger) with the
// sha256 lever computed while writing. A download is served only from a
// private copy whose sha256 equals its record (CopyVerified), as an
// attachment that no browser renders. A login sees and fetches only its
// own uploads and the shares recorded for it; an operator may also fetch
// any record of an agent it may message (spec 5's view links here). The
// hub is never asked; nothing here reaches /api/.

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/chatfiles"
	"github.com/stevegeek/lever/internal/chatledger"
	"github.com/stevegeek/lever/internal/fileledger"
	"github.com/stevegeek/lever/internal/fsutil"
)

// FilesConfig is remote.files as the proxy needs it.
type FilesConfig struct {
	Tree       string
	Workspaces map[string]string // agent name → tree-relative workspace ("." = the manager)
	MaxBytes   int64
	Extensions []string
	// LedgerDir: "" = the state directory is inside the tree; every route
	// answers unavailable.
	LedgerDir string
}

const (
	DecisionFileUpload   Decision = "file-upload"
	DecisionFileDownload Decision = "file-download"
	DecisionDenyFile     Decision = "deny-file"
)

const (
	filesPrefix       = "/lever/api/files/"
	maxFileList       = 200
	uploadsPerHour    = 30      // per login and agent, from the ledger
	uploadBytesPerDay = 1 << 30 // per login and agent, from the ledger
	uploadsAtOnce     = 4
	uploadsPerLogin   = 2
	downloadsAtOnce   = 4
	fileBodyDeadline  = 10 * time.Minute
	// formOverhead is what a one-file form adds around the file: boundary
	// lines and part headers, far below this.
	formOverhead = 64 << 10
)

var fileIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// filesTarget reports whether p is under /lever/api/files: agent (and id,
// for a download) of a well-formed path, or "" for any other path there.
func filesTarget(p string) (agent, id string, under bool) {
	rest, ok := strings.CutPrefix(p, filesPrefix)
	if !ok {
		return "", "", p == strings.TrimSuffix(filesPrefix, "/")
	}
	a, i, hasID := strings.Cut(rest, "/")
	if !agentNameRE.MatchString(a) || hasID && !fileIDRE.MatchString(i) {
		return "", "", true
	}
	return a, i, true
}

// filesState is the proxy's side of the exchange: the config, the ledger
// (opened on first use and again after a failure) and the slots that bound
// uploads and downloads in flight.
type filesState struct {
	cfg         FilesConfig
	now         func() time.Time // tests
	bytesPerDay int64            // uploadBytesPerDay; tests shrink it

	mu        sync.Mutex
	ledger    *fileledger.Ledger
	uploading map[string]int
	total     int
	downloads chan struct{}
}

// newFilesState is the file exchange for cfg, its ledger not yet opened.
func newFilesState(cfg FilesConfig) *filesState {
	return &filesState{cfg: cfg, now: time.Now, bytesPerDay: uploadBytesPerDay,
		uploading: map[string]int{}, downloads: make(chan struct{}, downloadsAtOnce)}
}

// led is the files ledger, opened on first use and again after a failure.
func (s *filesState) led() (*fileledger.Ledger, error) {
	if s.cfg.LedgerDir == "" {
		return nil, errors.New("the files ledger is off: the state directory is inside the tree")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ledger == nil {
		l, err := fileledger.Open(s.cfg.LedgerDir)
		if err != nil {
			return nil, err
		}
		s.ledger = l
	}
	return s.ledger, nil
}

// beginUpload takes an upload slot for login, or reports false when the
// login or the proxy has none free; done gives it back.
func (s *filesState) beginUpload(login string) (func(), bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.total >= uploadsAtOnce || s.uploading[login] >= uploadsPerLogin {
		return nil, false
	}
	s.total++
	s.uploading[login]++
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.total--
		if s.uploading[login]--; s.uploading[login] <= 0 {
			delete(s.uploading, login)
		}
	}, true
}

// uploadLimit is the refusal word for one more upload of size bytes by
// login, from the agent's records ("" = allowed).
func (s *filesState) uploadLimit(prior []fileledger.Record, login string, now time.Time, size int64) string {
	n, total := 0, size
	for _, p := range prior {
		if p.Op != fileledger.OpUpload || !sameLogin(p.Login, login) {
			continue
		}
		if now.Sub(p.At) < time.Hour {
			n++
		}
		if now.Sub(p.At) < 24*time.Hour {
			total += p.Size
		}
	}
	switch {
	case n >= uploadsPerHour:
		return "rate"
	case total > s.bytesPerDay:
		return "quota"
	}
	return ""
}

// fileEntry is one row of a login's exchange with an agent.
type fileEntry struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	At        string `json:"at"`
	Direction string `json:"direction"` // "sent": the login's upload; "received": a share to it
}

// filesFor is login's exchange with agent, newest first, at most
// maxFileList rows: its uploads and the agent's shares to it. Spec 5's
// operator view lists a contact's files with this.
func (g *gate) filesFor(agent, login string) ([]fileEntry, error) {
	led, err := g.files.led()
	if err != nil {
		return nil, err
	}
	recs, err := led.List(agent)
	if err != nil {
		return nil, err
	}
	out := []fileEntry{}
	for i := len(recs) - 1; i >= 0 && len(out) < maxFileList; i-- {
		r := recs[i]
		if !sameLogin(r.Login, login) {
			continue
		}
		dir := "received"
		if r.Op == fileledger.OpUpload {
			dir = "sent"
		}
		out = append(out, fileEntry{ID: r.ID, Name: r.Name, Size: r.Size, At: r.At.UTC().Format(time.RFC3339), Direction: dir})
	}
	return out, nil
}

// mayDownload: a login fetches its own uploads and the shares recorded for
// it; an operator also fetches any record of an agent it may message (the
// caller checked that), which spec 5's operator view links to.
func mayDownload(v viewer, rec fileledger.Record) bool {
	return sameLogin(rec.Login, v.login) || v.tier == chatledger.TierOperator
}

// sameLogin compares two logins as lever does everywhere: lowercased (the
// config's duplicate check, the hub's emails, chatfiles.Key, the broker).
func sameLogin(a, b string) bool { return strings.ToLower(a) == strings.ToLower(b) }

// serveFiles answers every well-formed /lever/api/files/ path: only for an
// agent v may message, so a see-only agent, a hidden one and an unknown
// name get one answer.
func (g *gate) serveFiles(w http.ResponseWriter, r *http.Request, line *AuditLine, v viewer, agent, id string) {
	if !slices.Contains(v.message, agent) {
		// One answer for a see-only agent, a hidden one and a name that
		// does not exist.
		g.refuseFile(w, r, line, http.StatusForbidden, "not-allowed")
		return
	}
	switch {
	case id == "" && r.Method == http.MethodGet:
		g.serveFileList(w, r, line, v, agent)
	case id == "" && r.Method == http.MethodPost:
		g.serveUpload(w, r, line, v, agent)
	case id != "" && r.Method == http.MethodGet:
		g.serveDownload(w, r, line, v, agent, id)
	default:
		if id == "" {
			w.Header().Set("Allow", "GET, POST")
		} else {
			w.Header().Set("Allow", "GET")
		}
		g.refuseFile(w, r, line, http.StatusMethodNotAllowed, "method")
	}
}

// refuseFile answers a file route with one fixed error word.
func (g *gate) refuseFile(w http.ResponseWriter, r *http.Request, line *AuditLine, status int, word string) {
	line.Reason = word
	g.answerFileJSON(w, r, line, DecisionDenyFile, status, map[string]any{"error": word})
}

// answerFileJSON writes a JSON answer of the file routes: inert as a
// document, not stored.
func (g *gate) answerFileJSON(w http.ResponseWriter, r *http.Request, line *AuditLine, decision Decision, status int, body any) {
	b, _ := json.Marshal(body)
	g.answerChat(w, line, decision, status, func() {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Security-Policy", "sandbox")
	}, append(b, '\n'), r)
}

// serveFileList answers GET /lever/api/files/<agent>: v's own exchange.
func (g *gate) serveFileList(w http.ResponseWriter, r *http.Request, line *AuditLine, v viewer, agent string) {
	files, err := g.filesFor(agent, v.login)
	if err != nil {
		line.Error = err.Error()
		g.refuseFile(w, r, line, http.StatusServiceUnavailable, "unavailable")
		return
	}
	g.answerFileJSON(w, r, line, DecisionAllow, http.StatusOK, map[string]any{"files": files})
}

// serveUpload answers POST /lever/api/files/<agent>: one multipart file,
// streamed into the agent's in/<key of v>/ and recorded. The key comes from
// v.login, the configured spelling the allowlist matched.
func (g *gate) serveUpload(w http.ResponseWriter, r *http.Request, line *AuditLine, v viewer, agent string) {
	s := g.files
	if !sameOriginWrite(r) {
		g.refuseFile(w, r, line, http.StatusForbidden, "origin")
		return
	}
	if v.tier == chatledger.TierContact {
		// The same rule as a contact's post: only to an agent whose session
		// started fresh on the current skill (the follow-up chat note would
		// be refused anyway).
		recs, err := g.records(r.Context())
		rec, found := recs[agent]
		state, _ := agentState(rec, found, err != nil)
		if !contactFresh(state, agent, g.cfg.ContactSession) {
			g.refuseFile(w, r, line, http.StatusConflict, "not-fresh")
			return
		}
	}
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		g.refuseFile(w, r, line, http.StatusUnsupportedMediaType, "bad-form")
		return
	}
	limit := s.cfg.MaxBytes + formOverhead
	if r.ContentLength > limit {
		g.refuseFile(w, r, line, http.StatusRequestEntityTooLarge, "too-large")
		return
	}
	ws, ok := s.cfg.Workspaces[agent]
	led, lerr := s.led()
	if !ok || lerr != nil {
		line.Error = filesErrText(lerr)
		g.refuseFile(w, r, line, http.StatusServiceUnavailable, "unavailable")
		return
	}
	now := s.now()
	if prior, err := led.List(agent); err != nil {
		line.Error = err.Error()
		g.refuseFile(w, r, line, http.StatusServiceUnavailable, "unavailable")
		return
	} else if word := s.uploadLimit(prior, v.login, now, 0); word != "" {
		g.refuseFile(w, r, line, http.StatusTooManyRequests, word)
		return
	}
	done, ok := s.beginUpload(v.login)
	if !ok {
		w.Header().Set("Retry-After", "30")
		g.refuseFile(w, r, line, http.StatusTooManyRequests, "busy")
		return
	}
	defer done()
	// The server has no ReadTimeout (serve.go): the body gets its own.
	_ = http.NewResponseController(w).SetReadDeadline(now.Add(fileBodyDeadline))
	mr := multipart.NewReader(http.MaxBytesReader(w, r.Body, limit), params["boundary"])
	part, err := mr.NextPart()
	if tooLarge(err) {
		g.refuseFile(w, r, line, http.StatusRequestEntityTooLarge, "too-large")
		return
	}
	if err != nil || part.FormName() != "file" || part.FileName() == "" {
		g.refuseFile(w, r, line, http.StatusBadRequest, "bad-form")
		return
	}
	name := chatfiles.SanitizeName(part.FileName())
	if !chatfiles.ExtAllowed(name, s.cfg.Extensions) {
		g.refuseFile(w, r, line, http.StatusUnsupportedMediaType, "extension")
		return
	}
	st, err := chatfiles.Store(s.cfg.Tree, chatfiles.InDir(ws, v.login), name, part, s.cfg.MaxBytes, now)
	switch {
	case err == nil:
	case errors.Is(err, chatfiles.ErrTooLarge) || tooLarge(err):
		g.refuseFile(w, r, line, http.StatusRequestEntityTooLarge, "too-large")
		return
	case errors.Is(err, chatfiles.ErrBusy):
		w.Header().Set("Retry-After", "1")
		g.refuseFile(w, r, line, http.StatusTooManyRequests, "busy")
		return
	case errors.Is(err, fsutil.ErrSymlink) || errors.Is(err, fsutil.ErrEscapesTree) || errors.Is(err, fsutil.ErrNotRegularFile) || errors.Is(err, fs.ErrExist):
		line.Error = err.Error()
		g.refuseFile(w, r, line, http.StatusConflict, "workspace")
		return
	default:
		line.Error = err.Error()
		g.refuseFile(w, r, line, http.StatusInternalServerError, "failed")
		return
	}
	drop := func() { _ = fsutil.RemoveInTreeNoLinks(s.cfg.Tree, st.Rel) }
	if _, err := mr.NextPart(); err != io.EOF {
		drop()
		if tooLarge(err) {
			g.refuseFile(w, r, line, http.StatusRequestEntityTooLarge, "too-large")
			return
		}
		g.refuseFile(w, r, line, http.StatusBadRequest, "one-file")
		return
	}
	id, err := fileledger.NewID()
	if err != nil {
		drop()
		g.refuseFile(w, r, line, http.StatusServiceUnavailable, "unavailable")
		return
	}
	rec := fileledger.Record{V: 1, Op: fileledger.OpUpload, ID: id, Agent: agent, Login: v.login, Name: name, Rel: st.Rel,
		SHA256: st.SHA256, Size: st.Size, At: now}
	var limitWord string
	err = led.Add(rec, func(prior []fileledger.Record) error {
		if limitWord = s.uploadLimit(prior, v.login, now, st.Size); limitWord != "" {
			return errors.New(limitWord)
		}
		return nil
	})
	if err != nil {
		drop()
		if limitWord != "" {
			g.refuseFile(w, r, line, http.StatusTooManyRequests, limitWord)
			return
		}
		line.Error = err.Error()
		g.refuseFile(w, r, line, http.StatusServiceUnavailable, "unavailable")
		return
	}
	g.answerFileJSON(w, r, line, DecisionFileUpload, http.StatusCreated,
		map[string]any{"id": id, "name": name, "size": st.Size, "sha256": st.SHA256})
}

// tooLarge reports whether err is the body limit (http.MaxBytesReader).
func tooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

// filesErrText is the audit text of a failed workspace or ledger lookup.
func filesErrText(err error) string {
	if err == nil {
		return "no workspace for this agent"
	}
	return err.Error()
}

// serveDownload answers GET /lever/api/files/<agent>/<id>: the recorded
// file, from a private copy whose sha256 equals the record, as an
// attachment no browser renders. Another login's id answers like an
// unknown one.
func (g *gate) serveDownload(w http.ResponseWriter, r *http.Request, line *AuditLine, v viewer, agent, id string) {
	s := g.files
	led, err := s.led()
	if err != nil {
		line.Error = err.Error()
		g.refuseFile(w, r, line, http.StatusServiceUnavailable, "unavailable")
		return
	}
	rec, found, err := led.Find(agent, id)
	if err != nil {
		line.Error = err.Error()
		g.refuseFile(w, r, line, http.StatusServiceUnavailable, "unavailable")
		return
	}
	// One answer for an unknown id, another login's record, and a record
	// whose path is not where lever puts that login's files for this agent
	// (a ledger line written by anything but lever).
	if !found || !mayDownload(v, rec) || !recordedWhereExpected(s.cfg.Workspaces[agent], rec) {
		g.refuseFile(w, r, line, http.StatusNotFound, "not-found")
		return
	}
	select {
	case s.downloads <- struct{}{}:
		defer func() { <-s.downloads }()
	default:
		w.Header().Set("Retry-After", "10")
		g.refuseFile(w, r, line, http.StatusTooManyRequests, "busy")
		return
	}
	f, size, err := chatfiles.CopyVerified(s.cfg.Tree, rec.Rel, rec.SHA256, s.cfg.MaxBytes)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist):
		g.refuseFile(w, r, line, http.StatusGone, "gone")
		return
	case errors.Is(err, chatfiles.ErrChanged), errors.Is(err, fsutil.ErrSymlink), errors.Is(err, fsutil.ErrHardLink),
		errors.Is(err, fsutil.ErrNotRegularFile):
		line.Error = err.Error()
		g.refuseFile(w, r, line, http.StatusConflict, "changed")
		return
	default:
		line.Error = err.Error()
		g.refuseFile(w, r, line, http.StatusServiceUnavailable, "unavailable")
		return
	}
	defer f.Close()
	line.Decision, line.Status = DecisionFileDownload, http.StatusOK
	g.audit(*line)
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", contentDisposition(rec.Name))
	h.Set("Content-Length", strconv.FormatInt(size, 10))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(fileBodyDeadline))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

// recordedWhereExpected reports whether rec's file is directly in the
// directory lever uses for its login and kind in workspace ws: in/<key>/
// for an upload, out/<key>/ for a share.
func recordedWhereExpected(ws string, rec fileledger.Record) bool {
	dir := chatfiles.OutDir(ws, rec.Login)
	if rec.Op == fileledger.OpUpload {
		dir = chatfiles.InDir(ws, rec.Login)
	}
	return ws != "" && path.Dir(rec.Rel) == dir
}

// contentDisposition is the attachment header for a recorded name. Names
// are SanitizeName's ASCII ([A-Za-z0-9._ -]), so the quoted form needs no
// escape (anything else is sent as "file"); filename* is the RFC 6266/5987
// form browsers prefer.
func contentDisposition(name string) string {
	if chatfiles.SanitizeName(name) != name {
		name = "file" // never a quote or a byte the header cannot carry
	}
	return `attachment; filename="` + name + `"; filename*=UTF-8''` + strings.ReplaceAll(url.PathEscape(name), "+", "%2B")
}

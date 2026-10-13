package remoteproxy

// Files in the chat (remote.files): list, upload and download of a login's
// file exchange with an agent.
//
// Uploads are written host-side into the agent's workspace through the
// no-link walk, O_EXCL, and recorded with the sha256 lever computed while
// writing. A download is served only from a private copy whose sha256 equals
// its record, as an attachment no browser renders. A login fetches only its
// own uploads and the shares recorded for it; an operator may also fetch any
// record of an agent it may message. Nothing here reaches the hub.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
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
	// LedgerDir "" (the state directory is inside the tree): every route
	// answers unavailable.
	LedgerDir string
	// NoShares: a share already made is listed but its download is refused.
	NoUploads, NoShares bool
	// Excluded logins (files: false) get the files-off 404 on every route.
	Excluded []string
}

// filesOnFor compares logins lowercased, like every lever login.
func (g *gate) filesOnFor(login string) bool {
	if g.files == nil {
		return false
	}
	for _, x := range g.files.cfg.Excluded {
		if sameLogin(x, login) {
			return false
		}
	}
	return true
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
	// A contact never takes the last download slot: a slow reader cannot hold
	// every slot, and the operator can always fetch.
	downloadsPerLogin = 2
	// Refused attempts count too: each one still reads the ledger or a body.
	listsPerMinute     = 60
	downloadsPerMinute = 20
	uploadTriesPerHour = 2 * uploadsPerHour
	// uploadHeader must be on every upload: a browser sends a custom header
	// across origins only after a CORS preflight, which the proxy never grants,
	// so a cross-site form or a 307/308 resend cannot carry it.
	uploadHeader = "X-Lever-Upload"
	// Upload and download bodies get fileBodyBase plus max_bytes at
	// fileBodyMinRate, at most fileBodyMax.
	fileBodyBase    = 10 * time.Minute
	fileBodyMinRate = 64 << 10
	fileBodyMax     = time.Hour
	formOverhead    = 64 << 10
)

var fileIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// filesTarget returns "" for any malformed path under /lever/api/files.
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

type filesState struct {
	cfg         FilesConfig
	now         func() time.Time // tests
	afterCopy   func()           // tests: runs after the copy into the tree
	bytesPerDay int64            // uploadBytesPerDay; tests shrink it

	mu          sync.Mutex
	ledger      *fileledger.Ledger
	uploading   map[string]int
	total       int
	downloading map[string]int
	downTotal   int
	reserveMu   sync.Mutex
	pending     map[string][]int64 // agent NUL lowercase login → sizes reserved, not yet recorded

	lists, downloads, uploadTries *loginRate
}

func newFilesState(cfg FilesConfig) *filesState {
	return &filesState{cfg: cfg, now: time.Now, bytesPerDay: uploadBytesPerDay,
		uploading: map[string]int{}, downloading: map[string]int{}, pending: map[string][]int64{},
		lists: newLoginRate(listsPerMinute, time.Minute), downloads: newLoginRate(downloadsPerMinute, time.Minute),
		uploadTries: newLoginRate(uploadTriesPerHour, time.Hour)}
}

func (s *filesState) bodyDeadline() time.Duration {
	return min(fileBodyBase+time.Duration(s.cfg.MaxBytes/fileBodyMinRate)*time.Second, fileBodyMax)
}

// timedOut is a slow connection (408 "timeout"), not a malformed form.
func timedOut(err error) bool {
	return errors.Is(err, os.ErrDeadlineExceeded)
}

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

// beginUpload: only an operator takes the last of uploadsAtOnce.
func (s *filesState) beginUpload(login string, operator bool) (func(), bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	free := uploadsAtOnce - s.total
	if free <= 0 || !operator && free <= 1 || s.uploading[login] >= uploadsPerLogin {
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

// reserve checks and reserves in one step under reserveMu, so the copy into
// the tree can run outside the ledger lock and two uploads cannot both pass
// the last slot. reserveMu is taken before the ledger lock, never inside it.
func (s *filesState) reserve(led *fileledger.Ledger, agent, login string, now time.Time, size int64) (release func(), word string, err error) {
	s.reserveMu.Lock()
	defer s.reserveMu.Unlock()
	prior, err := led.List(agent)
	if err != nil {
		return nil, "", err
	}
	key := agent + "\x00" + strings.ToLower(login)
	if word := s.uploadLimit(prior, login, now, size, s.pending[key]); word != "" {
		return nil, word, nil
	}
	s.pending[key] = append(s.pending[key], size)
	return func() {
		s.reserveMu.Lock()
		defer s.reserveMu.Unlock()
		p := s.pending[key]
		if i := slices.Index(p, size); i >= 0 {
			p = slices.Delete(p, i, i+1)
		}
		if len(p) == 0 {
			delete(s.pending, key)
		} else {
			s.pending[key] = p
		}
	}, "", nil
}

// beginDownload: only an operator takes the last of downloadsAtOnce.
func (s *filesState) beginDownload(login string, operator bool) (func(), bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	free := downloadsAtOnce - s.downTotal
	if free <= 0 || !operator && free <= 1 || s.downloading[login] >= downloadsPerLogin {
		return nil, false
	}
	s.downTotal++
	s.downloading[login]++
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.downTotal--
		if s.downloading[login]--; s.downloading[login] <= 0 {
			delete(s.downloading, login)
		}
	}, true
}

type loginRate struct {
	limit  int
	window time.Duration
	mu     sync.Mutex
	hits   map[string][]time.Time
}

func newLoginRate(limit int, window time.Duration) *loginRate {
	return &loginRate{limit: limit, window: window, hits: map[string][]time.Time{}}
}

func (l *loginRate) take(login string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, ts := range l.hits {
		keep := ts[:0]
		for _, t := range ts {
			if now.Sub(t) < l.window {
				keep = append(keep, t)
			}
		}
		if len(keep) == 0 {
			delete(l.hits, k)
		} else {
			l.hits[k] = keep
		}
	}
	key := strings.ToLower(login)
	if len(l.hits[key]) >= l.limit {
		return false
	}
	l.hits[key] = append(l.hits[key], now)
	return true
}

// uploadLimit counts reserved but not yet recorded uploads too ("" = allowed).
func (s *filesState) uploadLimit(prior []fileledger.Record, login string, now time.Time, size int64, pending []int64) string {
	n, total := len(pending), size
	for _, p := range pending {
		total += p
	}
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

type fileEntry struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	At        string `json:"at"`
	Direction string `json:"direction"` // "sent": the login's upload; "received": a share to it
}

// filesFor is newest first, at most maxFileList rows.
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

// mayDownload: an operator also fetches any record of an agent it may
// message; the caller checked that.
func mayDownload(v viewer, rec fileledger.Record) bool {
	return sameLogin(rec.Login, v.login) || v.tier == chatledger.TierOperator
}

// sameLogin compares lowercased, as lever does everywhere (config,
// hub emails, chatfiles.Key, the broker).
func sameLogin(a, b string) bool { return strings.ToLower(a) == strings.ToLower(b) }

// serveFiles answers only for an agent v may message.
func (g *gate) serveFiles(w http.ResponseWriter, r *http.Request, line *AuditLine, v viewer, agent, id string) {
	if !slices.Contains(v.message, agent) {
		// One answer for a see-only agent, a hidden one and an unknown name.
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

func (g *gate) refuseFile(w http.ResponseWriter, r *http.Request, line *AuditLine, status int, word string) {
	line.Reason = word
	g.answerFileJSON(w, r, line, DecisionDenyFile, status, map[string]any{"error": word})
}

// answerFileJSON's answers are inert as a document and not stored.
func (g *gate) answerFileJSON(w http.ResponseWriter, r *http.Request, line *AuditLine, decision Decision, status int, body any) {
	b, _ := json.Marshal(body)
	g.answerChat(w, line, decision, status, func() {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Security-Policy", "sandbox")
	}, append(b, '\n'), r)
}

func (g *gate) serveFileList(w http.ResponseWriter, r *http.Request, line *AuditLine, v viewer, agent string) {
	if !g.files.lists.take(v.login, g.files.now()) {
		w.Header().Set("Retry-After", "60")
		g.refuseFile(w, r, line, http.StatusTooManyRequests, "rate")
		return
	}
	files, err := g.filesFor(agent, v.login)
	if err != nil {
		line.Error = err.Error()
		g.refuseFile(w, r, line, http.StatusServiceUnavailable, "unavailable")
		return
	}
	g.answerFileJSON(w, r, line, DecisionAllow, http.StatusOK, map[string]any{"files": files})
}

// serveUpload: the key comes from v.login, the configured spelling the
// allowlist matched.
func (g *gate) serveUpload(w http.ResponseWriter, r *http.Request, line *AuditLine, v viewer, agent string) {
	s := g.files
	if s.cfg.NoUploads {
		// Before anything reads the body.
		g.refuseFile(w, r, line, http.StatusForbidden, "uploads-off")
		return
	}
	if !sameOriginWrite(r, g.cfg.ServeHost) || r.Header.Get(uploadHeader) != "1" {
		g.refuseFile(w, r, line, http.StatusForbidden, "origin")
		return
	}
	if !s.uploadTries.take(v.login, s.now()) {
		w.Header().Set("Retry-After", "600")
		g.refuseFile(w, r, line, http.StatusTooManyRequests, "rate")
		return
	}
	if v.tier == chatledger.TierContact {
		// Only to an agent whose session started fresh on the current skill, like
		// a contact's post. An unknown state or no record is no pass here.
		recs, err := g.records(r.Context())
		rec, found := recs[agent]
		if found && !validHubID(rec.ID) {
			err = errBadUserID
		}
		if state, _ := agentState(rec, found, err != nil); state == "unknown" {
			line.Error = "the agent's hub state cannot be read"
			w.Header().Set("Retry-After", "30")
			g.refuseFile(w, r, line, http.StatusServiceUnavailable, "try-again")
			return
		}
		if fresh := g.cfg.ContactSession; fresh == nil || fresh(agent) != nil {
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
	} else if word := s.uploadLimit(prior, v.login, now, 0, nil); word != "" {
		g.refuseFile(w, r, line, http.StatusTooManyRequests, word)
		return
	}
	done, ok := s.beginUpload(v.login, v.tier == chatledger.TierOperator)
	if !ok {
		w.Header().Set("Retry-After", "30")
		g.refuseFile(w, r, line, http.StatusTooManyRequests, "busy")
		return
	}
	defer done()
	// The server has no ReadTimeout: the body gets its own.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(s.bodyDeadline()))
	mr := multipart.NewReader(http.MaxBytesReader(w, r.Body, limit), params["boundary"])
	part, err := mr.NextPart()
	if tooLarge(err) {
		g.refuseFile(w, r, line, http.StatusRequestEntityTooLarge, "too-large")
		return
	}
	if timedOut(err) {
		g.refuseFile(w, r, line, http.StatusRequestTimeout, "timeout")
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
	// Staged outside the tree first: nothing reaches the agent's workspace
	// until the whole form was read and every limit checked.
	staged, sha, size, err := chatfiles.Stage(part, s.cfg.MaxBytes)
	if errors.Is(err, chatfiles.ErrTooLarge) || tooLarge(err) {
		g.refuseFile(w, r, line, http.StatusRequestEntityTooLarge, "too-large")
		return
	}
	if errors.Is(err, chatfiles.ErrStage) {
		line.Error = err.Error()
		g.refuseFile(w, r, line, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if timedOut(err) {
		g.refuseFile(w, r, line, http.StatusRequestTimeout, "timeout")
		return
	}
	if err != nil {
		line.Error = err.Error()
		g.refuseFile(w, r, line, http.StatusBadRequest, "bad-form")
		return
	}
	defer staged.Close()
	if afterStage != nil {
		afterStage(staged)
	}
	if _, err := mr.NextPart(); err != io.EOF {
		if tooLarge(err) {
			g.refuseFile(w, r, line, http.StatusRequestEntityTooLarge, "too-large")
			return
		}
		if timedOut(err) {
			g.refuseFile(w, r, line, http.StatusRequestTimeout, "timeout")
			return
		}
		g.refuseFile(w, r, line, http.StatusBadRequest, "one-file")
		return
	}
	id, err := fileledger.NewID()
	if err != nil {
		g.refuseFile(w, r, line, http.StatusServiceUnavailable, "unavailable")
		return
	}
	// Reserve, copy into in/<key>/ (no link, O_EXCL) outside the ledger lock,
	// require the copy's hash and size to match the staged file, then record
	// under the lock with the limits checked again. Any refusal after the copy
	// removes it.
	release, limitWord, err := s.reserve(led, agent, v.login, now, size)
	if err != nil {
		line.Error = err.Error()
		g.refuseFile(w, r, line, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if limitWord != "" {
		g.refuseFile(w, r, line, http.StatusTooManyRequests, limitWord)
		return
	}
	defer release()
	st, err := chatfiles.Store(s.cfg.Tree, chatfiles.InDir(ws, v.login), name, staged, s.cfg.MaxBytes, now)
	if s.afterCopy != nil {
		s.afterCopy()
	}
	if err == nil && (st.SHA256 != sha || st.Size != size) {
		err = errStagedChanged
	}
	if err == nil {
		err = led.Add(fileledger.Record{V: 1, Op: fileledger.OpUpload, ID: id, Agent: agent, Login: v.login, Name: name, Rel: st.Rel,
			SHA256: sha, Size: size, At: now}, func(prior []fileledger.Record) error {
			if limitWord = s.uploadLimit(prior, v.login, now, size, nil); limitWord != "" {
				return errors.New(limitWord)
			}
			return nil
		})
	}
	if err != nil && st.Rel != "" {
		if rmErr := fsutil.RemoveInTreeNoLinks(s.cfg.Tree, st.Rel); rmErr != nil {
			// Name the copy left in the tree (path, never content) so the operator
			// can remove it.
			err = fmt.Errorf("%w; the copy %s was not removed: %v", err, st.Rel, rmErr)
			line.Error = err.Error()
		}
	}
	switch {
	case err == nil:
	case limitWord != "":
		g.refuseFile(w, r, line, http.StatusTooManyRequests, limitWord)
		return
	case errors.Is(err, chatfiles.ErrBusy):
		w.Header().Set("Retry-After", "1")
		g.refuseFile(w, r, line, http.StatusTooManyRequests, "busy")
		return
	case errors.Is(err, fsutil.ErrSymlink) || errors.Is(err, fsutil.ErrEscapesTree) || errors.Is(err, fsutil.ErrNotRegularFile) || errors.Is(err, fs.ErrExist):
		line.Error = err.Error()
		g.refuseFile(w, r, line, http.StatusConflict, "workspace")
		return
	case errors.Is(err, errStagedChanged):
		line.Error = err.Error()
		g.refuseFile(w, r, line, http.StatusInternalServerError, "failed")
		return
	default:
		line.Error = err.Error()
		g.refuseFile(w, r, line, http.StatusServiceUnavailable, "unavailable")
		return
	}
	g.answerFileJSON(w, r, line, DecisionFileUpload, http.StatusCreated,
		map[string]any{"id": id, "name": name, "size": size, "sha256": sha})
}

// afterStage is a test seam (set only via setAfterStage, never in parallel).
var afterStage func(*os.File)

var errStagedChanged = errors.New("the copy into the tree differs from the staged upload")

func tooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

func filesErrText(err error) string {
	if err == nil {
		return "no workspace for this agent"
	}
	return err.Error()
}

// serveDownload: another login's id answers like an unknown one.
func (g *gate) serveDownload(w http.ResponseWriter, r *http.Request, line *AuditLine, v viewer, agent, id string) {
	s := g.files
	if !s.downloads.take(v.login, s.now()) {
		w.Header().Set("Retry-After", "60")
		g.refuseFile(w, r, line, http.StatusTooManyRequests, "rate")
		return
	}
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
	// One answer for an unknown id, another login's record, and a record not
	// where lever puts that login's files (a ledger line lever did not write).
	// A record of a files: false login is served to no one, the operator
	// included.
	if !found || !mayDownload(v, rec) || !g.filesOnFor(rec.Login) || !recordedWhereExpected(s.cfg.Workspaces[agent], rec) {
		g.refuseFile(w, r, line, http.StatusNotFound, "not-found")
		return
	}
	if rec.Op == fileledger.OpShare && s.cfg.NoShares {
		g.refuseFile(w, r, line, http.StatusForbidden, "shares-off")
		return
	}
	done, ok := s.beginDownload(v.login, v.tier == chatledger.TierOperator)
	if !ok {
		w.Header().Set("Retry-After", "10")
		g.refuseFile(w, r, line, http.StatusTooManyRequests, "busy")
		return
	}
	defer done()
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
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.bodyDeadline()))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

func recordedWhereExpected(ws string, rec fileledger.Record) bool {
	dir := chatfiles.OutDir(ws, rec.Login)
	if rec.Op == fileledger.OpUpload {
		dir = chatfiles.InDir(ws, rec.Login)
	}
	return ws != "" && path.Dir(rec.Rel) == dir
}

// Names are SanitizeName's ASCII, so the quoted filename needs no escape
// (anything else is sent as "file").
func contentDisposition(name string) string {
	if chatfiles.SanitizeName(name) != name {
		name = "file" // never a quote or a byte the header cannot carry
	}
	return `attachment; filename="` + name + `"; filename*=UTF-8''` + strings.ReplaceAll(url.PathEscape(name), "+", "%2B")
}

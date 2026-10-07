package broker

// Files in the chat (remote.files): an agent lists its exchange and
// records a file it shares with a login.
//
// The files live in the agent's own workspace (.lever-files/in|out/<key>/,
// package chatfiles); the record of what lever stored or the agent shared
// is the host ledger (package fileledger), which nothing in the jail can
// write. The caller is the mTLS identity; its workspace comes from config,
// never from the request. A share is accepted only from the caller's own
// out/<key-of-to>/, with no link on the path, and only to a login that may
// message the caller (or an operator). File content is never logged.

import (
	"errors"
	"io/fs"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/chatfiles"
	"github.com/stevegeek/lever/internal/chatledger"
	"github.com/stevegeek/lever/internal/fileledger"
	"github.com/stevegeek/lever/internal/fsutil"
	"github.com/stevegeek/lever/internal/wire"
)

// FilesConfig is remote.files as the broker needs it.
type FilesConfig struct {
	Enabled bool
	// Tree is the host path of the instance tree; Workspaces maps each
	// agent slug to its tree-relative workspace ("." for the manager).
	Tree       string
	Workspaces map[string]string
	MaxBytes   int64
	Extensions []string
	// LedgerDir is the files ledger; "" = the state directory is inside
	// the tree, so nothing can be recorded (every call: unavailable).
	LedgerDir string
	Contacts  []FileContactEntry
	Operators []string
	// NoShares (remote.files.shares false): share_file answers shares-off.
	// A login with files: false is in neither list, so it is no target.
	NoShares bool
}

// FileContactEntry is one contact login and the agents it may message.
type FileContactEntry struct {
	Login  string
	Agents []string
}

const (
	filesCallLimit     = 60 // list + share calls per agent per minute
	filesSharesPerHour = 20 // per agent, from the ledger
	filesBodyLimit     = 8 << 10
	filesListMax       = 200 // newest records of each kind in a list answer
)

const (
	// filesContactRequired: a list call that names no contact.
	filesContactRequired = "contact-required"
	refuseBadPath        = "bad-path"
	refuseNotFound       = "not-found"
	refuseSymlink        = "symlink"
	refuseHardLink       = "hard-link"
	refuseSharesOff      = "shares-off"
	refuseNotFile        = "not-a-file"
	refuseTooLarge       = "too-large"
	refuseExtension      = "extension"
)

// fileRecord opens the files ledger on first use and again after a failure.
type fileRecord struct {
	dir    string
	mu     sync.Mutex
	ledger *fileledger.Ledger
}

func (f *fileRecord) get() (*fileledger.Ledger, error) {
	if f.dir == "" {
		return nil, errors.New("the files ledger is off: the state directory is inside the tree")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ledger == nil {
		l, err := fileledger.Open(f.dir)
		if err != nil {
			return nil, err
		}
		f.ledger = l
	}
	return f.ledger, nil
}

// fileTarget is login as a file target of the agent slug: the configured
// spelling of the login and its tier — operator, or contact when its agents
// list slug — or ok false. Logins compare lowercased, the one fold of the
// config's duplicate check, the hub's emails and chatfiles.Key (a Unicode
// case fold would also match U+017F to "s"), so the record always carries
// the config's spelling.
func (b *Broker) fileTarget(login, slug string) (canon, tier string, ok bool) {
	for _, op := range b.files.Operators {
		if sameLogin(op, login) {
			return op, chatledger.TierOperator, true
		}
	}
	for _, c := range b.files.Contacts {
		if sameLogin(c.Login, login) && slices.Contains(c.Agents, slug) {
			return c.Login, chatledger.TierContact, true
		}
	}
	return "", "", false
}

// sameLogin compares two logins as lever does everywhere: lowercased.
func sameLogin(a, b string) bool { return strings.ToLower(a) == strings.ToLower(b) }

// fileTargets is the entry of login (a target of slug, as fileTarget
// spells it) with its directories as slug's container sees them.
func (b *Broker) fileTargets(slug, ws, login string) []wire.FileContact {
	_, tier, ok := b.fileTarget(login, slug)
	if !ok {
		return []wire.FileContact{}
	}
	return []wire.FileContact{{Login: login, Tier: tier,
		InDir:  chatfiles.ContainerPath(ws, chatfiles.InDir(ws, login)) + "/",
		OutDir: chatfiles.ContainerPath(ws, chatfiles.OutDir(ws, login)) + "/"}}
}

func (b *Broker) handleFilesList(w http.ResponseWriter, r *http.Request) {
	caller, ok := b.requireLiveAgent(w, r, "files", "")
	if !ok {
		return
	}
	now := time.Now()
	if ok, _ := b.filesRate.take(caller, now); !ok {
		writeJSON(w, wire.FilesListResponse{Enabled: b.files.Enabled, Note: refuseRate, Contacts: []wire.FileContact{}, Uploads: []wire.FileInfo{}, Shares: []wire.FileInfo{}})
		return
	}
	var req wire.FilesListRequest
	_ = decodeBody(w, r, filesBodyLimit, &req) // an empty or bad body names no contact
	empty := wire.FilesListResponse{Contacts: []wire.FileContact{}, Uploads: []wire.FileInfo{}, Shares: []wire.FileInfo{}}
	if !b.files.Enabled {
		empty.Note = "files are off on this instance"
		writeJSON(w, empty)
		return
	}
	_, slug, _, _ := b.identity(caller)
	// One login per call, never every login's files at once: the agent asks
	// for the login whose verified message it is answering, and gets only
	// that login's records and directories.
	if req.Contact == "" {
		empty.Enabled, empty.Note = true, filesContactRequired
		b.audit("files", caller, "deny", "list: no contact", "reason", filesContactRequired)
		writeJSON(w, empty)
		return
	}
	contact, _, isTarget := b.fileTarget(req.Contact, slug)
	if !isTarget {
		empty.Enabled, empty.Note = true, refuseNotContact
		b.audit("files", caller, "deny", "list contact="+boundedLogin(req.Contact), "reason", refuseNotContact)
		writeJSON(w, empty)
		return
	}
	ws, wsOK := b.files.Workspaces[slug]
	led, err := b.fileLedger.get()
	if !wsOK || err != nil {
		empty.Enabled, empty.Note = true, refuseUnavailable
		detail := "no workspace for " + slug
		if err != nil {
			detail = err.Error()
		}
		b.audit("files", caller, "error", "list: "+detail)
		writeJSON(w, empty)
		return
	}
	recs, err := led.List(slug)
	if err != nil {
		empty.Enabled, empty.Note = true, refuseUnavailable
		b.audit("files", caller, "error", "list: "+err.Error())
		writeJSON(w, empty)
		return
	}
	out := wire.FilesListResponse{Enabled: true, MaxBytes: b.files.MaxBytes, Extensions: b.files.Extensions,
		Contacts: b.fileTargets(slug, ws, contact), Uploads: []wire.FileInfo{}, Shares: []wire.FileInfo{}}
	for _, rec := range recs {
		if !sameLogin(rec.Login, contact) {
			continue
		}
		info := wire.FileInfo{ID: rec.ID, Login: rec.Login, Name: rec.Name, Size: rec.Size, SHA256: rec.SHA256,
			Path: chatfiles.ContainerPath(ws, rec.Rel), At: rfc(rec.At)}
		if rec.Op == fileledger.OpUpload {
			out.Uploads = append(out.Uploads, info)
		} else {
			out.Shares = append(out.Shares, info)
		}
	}
	out.Uploads, out.Shares = lastN(out.Uploads, filesListMax), lastN(out.Shares, filesListMax)
	b.audit("files", caller, "allow", "list contact="+boundedLogin(contact), "uploads", len(out.Uploads), "shares", len(out.Shares))
	writeJSON(w, out)
}

func lastN[T any](s []T, n int) []T {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

var errShareRate = errors.New(refuseRate)

// afterShareHash, when set, runs between a share's hash and its record: a
// test seam for a call that times out in that window.
var afterShareHash func()

// shareRate is the hourly share limit over an agent's records.
func shareRate(now time.Time) func([]fileledger.Record) error {
	return func(prior []fileledger.Record) error {
		n := 0
		for _, p := range prior {
			if p.Op == fileledger.OpShare && now.Sub(p.At) < time.Hour {
				n++
			}
		}
		if n >= filesSharesPerHour {
			return errShareRate
		}
		return nil
	}
}

func (b *Broker) handleFilesShare(w http.ResponseWriter, r *http.Request) {
	caller, ok := b.requireLiveAgent(w, r, "files", "")
	if !ok {
		return
	}
	now := time.Now()
	var req wire.FileShareRequest
	refuse := func(word, note, detail string) {
		b.audit("files", caller, "deny", "share to="+boundedLogin(req.To)+": "+detail, "reason", word)
		writeJSON(w, wire.FileShareResponse{Reason: word, Note: note})
	}
	if ok, _ := b.filesRate.take(caller, now); !ok {
		refuse(refuseRate, "too many calls this minute", "call rate")
		return
	}
	if err := decodeBody(w, r, filesBodyLimit, &req); err != nil {
		refuse(refuseBadPath, "the request was not valid JSON", "bad body")
		return
	}
	if !b.files.Enabled {
		refuse(refuseOff, "files are off on this instance", "off")
		return
	}
	if b.files.NoShares {
		refuse(refuseSharesOff, "sharing files is off on this instance: tell the login in the chat instead", "shares off")
		return
	}
	_, slug, _, _ := b.identity(caller)
	to, _, ok := b.fileTarget(req.To, slug)
	if !ok {
		refuse(refuseNotContact, "to must be a login from contact_files", "not a target of "+slug)
		return
	}
	ws, wsOK := b.files.Workspaces[slug]
	if !wsOK {
		refuse(refuseUnavailable, "lever has no workspace for you", "no workspace")
		return
	}
	rel, name, err := chatfiles.ShareRel(ws, to, req.Path)
	if err != nil {
		refuse(refuseBadPath, "path must be a file directly in that login's out_dir, named with letters, digits, '.', '_', '-' or spaces", "path")
		return
	}
	if !chatfiles.ExtAllowed(name, b.files.Extensions) {
		refuse(refuseExtension, "that file type is not accepted on this instance", "extension")
		return
	}
	led, err := b.fileLedger.get()
	if err != nil {
		refuse(refuseUnavailable, "the host record is unavailable", "ledger: "+err.Error())
		return
	}
	// A cheap look at the hourly count first, so an agent past it cannot
	// make the host hash max_bytes on every call; Add checks again under
	// the lock, and that check decides.
	if prior, err := led.List(slug); err != nil {
		refuse(refuseUnavailable, "the host record is unavailable", "ledger: "+err.Error())
		return
	} else if shareRate(now)(prior) != nil {
		refuse(refuseRate, "too many shares this hour", "hourly rate")
		return
	}
	// The route runs under a TimeoutHandler: past its bound the agent got
	// 503 and may retry, so a call whose context is done records nothing
	// (a duplicate share would count toward the hourly rate).
	sha, size, err := chatfiles.Hash(r.Context(), b.files.Tree, rel, b.files.MaxBytes)
	if afterShareHash != nil {
		afterShareHash()
	}
	if r.Context().Err() != nil {
		refuse(refuseUnavailable, "the call timed out", "timed out")
		return
	}
	if word := shareFault(err); word != "" {
		refuse(word, "see the skill's share rules", "file: "+word)
		return
	}
	id, err := fileledger.NewID()
	if err != nil {
		refuse(refuseUnavailable, "the host record is unavailable", "id: "+err.Error())
		return
	}
	rec := fileledger.Record{V: 1, Op: fileledger.OpShare, ID: id, Agent: slug, Login: to, Name: name, Rel: rel,
		SHA256: sha, Size: size, At: now}
	err = led.Add(rec, shareRate(now))
	switch {
	case errors.Is(err, errShareRate):
		refuse(refuseRate, "too many shares this hour", "hourly rate")
		return
	case err != nil:
		refuse(refuseUnavailable, "the host record is unavailable", "record: "+err.Error())
		return
	}
	b.audit("files", caller, "allow", "share to="+boundedLogin(req.To)+" name="+name, "id", id, "size", size)
	writeJSON(w, wire.FileShareResponse{OK: true, ID: id, Name: name, SHA256: sha, Size: size})
}

// shareFault maps a Hash error to its refusal word ("" for none). A
// component that exists but is not a directory (out is a file) is a plain
// error from the walk and so unavailable: the agent broke its own exchange.
// A hard link has its own word; a path that leaves the tree is bad-path.
func shareFault(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, fs.ErrNotExist):
		return refuseNotFound
	case errors.Is(err, fsutil.ErrSymlink):
		return refuseSymlink
	case errors.Is(err, fsutil.ErrHardLink):
		return refuseHardLink
	case errors.Is(err, fsutil.ErrEscapesTree):
		return refuseBadPath
	case errors.Is(err, fsutil.ErrNotRegularFile):
		return refuseNotFile
	case errors.Is(err, fsutil.ErrFileTooLarge), errors.Is(err, chatfiles.ErrTooLarge):
		return refuseTooLarge
	}
	return refuseUnavailable
}

// withDefaults fills a zero MaxBytes or empty Extensions (brokerctl always
// passes the effective values; this keeps a test config usable).
func (c FilesConfig) withDefaults() FilesConfig {
	if c.MaxBytes <= 0 {
		c.MaxBytes = 25 << 20
	}
	if len(c.Extensions) == 0 {
		c.Extensions = chatfiles.DefaultExtensions
	}
	return c
}

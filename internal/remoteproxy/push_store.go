package remoteproxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/stevegeek/lever/internal/webpush"
)

// The push store: each login's push subscriptions and, per (login, agent),
// the newest message a push was sent (or decided not to be sent) for. One
// 0600 file the proxy alone writes. A subscription is a way to put a
// notification on a device, so the file is private like the VAPID key, and
// an endpoint belongs to one login only.

const (
	pushKeyFile     = "vapid.key"
	pushStoreFile   = "subscriptions.json"
	pushStatusFile  = "status.json"
	maxSubsPerLogin = 5
	maxPushStore    = 1 << 20
)

type PushSub struct {
	webpush.Subscription
	Created time.Time `json:"created"`
}

// PushMark is the newest row a (login, agent) decision covered.
type PushMark struct {
	ID string    `json:"id"`
	At time.Time `json:"at"`
}

type pushLogin struct {
	Subs []PushSub           `json:"subs"`
	Seen map[string]PushMark `json:"seen,omitempty"`
}

type pushFile struct {
	V      int                   `json:"v"`
	Logins map[string]*pushLogin `json:"logins"`
}

type PushStore struct {
	path string
	now  func() time.Time
	mu   sync.Mutex
	f    pushFile
}

var errBadPushStore = errors.New("the push store is not a lever push store")

// checkPrivateDir: the push directory is this user's alone (owner, mode,
// and not a link).
func checkPrivateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 || ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s: not a private directory of this user (want 0700)", dir)
	}
	return nil
}

// OpenPushStore opens (creating dir 0700) the store and forgets every
// login not in logins: a login removed from allowed_users gets no push.
func OpenPushStore(dir string, logins []string) (*PushStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := checkPrivateDir(dir); err != nil {
		return nil, err
	}
	s := &PushStore{path: filepath.Join(dir, pushStoreFile), now: time.Now, f: pushFile{V: 1, Logins: map[string]*pushLogin{}}}
	b, err := webpush.ReadPrivateFile(s.path, maxPushStore)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if json.Unmarshal(b, &s.f) != nil || s.f.V != 1 || s.f.Logins == nil {
			return nil, fmt.Errorf("%s: %w", s.path, errBadPushStore)
		}
	}
	gone := false
	for l := range s.f.Logins {
		if !slices.Contains(logins, l) || s.f.Logins[l] == nil {
			delete(s.f.Logins, l)
			gone = true
		}
	}
	if gone {
		if err := s.save(s.f); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *PushStore) save(f pushFile) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return webpush.WritePrivateFile(s.path, b)
}

// update applies change to a copy, writes it, and only then keeps it: a
// failed write leaves memory as the file is.
func (s *PushStore) update(change func(f *pushFile)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(s.f)
	if err != nil {
		return err
	}
	var next pushFile
	if err := json.Unmarshal(b, &next); err != nil {
		return err
	}
	if next.Logins == nil {
		next.Logins = map[string]*pushLogin{}
	}
	change(&next)
	if err := s.save(next); err != nil {
		return err
	}
	s.f = next
	return nil
}

func entry(f *pushFile, login string) *pushLogin {
	e := f.Logins[login]
	if e == nil {
		e = &pushLogin{}
		f.Logins[login] = e
	}
	return e
}

// Add stores sub for login: replaces the same endpoint, takes it from any
// other login, and keeps the newest maxSubsPerLogin.
func (s *PushStore) Add(login string, sub webpush.Subscription) error {
	now := s.now().UTC()
	return s.update(func(f *pushFile) {
		for _, e := range f.Logins {
			e.Subs = slices.DeleteFunc(e.Subs, func(x PushSub) bool { return x.Endpoint == sub.Endpoint })
		}
		e := entry(f, login)
		e.Subs = append(e.Subs, PushSub{Subscription: sub, Created: now})
		slices.SortStableFunc(e.Subs, func(a, b PushSub) int { return a.Created.Compare(b.Created) })
		if n := len(e.Subs); n > maxSubsPerLogin {
			e.Subs = e.Subs[n-maxSubsPerLogin:]
		}
	})
}

// Remove drops login's subscription with endpoint; another login's stays.
func (s *PushStore) Remove(login, endpoint string) (bool, error) {
	found := false
	err := s.update(func(f *pushFile) {
		if e := f.Logins[login]; e != nil {
			n := len(e.Subs)
			e.Subs = slices.DeleteFunc(e.Subs, func(x PushSub) bool { return x.Endpoint == endpoint })
			found = len(e.Subs) != n
		}
	})
	return found, err
}

func (s *PushStore) Subs(login string) []PushSub {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.f.Logins[login]; e != nil {
		return slices.Clone(e.Subs)
	}
	return nil
}

// Logins are the logins with at least one subscription, sorted.
func (s *PushStore) Logins() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for l, e := range s.f.Logins {
		if len(e.Subs) > 0 {
			out = append(out, l)
		}
	}
	slices.Sort(out)
	return out
}

func (s *PushStore) Counts() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	for l, e := range s.f.Logins {
		if len(e.Subs) > 0 {
			out[l] = len(e.Subs)
		}
	}
	return out
}

func (s *PushStore) Mark(login, agent string) (PushMark, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.f.Logins[login]; e != nil {
		m, ok := e.Seen[agent]
		return m, ok
	}
	return PushMark{}, false
}

func (s *PushStore) SetMark(login, agent string, m PushMark) error {
	return s.update(func(f *pushFile) {
		e := entry(f, login)
		if e.Seen == nil {
			e.Seen = map[string]PushMark{}
		}
		e.Seen[agent] = PushMark{ID: m.ID, At: m.At.UTC()}
	})
}

// PushStatus is the proxy's last push outcome, for `lever doctor` (another
// process). Host is the endpoint's host only.
type PushStatus struct {
	At        time.Time `json:"at"`
	Result    string    `json:"result"` // started | sent | gone | failed
	Status    int       `json:"status,omitempty"`
	Host      string    `json:"host,omitempty"`
	TestHosts bool      `json:"test_hosts,omitempty"`
}

func WritePushStatus(dir string, st PushStatus) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return webpush.WritePrivateFile(filepath.Join(dir, pushStatusFile), b)
}

// PushSummary is what doctor shows. KeyErr is fs.ErrNotExist (wrapped) while
// no proxy has created the key yet.
type PushSummary struct {
	KeyErr   error
	Subs     map[string]int
	StoreErr error
	Last     *PushStatus
}

// ReadPushSummary reads the push directory the way the proxy does.
func ReadPushSummary(dir string) PushSummary {
	var sum PushSummary
	if err := checkPrivateDir(dir); err != nil {
		sum.KeyErr, sum.StoreErr = err, err
		return sum
	}
	if _, err := webpush.ReadPrivateFile(filepath.Join(dir, pushKeyFile), 128); err != nil {
		sum.KeyErr = err
	}
	b, err := webpush.ReadPrivateFile(filepath.Join(dir, pushStoreFile), maxPushStore)
	switch {
	case errors.Is(err, os.ErrNotExist):
		sum.Subs = map[string]int{}
	case err != nil:
		sum.StoreErr = err
	default:
		var f pushFile
		if json.Unmarshal(b, &f) != nil || f.V != 1 {
			sum.StoreErr = errBadPushStore
			break
		}
		sum.Subs = map[string]int{}
		for l, e := range f.Logins {
			if e != nil && len(e.Subs) > 0 {
				sum.Subs[l] = len(e.Subs)
			}
		}
	}
	if b, err := webpush.ReadPrivateFile(filepath.Join(dir, pushStatusFile), 4096); err == nil {
		var st PushStatus
		if json.Unmarshal(b, &st) == nil {
			sum.Last = &st
		}
	}
	return sum
}

package remoteproxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/webpush"
)

const testP256 = "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"
const testAuth = "BTBZMqHH6r4Tts7J_aSIgg"

func sub(id string) webpush.Subscription {
	return webpush.Subscription{Endpoint: "https://fcm.googleapis.com/fcm/send/" + id, P256DH: testP256, Auth: testAuth}
}

// privDir is a fresh 0700 directory: the store refuses a push directory
// others can read, and t.TempDir is not 0700 on every system.
func privDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.Chmod(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

func openStore(t *testing.T, dir string, logins ...string) *PushStore {
	t.Helper()
	s, err := OpenPushStore(dir, logins)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStorePersists0600(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "push")
	s := openStore(t, dir, "op@x", "c@x")
	if err := s.Add("op@x", sub("a")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMark("op@x", "w1", PushMark{ID: "m1", At: time.Unix(100, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, pushStoreFile): 0o600} {
		fi, err := os.Lstat(p)
		if err != nil || fi.Mode().Perm() != want {
			t.Fatalf("%s: %v %v, want %v", p, fi.Mode(), err, want)
		}
	}
	again := openStore(t, dir, "op@x", "c@x")
	if got := again.Subs("op@x"); len(got) != 1 || got[0].Endpoint != sub("a").Endpoint {
		t.Fatalf("reloaded %+v", got)
	}
	if m, ok := again.Mark("op@x", "w1"); !ok || m.ID != "m1" {
		t.Fatalf("mark %+v %v", m, ok)
	}
}

func TestStoreCapsFivePerLoginOldestFirst(t *testing.T) {
	s := openStore(t, privDir(t), "op@x")
	now := time.Unix(1000, 0)
	s.now = func() time.Time { now = now.Add(time.Second); return now }
	for _, id := range []string{"1", "2", "3", "4", "5", "6"} {
		if err := s.Add("op@x", sub(id)); err != nil {
			t.Fatal(err)
		}
	}
	var ids []string
	for _, x := range s.Subs("op@x") {
		ids = append(ids, strings.TrimPrefix(x.Endpoint, "https://fcm.googleapis.com/fcm/send/"))
	}
	if strings.Join(ids, ",") != "2,3,4,5,6" {
		t.Fatalf("kept %v", ids)
	}
}

func TestStoreEndpointBelongsToOneLogin(t *testing.T) {
	s := openStore(t, privDir(t), "op@x", "c@x")
	s.Add("op@x", sub("shared"))
	s.Add("c@x", sub("shared"))
	if len(s.Subs("op@x")) != 0 || len(s.Subs("c@x")) != 1 {
		t.Fatalf("op %v, c %v: the endpoint must move to the last login", s.Subs("op@x"), s.Subs("c@x"))
	}
	if ok, _ := s.Remove("op@x", sub("shared").Endpoint); ok || len(s.Subs("c@x")) != 1 {
		t.Fatal("a login removed another login's subscription")
	}
	if ok, _ := s.Remove("c@x", sub("shared").Endpoint); !ok || len(s.Subs("c@x")) != 0 {
		t.Fatal("own remove failed")
	}
	if got := s.Logins(); len(got) != 0 {
		t.Fatalf("logins with subscriptions %v", got)
	}
}

func TestStoreForgetsLoginsNoLongerAllowed(t *testing.T) {
	dir := privDir(t)
	s := openStore(t, dir, "op@x", "gone@x")
	s.Add("gone@x", sub("g"))
	s.Add("op@x", sub("o"))
	again := openStore(t, dir, "op@x")
	if len(again.Subs("gone@x")) != 0 || strings.Join(again.Logins(), ",") != "op@x" {
		t.Fatalf("logins %v", again.Logins())
	}
}

func TestStoreRefusesAnUnsafeFile(t *testing.T) {
	dir := privDir(t)
	os.WriteFile(filepath.Join(dir, pushStoreFile), []byte(`{"v":1,"logins":{}}`), 0o644)
	if _, err := OpenPushStore(dir, []string{"op@x"}); err == nil {
		t.Fatal("a 0644 store was loaded")
	}
	dir2 := privDir(t)
	os.WriteFile(filepath.Join(dir2, pushStoreFile), []byte(`not json`), 0o600)
	if _, err := OpenPushStore(dir2, []string{"op@x"}); err == nil {
		t.Fatal("a broken store was loaded")
	}
	dir3 := privDir(t)
	os.Chmod(dir3, 0o755)
	if _, err := OpenPushStore(dir3, []string{"op@x"}); err == nil {
		t.Fatal("a 0755 push directory was used")
	}
}

func TestStoreFailedWriteChangesNothing(t *testing.T) {
	dir := privDir(t)
	s := openStore(t, dir, "op@x")
	s.Add("op@x", sub("a"))
	os.Chmod(dir, 0o500) // no new temp file: the write fails
	defer os.Chmod(dir, 0o700)
	if err := s.Add("op@x", sub("b")); err == nil {
		t.Fatal("write did not fail")
	}
	if len(s.Subs("op@x")) != 1 {
		t.Fatal("memory changed although the file did not")
	}
}

func TestPushSummary(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "push")
	sum := ReadPushSummary(dir)
	if sum.KeyErr == nil {
		t.Fatal("no key yet must be an error (not-exist)")
	}
	s := openStore(t, dir, "op@x", "c@x")
	s.Add("op@x", sub("1"))
	s.Add("op@x", sub("2"))
	s.Add("c@x", sub("3"))
	webpush.LoadOrCreateKey(filepath.Join(dir, pushKeyFile))
	WritePushStatus(dir, PushStatus{At: time.Unix(5, 0).UTC(), Result: "gone", Status: 410, Host: "fcm.googleapis.com"})
	sum = ReadPushSummary(dir)
	if sum.KeyErr != nil || sum.StoreErr != nil || sum.Subs["op@x"] != 2 || sum.Subs["c@x"] != 1 || sum.Last == nil || sum.Last.Result != "gone" {
		t.Fatalf("%+v", sum)
	}
}

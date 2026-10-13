package state

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestHubIdentityRoundTrips(t *testing.T) {
	st := ForConfig(t.TempDir())
	if st.HubAuthKey() != filepath.Join(st.Dir, "hub-auth.key") || st.HubIdentityFile() != filepath.Join(st.Dir, "hub-identity.json") {
		t.Fatalf("paths: %s %s", st.HubAuthKey(), st.HubIdentityFile())
	}
	in := HubIdentity{
		Version: HubIdentityVersion, Mode: HubModeMigrated, AdminID: "a1", ControllerID: "c1", ProjectID: "p1",
		KeyFingerprint: "sha256:00", HubMarker: "r1", Users: map[string]string{"alice@example.com": "u1"},
		Repairs: []string{"R1 restored controller"},
	}
	if err := st.SaveHubIdentity(in); err != nil {
		t.Fatal(err)
	}
	got, found, err := st.LoadHubIdentity()
	if err != nil || !found || !reflect.DeepEqual(got, in) {
		t.Fatalf("load: found=%v err=%v got=%+v", found, err, got)
	}
	fi, err := os.Stat(st.HubIdentityFile())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v %v", fi.Mode().Perm(), err)
	}
	if fi, _ := os.Stat(st.Dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("state dir mode %v", fi.Mode().Perm())
	}
}

func TestHubIdentityKeepsTheNewestRepairs(t *testing.T) {
	st := ForConfig(t.TempDir())
	h := HubIdentity{Version: HubIdentityVersion, Mode: HubModeFresh}
	for i := range 25 {
		h.Repairs = append(h.Repairs, fmt.Sprint(i))
	}
	if err := st.SaveHubIdentity(h); err != nil {
		t.Fatal(err)
	}
	got, _, err := st.LoadHubIdentity()
	if err != nil || len(got.Repairs) != MaxHubIdentityRepairs || got.Repairs[0] != "5" || got.Repairs[19] != "24" {
		t.Fatalf("repairs = %v err=%v", got.Repairs, err)
	}
	if len(h.Repairs) != 25 {
		t.Error("SaveHubIdentity changed the caller's slice length")
	}
}

func TestHubIdentityAbsentCorruptAndRemove(t *testing.T) {
	st := ForConfig(t.TempDir())
	if _, found, err := st.LoadHubIdentity(); found || err != nil {
		t.Fatalf("absent: found=%v err=%v", found, err)
	}
	if err := st.RemoveHubIdentity(); err != nil {
		t.Fatalf("remove absent: %v", err)
	}
	for _, h := range []HubIdentity{{Mode: HubModeFresh}, {Version: HubIdentityVersion}, {Version: HubIdentityVersion, Mode: "Fresh"}} {
		if err := st.SaveHubIdentity(h); err == nil {
			t.Errorf("saved %+v", h)
		}
	}
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"{", "", `{"v":2,"mode":"fresh"}`, `{"v":1,"mode":"other"}`, `{"mode":"fresh"}`} {
		if err := os.WriteFile(st.HubIdentityFile(), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, found, err := st.LoadHubIdentity(); err == nil || found {
			t.Errorf("%q: found=%v err=%v", body, found, err)
		}
	}
	if err := st.RemoveHubIdentity(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(st.HubIdentityFile()); !os.IsNotExist(err) {
		t.Fatalf("still there: %v", err)
	}
}

func TestRemoveRemotePAT(t *testing.T) {
	st := ForConfig(t.TempDir())
	if err := st.RemoveRemotePAT(); err != nil {
		t.Fatalf("absent: %v", err)
	}
	if err := st.SaveRemotePAT("tok"); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveRemotePATRecord(PATRecord{ID: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveControllerPAT("ctl"); err != nil {
		t.Fatal(err)
	}
	if err := st.RemoveRemotePAT(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{st.RemotePAT(), st.RemotePATRecord()} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survives: %v", p, err)
		}
	}
	if _, err := os.Stat(st.ControllerPAT()); err != nil {
		t.Errorf("controller PAT removed: %v", err)
	}
	// Only the record left: still removed.
	if err := st.SaveRemotePATRecord(PATRecord{ID: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := st.RemoveRemotePAT(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(st.RemotePATRecord()); !os.IsNotExist(err) {
		t.Errorf("record survives: %v", err)
	}
}

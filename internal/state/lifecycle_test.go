package state

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestLifecycleStepsRoundTrip(t *testing.T) {
	st := State{Dir: filepath.Join(t.TempDir(), DirName)}
	if st.StepMatches("warm:img", "abc") {
		t.Fatal("no record matches nothing")
	}
	at := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	if err := st.RecordStep("warm:img", "abc", at); err != nil {
		t.Fatal(err)
	}
	if !st.StepMatches("warm:img", "abc") || st.StepMatches("warm:img", "abd") || st.StepMatches("warm:other", "abc") {
		t.Fatal("a step matches only the inputs it recorded")
	}
	if st.StepMatches("warm:img", "") {
		t.Fatal("an empty digest never matches")
	}
	fi, err := os.Stat(st.LifecycleFile())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("record file: %v %v", fi, err)
	}
	if err := st.ForgetStep("warm:img"); err != nil {
		t.Fatal(err)
	}
	if st.StepMatches("warm:img", "abc") {
		t.Fatal("a forgotten step matches nothing")
	}
}

// TestLifecycleMigratesTheGuestBoot: the guest-boot.json of an older lever is
// read until the first write, which takes it over and removes it.
func TestLifecycleMigratesTheGuestBoot(t *testing.T) {
	st := State{Dir: t.TempDir()}
	const id = "0f4c3a9e-7b1d-4c2e-9a8f-5d6e7f8a9b0c"
	if err := os.WriteFile(st.legacyGuestBoot(), []byte(`{"boot_id":"`+id+`","recorded_at":"2026-10-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := st.LoadLifecycle()
	if err != nil || l.GuestBoot == nil || l.GuestBoot.BootID != id {
		t.Fatalf("legacy boot not read: %+v %v", l, err)
	}
	if err := st.RecordStep("x", "y", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(st.legacyGuestBoot()); !os.IsNotExist(err) {
		t.Fatalf("guest-boot.json survived the migration: %v", err)
	}
	l, err = st.LoadLifecycle()
	if err != nil || l.GuestBoot == nil || l.GuestBoot.BootID != id || l.Version != LifecycleVersion {
		t.Fatalf("migrated record: %+v %v", l, err)
	}
}

// TestLifecycleCorruptAndNewer: a corrupt record reads as none and is
// replaced; a newer lever's record is never overwritten.
func TestLifecycleCorruptAndNewer(t *testing.T) {
	st := State{Dir: t.TempDir()}
	if err := os.WriteFile(st.LifecycleFile(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st.StepMatches("x", "y") {
		t.Fatal("a corrupt record matches nothing")
	}
	if err := st.RecordStep("x", "y", time.Now()); err != nil {
		t.Fatalf("a corrupt record is replaced: %v", err)
	}
	if !st.StepMatches("x", "y") {
		t.Fatal("the replacement holds the step")
	}
	newer := []byte(`{"version":99,"steps":{"x":{"inputs":"y"}}}`)
	if err := os.WriteFile(st.LifecycleFile(), newer, 0o600); err != nil {
		t.Fatal(err)
	}
	if st.StepMatches("x", "y") {
		t.Fatal("a newer record is not trusted")
	}
	if err := st.RecordStep("x", "z", time.Now()); err == nil {
		t.Fatal("a newer record must not be overwritten")
	}
	if b, _ := os.ReadFile(st.LifecycleFile()); string(b) != string(newer) {
		t.Fatalf("newer record changed: %s", b)
	}
}

// TestLifecycleConcurrentUpdates: the bring-up's lanes record steps at once
// and none is lost.
func TestLifecycleConcurrentUpdates(t *testing.T) {
	st := State{Dir: t.TempDir()}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := st.RecordStep(string(rune('a'+i)), "v", time.Now()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	l, err := st.LoadLifecycle()
	if err != nil || len(l.Steps) != 8 {
		t.Fatalf("steps = %d, %v", len(l.Steps), err)
	}
}

// TestLifecycleRecordConverge: the last converge is kept beside the steps,
// and a later one replaces it.
func TestLifecycleRecordConverge(t *testing.T) {
	st := State{Dir: t.TempDir()}
	at := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	if err := st.RecordStep("x", "y", at); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordConverge("apply", "c1", "v1", at); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordConverge("up", "c2", "v1", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	l, err := st.LoadLifecycle()
	if err != nil || l.LastConverge == nil || *l.LastConverge != (ConvergeRecord{By: "up", Config: "c2", Version: "v1", At: at.Add(time.Hour)}) {
		t.Fatalf("last converge: %+v %v", l.LastConverge, err)
	}
	if !st.StepMatches("x", "y") {
		t.Fatal("the steps survive a converge record")
	}
}

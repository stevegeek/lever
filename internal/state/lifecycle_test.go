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

// TestLifecycleIgnoresTheOldGuestBoot: a guest-boot.json is not read.
func TestLifecycleIgnoresTheOldGuestBoot(t *testing.T) {
	st := State{Dir: t.TempDir()}
	old := filepath.Join(st.Dir, "guest-boot.json")
	if err := os.WriteFile(old, []byte(`{"boot_id":"0f4c3a9e-7b1d-4c2e-9a8f-5d6e7f8a9b0c"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if l, err := st.LoadLifecycle(); err != nil || l.GuestBoot != nil {
		t.Fatalf("%+v %v", l, err)
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
	if err := st.ForgetStep("x"); err != nil {
		t.Fatalf("forgetting in a newer record is a no-op, not a failure: %v", err)
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

package apply

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// memFingerprints is StepFingerprints in memory.
type memFingerprints struct {
	steps      map[string]string
	failRec    error
	failForget error
}

func (m *memFingerprints) Matches(step, inputs string) bool {
	return inputs != "" && m.steps[step] == inputs
}

func (m *memFingerprints) Record(step, inputs string) error {
	if m.failRec != nil {
		return m.failRec
	}
	if m.steps == nil {
		m.steps = map[string]string{}
	}
	m.steps[step] = inputs
	return nil
}

func (m *memFingerprints) Forget(step string) error {
	if m.failForget != nil {
		return m.failForget
	}
	delete(m.steps, step)
	return nil
}

// TestWarmUpFingerprint: the warm-up of an image the jail already holds is
// skipped only when a finished warm-up of the same inputs is recorded; a load
// forgets the record, and a failed or cut-short warm-up records nothing.
func TestWarmUpFingerprint(t *testing.T) {
	step := Step{Kind: KindLoadImage, Target: "lever.local/w:1"}
	key, inputs := warmFingerprint(step)
	cases := []struct {
		name               string
		loaded, recorded   bool
		warmErr            error
		wantLoad, wantWarm int
		wantRecorded       bool
	}{
		{name: "not loaded, nothing recorded", wantLoad: 1, wantWarm: 1, wantRecorded: true},
		{name: "not loaded, recorded: the load forgets it", recorded: true, wantLoad: 1, wantWarm: 1, wantRecorded: true},
		{name: "loaded, nothing recorded", loaded: true, wantWarm: 1, wantRecorded: true},
		{name: "loaded and recorded: skipped", loaded: true, recorded: true, wantRecorded: true},
		{name: "loaded, warm-up fails", loaded: true, warmErr: errors.New("podman"), wantWarm: 1},
		{name: "not loaded, recorded, warm-up fails", recorded: true, warmErr: errors.New("podman"), wantLoad: 1, wantWarm: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fp := &memFingerprints{}
			if c.recorded {
				fp.steps = map[string]string{key: inputs}
			}
			var loads, warms int
			d := fillDeps(Deps{
				Fingerprints: fp,
				ImageLoaded:  func(context.Context, string) bool { return c.loaded },
				LoadImage:    func(context.Context, string) error { loads++; return nil },
				WarmImage:    func(context.Context, string) error { warms++; return c.warmErr },
			})
			if err := runLoadImage(context.Background(), step, d, nil); err != nil {
				t.Fatal(err)
			}
			_, got := fp.steps[key]
			if loads != c.wantLoad || warms != c.wantWarm || got != c.wantRecorded {
				t.Fatalf("loads/warms/recorded = %d/%d/%v, want %d/%d/%v", loads, warms, got, c.wantLoad, c.wantWarm, c.wantRecorded)
			}
		})
	}
}

// A warm-up record lever cannot forget must not outlive the image it
// covered: the load is not made, so the record cannot vouch for a new image.
func TestWarmUpFingerprintForgetFails(t *testing.T) {
	step := Step{Kind: KindLoadImage, Target: "lever.local/w:1"}
	key, inputs := warmFingerprint(step)
	boom := errors.New("lifecycle.json: read-only file system")
	fp := &memFingerprints{steps: map[string]string{key: inputs}, failForget: boom}
	loads := 0
	d := fillDeps(Deps{
		Fingerprints: fp,
		ImageLoaded:  func(context.Context, string) bool { return false },
		LoadImage:    func(context.Context, string) error { loads++; return nil },
	})
	err := runLoadImage(context.Background(), step, d, nil)
	if !errors.Is(err, boom) || loads != 0 {
		t.Fatalf("err = %v, loads = %d; want the forget failure before any load", err, loads)
	}
}

// TestWarmUpFingerprintInputs: another archive (size or time) or reference
// is another fingerprint, and an archive lever cannot read has none.
func TestWarmUpFingerprintInputs(t *testing.T) {
	dir := t.TempDir()
	tar := filepath.Join(dir, "w.tar")
	if err := os.WriteFile(tar, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := Step{Kind: KindLoadImage, Target: "lever.local/w:1", TarPath: tar}
	_, a := warmFingerprint(s)
	if a == "" {
		t.Fatal("a readable archive has a fingerprint")
	}
	if err := os.WriteFile(tar, []byte("two!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, b := warmFingerprint(s); b == a {
		t.Fatal("a rebuilt archive must change the fingerprint")
	}
	if err := os.Chtimes(tar, time.Unix(1, 0), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	_, c := warmFingerprint(s)
	if err := os.Chtimes(tar, time.Unix(2, 0), time.Unix(2, 0)); err != nil {
		t.Fatal(err)
	}
	if _, d := warmFingerprint(s); d == c {
		t.Fatal("a touched archive must change the fingerprint")
	}
	if _, e := warmFingerprint(Step{Target: "lever.local/other:1", TarPath: tar}); e == c {
		t.Fatal("another reference is another fingerprint")
	}
	if _, f := warmFingerprint(Step{Target: s.Target, TarPath: filepath.Join(dir, "gone.tar")}); f != "" {
		t.Fatal("an archive lever cannot read has no fingerprint")
	}
}

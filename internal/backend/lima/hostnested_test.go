package lima

import (
	"io/fs"
	"strings"
	"testing"
)

func TestCheckHostNested(t *testing.T) {
	files := func(m map[string]string) func(string) ([]byte, error) {
		return func(p string) ([]byte, error) {
			if v, ok := m[p]; ok {
				return []byte(v), nil
			}
			return nil, fs.ErrNotExist
		}
	}
	amd := "/sys/module/kvm_amd/parameters/nested"
	intel := "/sys/module/kvm_intel/parameters/nested"
	for _, c := range []struct {
		name string
		m    map[string]string
		ok   bool
	}{
		{"amd 1", map[string]string{amd: "1\n"}, true},
		{"intel Y", map[string]string{intel: "Y\n"}, true},
		{"intel N", map[string]string{intel: "N\n"}, false},
		{"amd 0", map[string]string{amd: "0\n"}, false},
		{"no kvm module", map[string]string{}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := checkHostNested("amd64", files(c.m))
			if c.ok != (err == nil) {
				t.Fatalf("ok=%v, err=%v", c.ok, err)
			}
			if err != nil && !strings.Contains(err.Error(), "modprobe") {
				t.Fatalf("error must carry the fix: %v", err)
			}
		})
	}
}

func TestCheckHostNestedRefusesOtherArch(t *testing.T) {
	read := func(string) ([]byte, error) { return []byte("1\n"), nil }
	err := checkHostNested("arm64", read)
	if err == nil || !strings.Contains(err.Error(), "only on x86_64") || !strings.Contains(err.Error(), "arm64") || strings.Contains(err.Error(), "modprobe") {
		t.Fatalf("arm64 must get the arch refusal, not the Intel/AMD fix: %v", err)
	}
}

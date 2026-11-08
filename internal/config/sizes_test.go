package config

import (
	"strings"
	"testing"
)

// sizeBytes reads a size as Lima does (go-units RAMInBytes: MB and GB are
// binary), since lever passes memory and disk to Lima unchanged, and it
// refuses a size past int64 instead of wrapping it.
func TestSizeBytes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
		ok   bool
	}{
		{"2GiB", 2 << 30, true},
		{"2GB", 2 << 30, true}, // what Lima allocates for memory: 2GB
		{"1536MiB", 1536 << 20, true},
		{"1536MB", 1536 << 20, true},
		{"8589934591GiB", 8589934591 << 30, true}, // the largest that fits
		{"8589934592GiB", 0, false},               // 2^63: one past
		{"17179869186GiB", 0, false},              // wrapped to 2 GiB before
		{"99999999999999999999GiB", 0, false},     // past int64 before the unit
		{"-2GiB", 0, false},
		{"2.5GiB", 0, false},
		{"2048", 0, false},
		{"2TB", 0, false},
	} {
		got, ok := sizeBytes(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("sizeBytes(%q) = %d, %v; want %d, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestValidateLimaMemorySizes(t *testing.T) {
	for _, tc := range []struct{ mem, wantErr string }{
		{"2GiB", ""},
		{"2GB", ""}, // 2 GiB in the VM: refused as too small before
		{"2048MB", ""},
		{"1GB", "too small"},
		{"17179869186GiB", "invalid"},
	} {
		a := &App{Backend: BackendLima, Memory: tc.mem}
		err := validateLimaKeys(a, "linux")
		if tc.wantErr == "" && err != nil {
			t.Errorf("memory %q: %v", tc.mem, err)
		}
		if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
			t.Errorf("memory %q: err %v, want %q", tc.mem, err, tc.wantErr)
		}
	}
}

// A disk size past int64 would overflow in Lima's parser: refused like memory.
func TestValidateDiskRefusesOverflow(t *testing.T) {
	for _, tc := range []struct {
		disk string
		ok   bool
	}{
		{"", true}, {"24GiB", true}, {"40GB", true}, {"8589934591GiB", true},
		{"17179869186GiB", false}, {"24G", false},
	} {
		err := validateDisk(tc.disk)
		if (err == nil) != tc.ok {
			t.Errorf("disk %q: err %v, want ok=%v", tc.disk, err, tc.ok)
		}
	}
}

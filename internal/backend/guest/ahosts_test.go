package guest

import "testing"

func TestParseAhostsBothFamilies(t *testing.T) {
	v4, v6 := ParseAhosts("" +
		"fd07:b51a:cc66:f0::fe STREAM host.orb.internal\n" +
		"fd07:b51a:cc66:f0::fe DGRAM  \n" +
		"0.250.250.254   STREAM \n" +
		"0.250.250.254   DGRAM  \n")
	if v4 != "0.250.250.254" {
		t.Fatalf("v4=%q", v4)
	}
	if v6 != "fd07:b51a:cc66:f0::fe" {
		t.Fatalf("v6=%q", v6)
	}
}

func TestParseAhostsNoAddresses(t *testing.T) {
	v4, v6 := ParseAhosts("")
	if v4 != "" || v6 != "" {
		t.Fatalf("v4=%q v6=%q, want both empty", v4, v6)
	}
}

func TestParseAhostsIgnoresUnparseableLines(t *testing.T) {
	v4, v6 := ParseAhosts("not-an-ip STREAM\n0.250.250.254 STREAM \n")
	if v4 != "0.250.250.254" {
		t.Fatalf("v4=%q", v4)
	}
	if v6 != "" {
		t.Fatalf("v6=%q, want empty", v6)
	}
}

// With several addresses of a family, the last one listed is returned (the
// doc says so; the chains and their verification depend on the same pick).
func TestParseAhostsReturnsTheLastOfEachFamily(t *testing.T) {
	v4, v6 := ParseAhosts("10.0.0.1 STREAM a\n10.0.0.1 DGRAM\nfd00::1 STREAM\n10.0.0.2 STREAM\nfd00::2 STREAM\n")
	if v4 != "10.0.0.2" || v6 != "fd00::2" {
		t.Fatalf("v4=%q v6=%q, want the last of each family", v4, v6)
	}
}

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

package config

import "testing"

func TestEffectiveGuestKernel(t *testing.T) {
	cases := []struct {
		backend, set string
		nested       bool
		want         string
	}{
		{BackendLima, "", false, GuestKernelGA},
		{BackendLima, "", true, GuestKernelHWE},
		{BackendLima, GuestKernelGA, true, GuestKernelGA},
		{BackendLima, GuestKernelHWE, false, GuestKernelHWE},
		{BackendOrbstack, "", false, ""},
	}
	for _, c := range cases {
		a := &App{Backend: c.backend, GuestKernel: c.set, NestedVirt: c.nested}
		if got := a.EffectiveGuestKernel(); got != c.want {
			t.Errorf("%s set=%q nested=%t: got %q, want %q", c.backend, c.set, c.nested, got, c.want)
		}
	}
}

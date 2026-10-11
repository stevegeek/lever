package ghpush

import (
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

// semverRE is a Semantic Versioning 2.0.0 version with an optional leading v.
var semverRE = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)` +
	`(?:-((?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?` +
	`(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

// numericIDRE is a numeric pre-release identifier. big.Int.SetString alone
// would also take "-1" or "+1", and "-1" is an alphanumeric identifier.
var numericIDRE = regexp.MustCompile(`^[0-9]+$`)

type semver struct {
	core [3]*big.Int
	pre  []string
}

func parseSemver(s string) (semver, error) {
	m := semverRE.FindStringSubmatch(s)
	if m == nil {
		return semver{}, fmt.Errorf("%q is not a semantic version such as v1.2.3", s)
	}
	var v semver
	for i := 0; i < 3; i++ {
		v.core[i], _ = new(big.Int).SetString(m[i+1], 10)
	}
	if m[4] != "" {
		v.pre = strings.Split(m[4], ".")
	}
	return v, nil
}

// compareSemver orders a and b by Semantic Versioning precedence (build
// metadata ignored): -1, 0 or 1.
func compareSemver(a, b semver) int {
	for i := 0; i < 3; i++ {
		if c := a.core[i].Cmp(b.core[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a.pre) == 0 && len(b.pre) == 0:
		return 0
	case len(a.pre) == 0:
		return 1
	case len(b.pre) == 0:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		x, y := a.pre[i], b.pre[i]
		xn, xok := numericID(x)
		yn, yok := numericID(y)
		switch {
		case xok && yok:
			if c := xn.Cmp(yn); c != 0 {
				return c
			}
		case xok:
			return -1
		case yok:
			return 1
		default:
			if c := strings.Compare(x, y); c != 0 {
				return c
			}
		}
	}
	switch {
	case len(a.pre) < len(b.pre):
		return -1
	case len(a.pre) > len(b.pre):
		return 1
	}
	return 0
}

// numericID reads a pre-release identifier as a number when it is made only
// of digits.
func numericID(s string) (*big.Int, bool) {
	if !numericIDRE.MatchString(s) {
		return nil, false
	}
	return new(big.Int).SetString(s, 10)
}

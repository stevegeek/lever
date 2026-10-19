package common

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/stevegeek/lever/internal/backend/guest"
	"github.com/stevegeek/lever/internal/proc"
)

const versionOutMax = 200

// VersionAtLeast runs argv on the host, extracts "major.minor.patch" from its
// stdout with re (three numeric capture groups), and reports whether it is
// >= (major, minor, patch). got is the parsed version, or the raw output on
// parse failure.
func VersionAtLeast(ctx context.Context, r proc.Runner, argv []string, re *regexp.Regexp, major, minor, patch int) (ok bool, got string, err error) {
	name := strings.Join(argv, " ")
	res, err := r.Run(ctx, nil, argv[0], argv[1:]...)
	if err != nil {
		return false, "", fmt.Errorf("%s: %w", name, err)
	}
	m := re.FindStringSubmatch(res.Stdout)
	if len(m) != 4 {
		out := guest.SafeLine(strings.TrimSpace(res.Stdout), versionOutMax)
		return false, out, fmt.Errorf("%s: could not parse version from %q", name, out)
	}
	// m[1],m[2],m[3] are guaranteed digits by the regex.
	vMaj, _ := strconv.Atoi(m[1])
	vMin, _ := strconv.Atoi(m[2])
	vPat, _ := strconv.Atoi(m[3])
	got = fmt.Sprintf("%s.%s.%s", m[1], m[2], m[3])

	switch {
	case vMaj != major:
		return vMaj > major, got, nil
	case vMin != minor:
		return vMin > minor, got, nil
	default:
		return vPat >= patch, got, nil
	}
}

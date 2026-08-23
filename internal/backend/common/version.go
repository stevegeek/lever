package common

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/stevegeek/lever/internal/exec"
)

// VersionAtLeast runs argv on the host, extracts "major.minor.patch" from its
// stdout with re (three numeric capture groups), and reports whether it is
// >= (major, minor, patch). got is the parsed version on success, or the raw
// output on parse failure. Errors are prefixed with the argv so the preflight
// names the tool it probed.
func VersionAtLeast(ctx context.Context, r exec.Runner, argv []string, re *regexp.Regexp, major, minor, patch int) (ok bool, got string, err error) {
	name := strings.Join(argv, " ")
	res, err := r.Run(ctx, nil, argv[0], argv[1:]...)
	if err != nil {
		return false, "", fmt.Errorf("%s: %w", name, err)
	}
	m := re.FindStringSubmatch(res.Stdout)
	if len(m) != 4 {
		return false, strings.TrimSpace(res.Stdout), fmt.Errorf("%s: could not parse version from %q", name, strings.TrimSpace(res.Stdout))
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

package update

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a semantic version, enough of one to order releases.
//
// Hand-rolled rather than pulled in: the updater decides what to execute on the strength
// of this comparison, and a dependency in that position is a dependency in the trust
// path.
type Version struct {
	Major, Minor, Patch int
	Pre                 string // empty for a release, "rc.1" and so on for a prerelease
}

func ParseVersion(s string) (Version, error) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "v"))
	if s == "" {
		return Version{}, fmt.Errorf("empty version")
	}

	var v Version
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		if s[i] == '-' {
			v.Pre = s[i+1:]
			if j := strings.IndexByte(v.Pre, '+'); j >= 0 {
				v.Pre = v.Pre[:j] // build metadata does not affect ordering
			}
		}
		s = s[:i]
	}

	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("%q is not MAJOR.MINOR.PATCH", s)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return Version{}, fmt.Errorf("%q is not a number in %q", p, s)
		}
		switch i {
		case 0:
			v.Major = n
		case 1:
			v.Minor = n
		case 2:
			v.Patch = n
		}
	}
	return v, nil
}

func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	return s
}

// IsPrerelease reports whether this version carries a prerelease suffix.
func (v Version) IsPrerelease() bool { return v.Pre != "" }

// Compare returns -1, 0 or 1. A prerelease sorts below the release it precedes, so
// 1.2.0-rc.1 < 1.2.0 — which is what stops a release candidate looking like an upgrade
// from the version it is a candidate for.
func (v Version) Compare(o Version) int {
	for _, pair := range [][2]int{{v.Major, o.Major}, {v.Minor, o.Minor}, {v.Patch, o.Patch}} {
		switch {
		case pair[0] < pair[1]:
			return -1
		case pair[0] > pair[1]:
			return 1
		}
	}
	switch {
	case v.Pre == o.Pre:
		return 0
	case v.Pre == "":
		return 1
	case o.Pre == "":
		return -1
	}
	return comparePre(v.Pre, o.Pre)
}

// comparePre orders dot-separated prerelease identifiers: numeric ones numerically,
// others lexically, and a numeric identifier below a non-numeric one.
func comparePre(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		an, aerr := strconv.Atoi(as[i])
		bn, berr := strconv.Atoi(bs[i])
		switch {
		case aerr == nil && berr == nil:
			if an != bn {
				return sign(an - bn)
			}
		case aerr == nil:
			return -1
		case berr == nil:
			return 1
		default:
			if as[i] != bs[i] {
				return strings.Compare(as[i], bs[i])
			}
		}
	}
	return sign(len(as) - len(bs))
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

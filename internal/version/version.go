// Package version parses, compares, and matches Ansible Galaxy version
// specifiers such as "1.4.5", ">=1.0.0", "~=1.2", or ">1.0.0,<2.0.0".
//
// It implements the subset of PEP 440 / Python packaging semantics that
// Ansible Galaxy clients rely on, without adding an external dependency.
package version

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a parsed, comparable release version. It supports the common
// forms found in Galaxy tags and collection versions: "1.4.5", "v2.0.2",
// "2020.05.01", "1.0.0-rc1", "1.2.3.post1", "1.0.0a1".
type Version struct {
	original string
	parts    []int
	pre      string // "", "dev", "a", "b", or "rc"
	preNum   int
	post     int // -1 when no post-release suffix is present
}

// Parse parses a version string into a comparable Version.
func Parse(s string) (Version, error) {
	v := Version{original: s, post: -1}

	norm := strings.TrimSpace(s)
	norm = strings.TrimPrefix(norm, "v")
	norm = strings.TrimPrefix(norm, "V")
	if i := strings.IndexAny(norm, "+"); i >= 0 {
		norm = norm[:i]
	}
	if norm == "" {
		return v, fmt.Errorf("empty version")
	}

	for _, seg := range strings.FieldsFunc(norm, func(r rune) bool {
		return r == '.' || r == '-' || r == '_'
	}) {
		if seg == "" {
			continue
		}
		lower := strings.ToLower(seg)

		if _, num, ok := parseMarker(lower, []string{"post", "rev"}); ok {
			v.post = num
			continue
		}
		if pre, num, ok := parsePreRelease(lower); ok {
			v.pre = pre
			v.preNum = num
			continue
		}

		n, err := strconv.Atoi(seg)
		if err == nil {
			v.parts = append(v.parts, n)
			continue
		}

		// Support separator-less forms like "1.0.0rc1" or "2.0post1".
		numPart, rest, ok := trimTrailingMarker(seg)
		if !ok {
			return Version{}, fmt.Errorf("invalid version component %q in %q", seg, s)
		}
		n, err = strconv.Atoi(numPart)
		if err != nil {
			return Version{}, fmt.Errorf("invalid version component %q in %q", seg, s)
		}
		v.parts = append(v.parts, n)
		lowerRest := strings.ToLower(rest)
		if pre, num, ok := parsePreRelease(lowerRest); ok {
			v.pre = pre
			v.preNum = num
			continue
		}
		if _, num, ok := parseMarker(lowerRest, []string{"post", "rev"}); ok {
			v.post = num
			continue
		}
		return Version{}, fmt.Errorf("invalid version suffix %q in %q", rest, s)
	}

	if len(v.parts) == 0 {
		return Version{}, fmt.Errorf("invalid version %q", s)
	}
	return v, nil
}

// parsePreRelease recognizes dev/a/b/rc markers (with optional trailing
// number) and returns the canonical marker name plus its numeric part.
func parsePreRelease(seg string) (string, int, bool) {
	table := map[string]string{
		"dev":     "dev",
		"a":       "a",
		"alpha":   "a",
		"b":       "b",
		"beta":    "b",
		"rc":      "rc",
		"c":       "rc",
		"pre":     "rc",
		"preview": "rc",
	}
	for marker, canonical := range table {
		if seg == marker {
			return canonical, 0, true
		}
		if strings.HasPrefix(seg, marker) {
			if num, err := strconv.Atoi(strings.TrimPrefix(seg, marker)); err == nil {
				return canonical, num, true
			}
			return canonical, 0, true
		}
	}
	return "", 0, false
}

// parseMarker recognizes post-release markers such as "post1" or "rev2".
func parseMarker(seg string, markers []string) (string, int, bool) {
	for _, m := range markers {
		if seg == m {
			return m, 0, true
		}
		if strings.HasPrefix(seg, m) {
			if num, err := strconv.Atoi(strings.TrimPrefix(seg, m)); err == nil {
				return m, num, true
			}
			return m, 0, true
		}
	}
	return "", 0, false
}

// trimTrailingMarker splits a numeric segment that carries a trailing
// marker into its numeric part and marker part (e.g. "0rc1" -> "0", "rc1").
func trimTrailingMarker(seg string) (num, rest string, ok bool) {
	i := 0
	for i < len(seg) && seg[i] >= '0' && seg[i] <= '9' {
		i++
	}
	if i == 0 || i == len(seg) {
		return "", "", false
	}
	return seg[:i], seg[i:], true
}

func preRank(pre string) int {
	switch pre {
	case "dev":
		return 0
	case "a":
		return 1
	case "b":
		return 2
	case "rc":
		return 3
	}
	return 4
}

// Compare returns -1, 0, or 1 depending on whether v is less than, equal
// to, or greater than o. Numeric components with trailing zeros are
// treated as equal (1.0 == 1.0.0).
func (v Version) Compare(o Version) int {
	max := len(v.parts)
	if len(o.parts) > max {
		max = len(o.parts)
	}
	for i := 0; i < max; i++ {
		var x, y int
		if i < len(v.parts) {
			x = v.parts[i]
		}
		if i < len(o.parts) {
			y = o.parts[i]
		}
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
	}

	// A final release is greater than any pre-release of the same number.
	if v.pre == "" && o.pre != "" {
		return 1
	}
	if v.pre != "" && o.pre == "" {
		return -1
	}
	if v.pre != "" && o.pre != "" {
		if r := preRank(v.pre) - preRank(o.pre); r != 0 {
			if r < 0 {
				return -1
			}
			return 1
		}
		if v.preNum < o.preNum {
			return -1
		}
		if v.preNum > o.preNum {
			return 1
		}
	}

	// A post-release is greater than the bare final release.
	if v.post != o.post {
		if v.post < o.post {
			return -1
		}
		return 1
	}
	return 0
}

// Compare compares two version strings. Unparseable versions compare as
// greater than any parseable version so unresolvable entries sort last.
func Compare(a, b string) int {
	va, errA := Parse(a)
	vb, errB := Parse(b)
	if errA != nil && errB != nil {
		return strings.Compare(a, b)
	}
	if errA != nil {
		return 1
	}
	if errB != nil {
		return -1
	}
	return va.Compare(vb)
}

// IsLatest reports whether s means "pick the highest available version".
func IsLatest(s string) bool {
	t := strings.TrimSpace(s)
	return t == "" || strings.EqualFold(t, "latest")
}

// IsConstraint reports whether s is a range specifier (contains comparison
// operators, wildcards, or comma-separated alternatives) rather than an
// exact version or tag name.
func IsConstraint(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" || IsLatest(t) {
		return false
	}
	for _, part := range strings.Split(t, ",") {
		p := strings.TrimSpace(part)
		if strings.Contains(p, "*") {
			return true
		}
		for _, op := range []string{"!=", "==", "<=", ">=", "~=", "<", ">"} {
			if strings.HasPrefix(p, op) {
				return true
			}
		}
	}
	return false
}

// Specifier matches a single version clause.
type Specifier struct {
	op      string // "==", "!=", ">=", ">", "<=", "<", "~=", "=*" (wildcard)
	version Version
	prefix  []int // numeric prefix for wildcard clauses
}

// Set is an AND-combined list of Specifiers.
type Set []Specifier

// ParseSet parses a comma-separated specifier set such as ">=1.0.0,<2.0.0".
func ParseSet(s string) (Set, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty version specifier")
	}
	var set Set
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		sp, err := parseSpecifier(part)
		if err != nil {
			return nil, err
		}
		set = append(set, sp)
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("empty version specifier %q", s)
	}
	return set, nil
}

func parseSpecifier(s string) (Specifier, error) {
	if s == "*" {
		return Specifier{op: "=*", prefix: nil}, nil
	}

	ops := []string{"!=", "==", "<=", ">=", "~=", "<", ">"}
	op, rest := "==", strings.TrimSpace(s)
	for _, o := range ops {
		if strings.HasPrefix(s, o) {
			op, rest = o, strings.TrimSpace(s[len(o):])
			break
		}
	}
	if rest == "" {
		return Specifier{}, fmt.Errorf("missing version in specifier %q", s)
	}

	if strings.Contains(rest, "*") {
		prefix, err := parseWildcardPrefix(rest)
		if err != nil {
			return Specifier{}, fmt.Errorf("invalid wildcard %q in specifier %q", rest, s)
		}
		return Specifier{op: "=*", prefix: prefix}, nil
	}

	v, err := Parse(rest)
	if err != nil {
		return Specifier{}, fmt.Errorf("invalid version %q in specifier %q", rest, s)
	}
	return Specifier{op: op, version: v}, nil
}

func parseWildcardPrefix(rest string) ([]int, error) {
	parts := strings.Split(rest, ".")
	prefix := []int{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "*" {
			continue
		}
		if p == "" {
			return nil, fmt.Errorf("empty wildcard segment")
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("invalid numeric segment %q", p)
		}
		prefix = append(prefix, n)
	}
	return prefix, nil
}

// Match reports whether version v satisfies the specifier.
func (sp Specifier) Match(v Version) bool {
	switch sp.op {
	case "==":
		return v.Compare(sp.version) == 0
	case "=*":
		for i, n := range sp.prefix {
			if i >= len(v.parts) || v.parts[i] != n {
				return false
			}
		}
		return true
	case "!=":
		return v.Compare(sp.version) != 0
	case "<":
		return v.Compare(sp.version) < 0
	case "<=":
		return v.Compare(sp.version) <= 0
	case ">":
		return v.Compare(sp.version) > 0
	case ">=":
		return v.Compare(sp.version) >= 0
	case "~=":
		return compatibleMatch(v, sp.version)
	}
	return false
}

// compatibleMatch implements PEP 440 compatible release (~=): the version
// must be >= the given version and share its numeric prefix less the final
// segment. "~=1.4.5" matches 1.4.x where x >= 5; "~=1.4" matches 1.x.
func compatibleMatch(v, base Version) bool {
	if v.Compare(base) < 0 {
		return false
	}
	prefix := base.parts[:len(base.parts)-1]
	for i, n := range prefix {
		if i >= len(v.parts) || v.parts[i] != n {
			return false
		}
	}
	if len(prefix) == 0 {
		if len(v.parts) == 0 || v.parts[0] != base.parts[0] {
			return false
		}
	}
	return true
}

// Match reports whether version v satisfies every clause in the set.
func (set Set) Match(v Version) bool {
	for _, sp := range set {
		if !sp.Match(v) {
			return false
		}
	}
	return true
}

// Highest returns the greatest parseable version in the list.
func Highest(versions []string) (string, error) {
	var best string
	var bestV *Version
	for _, vs := range versions {
		pv, err := Parse(vs)
		if err != nil {
			continue
		}
		if bestV == nil || pv.Compare(*bestV) > 0 {
			best = vs
			bestV = &pv
		}
	}
	if best == "" {
		return "", fmt.Errorf("no parseable versions available")
	}
	return best, nil
}

// Pick returns the highest version in the list satisfying declared, which
// may be "latest"/empty, an exact version, or a specifier set. Unparseable
// list entries are ignored.
func Pick(versions []string, declared string) (string, error) {
	if IsLatest(declared) {
		return Highest(versions)
	}
	if !IsConstraint(declared) {
		for _, vs := range versions {
			if Compare(vs, declared) == 0 {
				return vs, nil
			}
		}
		return "", fmt.Errorf("version %q not found", declared)
	}

	set, err := ParseSet(declared)
	if err != nil {
		return "", err
	}
	picked := HighestMatching(versions, set)
	if picked == "" {
		return "", fmt.Errorf("no version satisfies %q", declared)
	}
	return picked, nil
}

// HighestMatching returns the greatest parseable version that satisfies
// set, or "" if none do.
func HighestMatching(versions []string, set Set) string {
	var best string
	var bestV *Version
	for _, vs := range versions {
		pv, err := Parse(vs)
		if err != nil {
			continue
		}
		if !set.Match(pv) {
			continue
		}
		if bestV == nil || pv.Compare(*bestV) > 0 {
			best = vs
			bestV = &pv
		}
	}
	return best
}

// Satisfies reports whether the on-disk version disk satisfies the declared
// requirement, which may be an exact version, "latest", or a specifier set.
func Satisfies(declared, disk string) bool {
	if IsLatest(declared) {
		return true
	}
	if !IsConstraint(declared) {
		return Compare(disk, declared) == 0
	}
	set, err := ParseSet(declared)
	if err != nil {
		return false
	}
	pv, err := Parse(disk)
	if err != nil {
		return false
	}
	return set.Match(pv)
}

// ShouldKeep reports whether the on-disk candidate version should be kept
// given the declared requirement and the full set of versions on disk for
// the same role or collection. For "latest" and constraint requirements
// only the single highest matching version is kept, mirroring what the
// fetcher stores so pruning never deletes resolvable state.
func ShouldKeep(declared string, disk []string, candidate string) bool {
	if IsLatest(declared) {
		best, err := Highest(disk)
		if err != nil {
			return false
		}
		return candidate == best
	}
	if IsConstraint(declared) {
		set, err := ParseSet(declared)
		if err != nil {
			return false
		}
		best := HighestMatching(disk, set)
		if best == "" {
			return false
		}
		return candidate == best
	}
	return Compare(candidate, declared) == 0
}

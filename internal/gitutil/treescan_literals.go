package gitutil

import "unicode/utf8"

// Literal requirements let a one-pass tree scan skip lines no pattern can
// match. A requirement is a set of ASCII strings, folded to lower case, such
// that every match of the pattern contains at least one of them; a line that
// contains none of a pattern's strings is never shown to the regex engine for
// that pattern. Only soundness matters for answers: a requirement that is too
// weak costs time, never a different match, because every candidate line is
// still decided by the same engine git grep uses. So anything this parser does
// not recognise, it treats as "requires nothing".
//
// A non-ASCII character enters a requirement only as the exact bytes of one
// valid UTF-8 sequence written outside a bracket. Under the C locale the
// engine matches those bytes; under a UTF-8 locale it matches the one
// character they decode to, and the platform decoders (FreeBSD's on macOS,
// glibc's) reject overlong and other invalid forms, so only the same bytes
// decode to it. Haystack folding touches ASCII letters only, so those bytes
// are compared as they are.

const (
	// Bounds on the string sets the extraction carries: past them a node keeps
	// only its weaker requirement, or none.
	maxLiteralExactStrings   = 64
	maxLiteralRequireStrings = 256
)

type literalInfo struct {
	// exact, when non-nil, is every string the node can match (folded).
	exact []string
	// require, when non-nil, is a set at least one of which every match
	// contains. A nil set requires nothing.
	require []string
}

// patternLiteralRequirement returns the requirement for one POSIX extended
// expression, or nil when every line must be checked.
func patternLiteralRequirement(pattern string) []string {
	p := literalParser{src: pattern}
	info, ok := p.alternation()
	if !ok || p.pos != len(p.src) {
		return nil
	}
	return usableRequirement(info)
}

func usableRequirement(info literalInfo) []string {
	set := info.require
	if info.exact != nil {
		set = info.exact
	}
	if len(set) == 0 || len(set) > maxLiteralRequireStrings {
		return nil
	}
	for _, s := range set {
		if s == "" {
			return nil
		}
	}
	return set
}

type literalParser struct {
	src string
	pos int
}

func (p *literalParser) alternation() (literalInfo, bool) {
	var branches []literalInfo
	for {
		branch, ok := p.concatenation()
		if !ok {
			return literalInfo{}, false
		}
		branches = append(branches, branch)
		if p.pos < len(p.src) && p.src[p.pos] == '|' {
			p.pos++
			continue
		}
		break
	}
	if len(branches) == 1 {
		return branches[0], true
	}
	var info literalInfo
	exact := []string{}
	for _, branch := range branches {
		if branch.exact == nil {
			exact = nil
			break
		}
		exact = unionStrings(exact, branch.exact)
		if len(exact) > maxLiteralExactStrings {
			exact = nil
			break
		}
	}
	info.exact = exact
	if exact != nil {
		return info, true
	}
	require := []string{}
	for _, branch := range branches {
		set := usableRequirement(branch)
		if set == nil {
			return literalInfo{}, true
		}
		require = unionStrings(require, set)
		if len(require) > maxLiteralRequireStrings {
			return literalInfo{}, true
		}
	}
	info.require = require
	return info, true
}

func (p *literalParser) concatenation() (literalInfo, bool) {
	current := []string{""}
	allExact := true
	var candidates [][]string
	finish := func() {
		if set := usableRequirement(literalInfo{exact: current}); set != nil {
			candidates = append(candidates, set)
		}
		current = []string{""}
	}
	for p.pos < len(p.src) && p.src[p.pos] != '|' && p.src[p.pos] != ')' {
		item, ok := p.quantified()
		if !ok {
			return literalInfo{}, false
		}
		if item.exact != nil && len(current)*len(item.exact) <= maxLiteralExactStrings {
			current = productStrings(current, item.exact)
			continue
		}
		allExact = false
		finish()
		if item.exact != nil {
			// Too many combinations to carry: start a fresh run from this item.
			current = item.exact
			continue
		}
		if set := usableRequirement(item); set != nil {
			candidates = append(candidates, set)
		}
	}
	if allExact {
		return literalInfo{exact: current}, true
	}
	finish()
	return literalInfo{require: bestRequirement(candidates)}, true
}

// bestRequirement prefers the set whose shortest string is longest (fewer
// false candidates), then the smaller set.
func bestRequirement(candidates [][]string) []string {
	var best []string
	bestShortest := 0
	for _, set := range candidates {
		shortest := len(set[0])
		for _, s := range set[1:] {
			shortest = min(shortest, len(s))
		}
		if best == nil || shortest > bestShortest || (shortest == bestShortest && len(set) < len(best)) {
			best, bestShortest = set, shortest
		}
	}
	return best
}

func (p *literalParser) quantified() (literalInfo, bool) {
	atom, ok := p.atom()
	if !ok {
		return literalInfo{}, false
	}
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case '*', '?':
			p.pos++
			atom = literalInfo{}
		case '+':
			p.pos++
			atom = literalInfo{require: usableRequirement(atom)}
		case '{':
			least, ok := p.interval()
			if !ok {
				return literalInfo{}, false
			}
			if least == 0 {
				atom = literalInfo{}
			} else {
				atom = literalInfo{require: usableRequirement(atom)}
			}
		default:
			return atom, true
		}
	}
	return atom, true
}

// interval consumes "{m}", "{m,}" or "{m,n}" and returns m.
func (p *literalParser) interval() (int, bool) {
	p.pos++ // '{'
	least, digits := 0, 0
	for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
		least = least*10 + int(p.src[p.pos]-'0')
		if least > 1<<16 {
			return 0, false
		}
		p.pos++
		digits++
	}
	if digits == 0 {
		return 0, false
	}
	if p.pos < len(p.src) && p.src[p.pos] == ',' {
		p.pos++
		for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
			p.pos++
		}
	}
	if p.pos >= len(p.src) || p.src[p.pos] != '}' {
		return 0, false
	}
	p.pos++
	return least, true
}

func (p *literalParser) atom() (literalInfo, bool) {
	c := p.src[p.pos]
	switch c {
	case '(':
		p.pos++
		if p.pos < len(p.src) && p.src[p.pos] == '?' {
			return literalInfo{}, false // an engine extension; do not guess
		}
		inner, ok := p.alternation()
		if !ok || p.pos >= len(p.src) || p.src[p.pos] != ')' {
			return literalInfo{}, false
		}
		p.pos++
		return inner, true
	case '[':
		return p.bracket()
	case '\\':
		if p.pos+1 >= len(p.src) {
			return literalInfo{}, false
		}
		next := p.src[p.pos+1]
		p.pos += 2
		if isERELiteralEscape(next) {
			return literalInfo{exact: []string{string(foldASCII(next))}}, true
		}
		// \w, \b, \<, back-references and the like: an unknown atom.
		return literalInfo{}, true
	case '^', '$':
		p.pos++
		return literalInfo{exact: []string{""}}, true
	case '.':
		p.pos++
		return literalInfo{}, true
	case '*', '+', '?', '{':
		return literalInfo{}, false
	}
	if c >= 0x80 {
		r, size := utf8.DecodeRuneInString(p.src[p.pos:])
		if r == utf8.RuneError {
			p.pos++
			return literalInfo{}, true
		}
		literal := p.src[p.pos : p.pos+size]
		p.pos += size
		return literalInfo{exact: []string{literal}}, true
	}
	p.pos++
	return literalInfo{exact: []string{string(foldASCII(c))}}, true
}

func isERELiteralEscape(c byte) bool {
	switch c {
	case '^', '.', '[', ']', '$', '(', ')', '|', '*', '+', '?', '{', '}', '\\', '/', '-':
		return true
	}
	return false
}

// bracket consumes a bracket expression. Only a plain list of ASCII
// characters that folds to one character becomes a literal; every other list
// (negated, ranged, classed, collating, non-ASCII) is an unknown atom.
func (p *literalParser) bracket() (literalInfo, bool) {
	p.pos++ // '['
	simple := true
	if p.pos < len(p.src) && p.src[p.pos] == '^' {
		simple = false
		p.pos++
	}
	var members []byte
	first := true
	for {
		if p.pos >= len(p.src) {
			return literalInfo{}, false
		}
		c := p.src[p.pos]
		if c == ']' && !first {
			p.pos++
			break
		}
		first = false
		if c == '[' && p.pos+1 < len(p.src) && (p.src[p.pos+1] == ':' || p.src[p.pos+1] == '.' || p.src[p.pos+1] == '=') {
			closer := p.src[p.pos+1]
			end := -1
			for i := p.pos + 2; i+1 < len(p.src); i++ {
				if p.src[i] == closer && p.src[i+1] == ']' {
					end = i + 2
					break
				}
			}
			if end < 0 {
				return literalInfo{}, false
			}
			p.pos = end
			simple = false
			continue
		}
		if c == '-' || c == '[' || c >= 0x80 || c == '\\' {
			simple = false
		}
		members = append(members, c)
		p.pos++
	}
	if !simple || len(members) == 0 {
		return literalInfo{}, true
	}
	folded := foldASCII(members[0])
	for _, m := range members[1:] {
		if foldASCII(m) != folded {
			return literalInfo{}, true
		}
	}
	return literalInfo{exact: []string{string(folded)}}, true
}

func foldASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

func productStrings(left, right []string) []string {
	out := make([]string, 0, len(left)*len(right))
	seen := make(map[string]bool, len(left)*len(right))
	for _, l := range left {
		for _, r := range right {
			if s := l + r; !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}

func unionStrings(left, right []string) []string {
	seen := make(map[string]bool, len(left)+len(right))
	out := make([]string, 0, len(left)+len(right))
	for _, set := range [][]string{left, right} {
		for _, s := range set {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}

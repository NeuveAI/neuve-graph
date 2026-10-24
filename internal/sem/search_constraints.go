package sem

import (
	"reflect"
	"slices"
	"strings"
	"unicode"
	"unsafe"
)

const (
	maxSearchEntities       = 8
	maxSearchPhrases        = 16
	maxSearchAgreementBonus = 4.0
)

type searchEntityConstraint struct {
	Raw        string
	Normalized string
}

type searchPhraseConstraint struct {
	Words    []string
	Explicit bool
}

type searchConstraints struct {
	Entities  []searchEntityConstraint
	Phrases   []searchPhraseConstraint
	Truncated bool
}

type searchConstraintSurface struct {
	words       []string
	wholeTokens map[string]bool
}

var searchConstraintInterrogatives = map[string]bool{
	"what": true, "when": true, "where": true, "who": true, "why": true, "how": true,
}

// parseSearchConstraints retains only a bounded, generic description of the
// caller's explicit entities and ordered content. It never decides admission:
// agreement is an additive score only after ordinary retrieval has found a
// candidate.
func parseSearchConstraints(raw string) searchConstraints {
	explicit, remaining := searchExplicitSearchPhrases(raw)
	constraints := searchConstraints{}
	shouted := searchQueryIsShouted(searchWordPattern.FindAllString(raw, -1))
	seenPhrases := map[string]bool{}
	for _, phrase := range explicit {
		searchAppendPhraseConstraint(&constraints, seenPhrases, phrase)
	}

	content := make([]string, 0, 16)
	seenEntities := map[string]bool{}
	firstInSentence := true
	last := 0
	for _, match := range searchWordPattern.FindAllStringIndex(remaining, -1) {
		if strings.ContainsAny(remaining[last:match[0]], ".!?") {
			firstInSentence = true
		}
		rawToken := remaining[match[0]:match[1]]
		normalized := strings.ToLower(rawToken)
		if normalized != "" {
			if !shouted && !firstInSentence && !searchStopWords[normalized] && !searchConstraintInterrogatives[normalized] &&
				(identifierShapedToken(rawToken) || searchConstraintTitleOrAllCaps(rawToken)) && !seenEntities[normalized] {
				if len(constraints.Entities) < maxSearchEntities {
					constraints.Entities = append(constraints.Entities, searchEntityConstraint{Raw: rawToken, Normalized: normalized})
					seenEntities[normalized] = true
				} else {
					constraints.Truncated = true
				}
			}
			for _, word := range searchQueryWordSequence(normalized) {
				if !searchStopWords[word] {
					content = append(content, word)
				}
			}
		}
		firstInSentence = false
		last = match[1]
	}

	for width := 3; width >= 2; width-- {
		for start := 0; start+width <= len(content); start++ {
			words := append([]string(nil), content[start:start+width]...)
			searchAppendPhraseConstraint(&constraints, seenPhrases, searchPhraseConstraint{Words: words})
		}
	}
	return constraints
}

func searchExplicitSearchPhrases(raw string) ([]searchPhraseConstraint, string) {
	var phrases []searchPhraseConstraint
	masked := []rune(raw)
	start := -1
	escaped := false
	for index, r := range masked {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if r != '"' {
			continue
		}
		if start < 0 {
			start = index
			continue
		}
		words := searchQueryWordSequence(strings.ToLower(string(masked[start+1 : index])))
		if len(words) > 0 {
			phrases = append(phrases, searchPhraseConstraint{Words: words, Explicit: true})
		}
		for clear := start; clear <= index; clear++ {
			masked[clear] = ' '
		}
		start = -1
	}
	return phrases, string(masked)
}

func searchAppendPhraseConstraint(constraints *searchConstraints, seen map[string]bool, phrase searchPhraseConstraint) {
	if len(phrase.Words) == 0 {
		return
	}
	key := strings.Join(phrase.Words, "\x00")
	if phrase.Explicit {
		key = "quoted\x00" + key
	}
	if seen[key] {
		return
	}
	seen[key] = true
	if len(constraints.Phrases) >= maxSearchPhrases {
		constraints.Truncated = true
		return
	}
	constraints.Phrases = append(constraints.Phrases, phrase)
}

func searchConstraintTitleOrAllCaps(raw string) bool {
	letters := 0
	hasUpper := false
	for _, r := range raw {
		if !unicode.IsLetter(r) {
			continue
		}
		letters++
		if unicode.IsUpper(r) {
			hasUpper = true
		}
	}
	if letters == 0 || !hasUpper {
		return false
	}
	first, _ := utf8FirstRune(raw)
	return unicode.IsUpper(first)
}

func utf8FirstRune(value string) (rune, bool) {
	for _, r := range value {
		return r, true
	}
	return 0, false
}

// orderedSearchPhraseMatch is the strict predicate used for quoted spans and
// direct callers: every normalized word must be adjacent and in written order.
func orderedSearchPhraseMatch(text string, phrase searchPhraseConstraint) bool {
	return searchOrderedPhraseMatch(searchQueryWordSequence(strings.ToLower(text)), phrase.Words, 0, false)
}

func searchOrderedPhraseMatch(words, phrase []string, maxInterveningContent int, looseMorphology bool) bool {
	if len(phrase) == 0 {
		return false
	}
	// The words each phrase word accepts: itself, or with loose morphology
	// the words searchConstraintWordsLooselyEqual accepts, found once instead
	// of on every comparison.
	forms := make([][]string, len(phrase))
	for index := range phrase {
		if looseMorphology {
			forms[index] = searchConstraintLooseForms(phrase[index])
		} else {
			forms[index] = phrase[index : index+1]
		}
	}
	equal := func(word string, want int) bool {
		for _, form := range forms[want] {
			if word == form {
				return true
			}
		}
		return false
	}
	// A phrase starts only at a word as long as one of its first word's
	// forms: most words are skipped on their length alone.
	var firstLengths uint64
	firstLong := false
	for _, form := range forms[0] {
		if len(form) < 64 {
			firstLengths |= 1 << len(form)
		} else {
			firstLong = true
		}
	}
	for start, word := range words {
		if length := len(word); length < 64 && firstLengths&(1<<length) == 0 || length >= 64 && !firstLong {
			continue
		}
		if !equal(word, 0) {
			continue
		}
		at := start + 1
		matched := true
		for want := 1; want < len(phrase); want++ {
			intervening := 0
			found := false
			for at < len(words) {
				if equal(words[at], want) {
					found = true
					at++
					break
				}
				if !searchStopWords[words[at]] {
					intervening++
				}
				if intervening > maxInterveningContent {
					break
				}
				at++
			}
			if !found {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// searchConstraintLooseForms is every got for which
// searchConstraintWordsLooselyEqual(got, want) holds: want itself, want with
// a suffix when want is longer than two bytes, and want without a suffix it
// ends in when what is left is longer than two bytes.
func searchConstraintLooseForms(want string) []string {
	forms := []string{want}
	for _, suffix := range []string{"ed", "ing", "s"} {
		if len(want) > 2 {
			forms = append(forms, want+suffix)
		}
		if strings.HasSuffix(want, suffix) && len(want) > len(suffix)+2 {
			forms = append(forms, strings.TrimSuffix(want, suffix))
		}
	}
	return forms
}

func searchConstraintWordsLooselyEqual(got, want string) bool {
	if got == want {
		return true
	}
	for _, suffix := range []string{"ed", "ing", "s"} {
		if strings.HasSuffix(got, suffix) && len(got) > len(suffix)+2 && strings.TrimSuffix(got, suffix) == want ||
			strings.HasSuffix(want, suffix) && len(want) > len(suffix)+2 && strings.TrimSuffix(want, suffix) == got {
			return true
		}
	}
	return false
}

func searchConstraintsRequireAgreement(constraints searchConstraints) bool {
	if len(constraints.Entities) > 0 {
		return true
	}
	for _, phrase := range constraints.Phrases {
		if phrase.Explicit {
			return true
		}
	}
	return false
}

func buildSearchConstraintSurface(parts ...string) searchConstraintSurface {
	surface := searchConstraintSurface{
		wholeTokens: map[string]bool{},
	}
	var raw []string
	for _, part := range parts {
		surface.words, raw = appendSearchConstraintTokens(part, surface.words, raw)
	}
	for _, word := range surface.words {
		surface.wholeTokens[word] = true
	}
	for _, token := range raw {
		surface.wholeTokens[token] = true
	}
	return surface
}

// appendSearchConstraintTokens appends one part's words (its runs of Unicode
// letters and digits, lowered) and its raw tokens (its runs of
// searchWordPattern's ASCII class, lowered), as a surface splits each of its
// parts: the end of a part ends both, and so does a line break, so a part's
// tokens are its lines' tokens in order.
func appendSearchConstraintTokens(part string, words, raw []string) ([]string, []string) {
	word := make([]rune, 0, 16)
	token := make([]byte, 0, 16)
	flushWord := func() {
		if len(word) > 0 {
			words = append(words, string(word))
			word = word[:0]
		}
	}
	flushRaw := func() {
		if len(token) > 0 {
			raw = append(raw, string(token))
			token = token[:0]
		}
	}
	for _, character := range part {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			word = append(word, unicode.ToLower(character))
		} else {
			flushWord()
		}
		// Go regexp POSIX character classes are ASCII. This is the exact
		// character set of searchWordPattern, accumulated during the same
		// scan as the Unicode word sequence above.
		if character >= 'A' && character <= 'Z' {
			token = append(token, byte(character-'A'+'a'))
		} else if character >= 'a' && character <= 'z' ||
			character >= '0' && character <= '9' ||
			strings.ContainsRune("_./:+#-", character) {
			token = append(token, byte(character))
		} else {
			flushRaw()
		}
	}
	// The old implementation joined parts with a newline, which terminates
	// both token classes between metadata fields.
	flushWord()
	flushRaw()
	return words, raw
}

func searchCandidateAgreementSurface(surface searchConstraintSurface, constraints searchConstraints) (float64, []string) {
	if !searchConstraintsRequireAgreement(constraints) {
		return 0, nil
	}
	found := make([]bool, len(constraints.Entities))
	for index, entity := range constraints.Entities {
		found[index] = surface.wholeTokens[entity.Normalized]
	}
	return searchCandidateAgreementWith(surface.words, found, constraints)
}

// searchCandidateAgreementWith scores agreement from a candidate's words and
// which of the constraints' entities are among its whole tokens.
func searchCandidateAgreementWith(words []string, entityFound []bool, constraints searchConstraints) (float64, []string) {
	matchedEntities := 0
	for _, found := range entityFound {
		if found {
			matchedEntities++
		}
	}
	matchedPhrases := 0
	eligiblePhrases := 0
	for _, phrase := range constraints.Phrases {
		if len(constraints.Entities) == 0 && !phrase.Explicit {
			continue
		}
		eligiblePhrases++
		maxInterveningContent := 2
		looseMorphology := true
		if phrase.Explicit {
			maxInterveningContent = 0
			looseMorphology = false
		}
		if searchOrderedPhraseMatch(words, phrase.Words, maxInterveningContent, looseMorphology) {
			matchedPhrases++
		}
	}
	if matchedEntities == 0 && matchedPhrases == 0 {
		return 0, nil
	}
	bonus := 0.0
	signals := make([]string, 0, 3)
	if matchedEntities > 0 {
		bonus += 1.5 * float64(matchedEntities) / float64(len(constraints.Entities))
		signals = append(signals, "constraint:entity")
	}
	if matchedPhrases > 0 {
		bonus += 1.5 * float64(matchedPhrases) / float64(eligiblePhrases)
		signals = append(signals, "constraint:phrase")
	}
	if matchedEntities > 0 && matchedPhrases > 0 {
		bonus += 1.0
		signals = append(signals, "constraint:relationship")
	}
	return minFloat64(maxSearchAgreementBonus, bonus), signals
}

func buildSearchCandidateConstraintSurface(candidate searchCandidate) searchConstraintSurface {
	return buildSearchConstraintSurface(
		candidate.result.SymbolName,
		candidate.result.QualifiedName,
		candidate.result.Signature,
		candidate.result.Snippet,
		strings.Join(candidate.aliases, "\n"),
	)
}

func searchCandidateConstraintMayMatch(candidate searchCandidate, constraints searchConstraints) bool {
	text := strings.ToLower(strings.Join([]string{
		candidate.result.SymbolName,
		candidate.result.QualifiedName,
		candidate.result.Signature,
		candidate.result.Snippet,
		strings.Join(candidate.aliases, "\n"),
	}, "\n"))
	return searchConstraintMayMatchWith(func(needle string) bool { return strings.Contains(text, needle) }, constraints)
}

// searchConstraintMayMatchWith is searchCandidateConstraintMayMatch's rule
// over a candidate's lowered text, given as whether it contains a string.
func searchConstraintMayMatchWith(contains func(string) bool, constraints searchConstraints) bool {
	for _, entity := range constraints.Entities {
		if contains(entity.Normalized) {
			return true
		}
	}
	for _, phrase := range constraints.Phrases {
		if len(phrase.Words) == 0 || len(constraints.Entities) == 0 && !phrase.Explicit {
			continue
		}
		first := phrase.Words[0]
		if contains(first) {
			return true
		}
		if phrase.Explicit {
			continue
		}
		for _, suffix := range []string{"ed", "ing", "s"} {
			if strings.HasSuffix(first, suffix) && len(first) > len(suffix)+2 &&
				contains(strings.TrimSuffix(first, suffix)) {
				return true
			}
		}
	}
	return false
}

// searchCandidateAgreementResult is one candidate's agreement with one set of
// constraints. Keeping the result, not the surface it was computed from,
// keeps a search's memory flat: a surface holds every word of the
// candidate's text, and a broad search ranks thousands of candidates, some
// of them whole files (measured: 4.7 of a 5 GB heap).
type searchCandidateAgreementResult struct {
	constraints searchConstraints
	bonus       float64
	signals     []string
	// fields, for a result computed when the candidate was made, names the
	// text it was computed from; it answers only while the candidate holds
	// that very text. A miss is remembered then too.
	fields *searchCandidateFields
}

// searchCandidateFields names a candidate's constraint text by its strings'
// bytes in memory: equal names, equal text.
type searchCandidateFields struct {
	name, qualified, signature, snippet searchPartKey
	aliases                             *string
	aliasCount                          int
}

func candidateConstraintFields(candidate *searchCandidate) searchCandidateFields {
	key := func(value string) searchPartKey {
		return searchPartKey{data: unsafe.StringData(value), length: len(value)}
	}
	fields := searchCandidateFields{
		name: key(candidate.result.SymbolName), qualified: key(candidate.result.QualifiedName),
		signature: key(candidate.result.Signature), snippet: key(candidate.result.Snippet),
		aliasCount: len(candidate.aliases),
	}
	if len(candidate.aliases) > 0 {
		fields.aliases = &candidate.aliases[0]
	}
	return fields
}

// precomputeSearchCandidateAgreement computes a new candidate's agreement
// from its file's line cache, remembered with the text it read.
func precomputeSearchCandidateAgreement(candidate *searchCandidate, constraints searchConstraints) {
	if !searchConstraintsRequireAgreement(constraints) {
		return
	}
	bonus, signals, _, ok := searchCandidateAgreementFromLines(candidate, constraints)
	if !ok {
		return
	}
	fields := candidateConstraintFields(candidate)
	candidate.agreement = &searchCandidateAgreementResult{constraints: constraints, bonus: bonus, signals: slices.Clone(signals), fields: &fields}
}

func searchCandidateAgreementCached(candidate *searchCandidate, constraints searchConstraints) (float64, []string) {
	if !searchConstraintsRequireAgreement(constraints) {
		return 0, nil
	}
	if cached := candidate.agreement; cached != nil && reflect.DeepEqual(cached.constraints, constraints) &&
		(cached.fields == nil || *cached.fields == candidateConstraintFields(candidate)) {
		return cached.bonus, slices.Clone(cached.signals)
	}
	if !searchCandidateConstraintMayMatch(*candidate, constraints) {
		return 0, nil
	}
	bonus, signals := searchCandidateAgreementSurface(buildSearchCandidateConstraintSurface(*candidate), constraints)
	candidate.agreement = &searchCandidateAgreementResult{constraints: constraints, bonus: bonus, signals: slices.Clone(signals)}
	return bonus, signals
}

func searchCandidateAgreement(candidate searchCandidate, constraints searchConstraints) (float64, []string) {
	return searchCandidateAgreementCached(&candidate, constraints)
}

func searchConstraintWholeTokens(text string, words []string) map[string]bool {
	tokens := make(map[string]bool, len(words))
	for _, word := range words {
		tokens[word] = true
	}
	for _, raw := range searchWordPattern.FindAllString(text, -1) {
		tokens[strings.ToLower(raw)] = true
	}
	return tokens
}

func applySearchCandidateAgreement(candidates []searchCandidate, constraints searchConstraints) {
	for index := range candidates {
		bonus, signals := searchCandidateAgreementCached(&candidates[index], constraints)
		if bonus == 0 {
			continue
		}
		candidates[index].score += bonus
		candidates[index].result.Signals = appendUnique(candidates[index].result.Signals, signals...)
	}
}

// searchCandidateAgreementFromLines answers what searchCandidateConstraintMayMatch
// and, where it may match, the surface of buildSearchCandidateConstraintSurface
// answer, reading the candidate's snippet lines' tokens from its file's line
// cache instead of splitting its text again: overlapping candidates (a file,
// its types, their members) hold the same lines. ok is false where that
// cannot be exact: no cache, a snippet that is not its lines joined, or a
// search term holding a line break, which a joined text could match across
// pieces.
func searchCandidateAgreementFromLines(candidate *searchCandidate, constraints searchConstraints) (bonus float64, signals []string, mayMatch bool, ok bool) {
	cache := candidate.lineCache
	result := &candidate.result
	if cache == nil || !searchSnippetIsLines(result.Snippet, cache.lines, result.SnippetStartLine, result.SnippetEndLine) {
		return 0, nil, false, false
	}
	for _, entity := range constraints.Entities {
		if strings.Contains(entity.Normalized, "\n") {
			return 0, nil, false, false
		}
	}
	for _, phrase := range constraints.Phrases {
		for _, word := range phrase.Words {
			if strings.Contains(word, "\n") {
				return 0, nil, false, false
			}
		}
	}
	// The text the surface splits, in order, as pieces split once per file:
	// the symbol's parts, the snippet's lines, and each alias (the aliases
	// are joined by LF, which ends every token, as a line break does).
	pieces := make([]*searchLineTokens, 0, 3+result.SnippetEndLine-result.SnippetStartLine+1+len(candidate.aliases))
	for _, part := range []string{result.SymbolName, result.QualifiedName, result.Signature} {
		pieces = append(pieces, cache.partTokens(part))
	}
	for line := result.SnippetStartLine; line <= result.SnippetEndLine; line++ {
		pieces = append(pieces, cache.constraintTokens(line))
	}
	for _, alias := range candidate.aliases {
		if strings.Contains(alias, "\n") {
			return 0, nil, false, false
		}
		pieces = append(pieces, cache.partTokens(alias))
	}
	// The lowered joined text, as pieces: lowering is per rune, and a term
	// without a line break matches inside one piece or not at all.
	contains := func(needle string) bool {
		if needle == "" {
			return true // the joined text always holds the empty string
		}
		for _, piece := range pieces {
			if strings.Contains(piece.lower, needle) {
				return true
			}
		}
		return false
	}
	if !searchConstraintMayMatchWith(contains, constraints) {
		return 0, nil, false, true
	}
	var words []string
	for _, piece := range pieces {
		words = append(words, piece.words...)
	}
	// An entity is found when some piece's words or tokens hold it; each
	// piece's entities are found once (entityHits), not again for every
	// candidate holding the piece.
	found := make([]bool, len(constraints.Entities))
	if len(constraints.Entities) > 0 {
		for _, piece := range pieces {
			for _, index := range cache.entityHits(piece, constraints.Entities) {
				found[index] = true
			}
		}
	}
	bonus, signals = searchCandidateAgreementWith(words, found, constraints)
	return bonus, signals, true, true
}

// searchSnippetIsLines reports whether snippet is lines[start-1:end] joined by
// LF, comparing in place.
func searchSnippetIsLines(snippet string, lines []string, start, end int) bool {
	if start < 1 || end > len(lines) || start > end {
		return false
	}
	at := 0
	for line := start; line <= end; line++ {
		if line > start {
			if at >= len(snippet) || snippet[at] != '\n' {
				return false
			}
			at++
		}
		text := lines[line-1]
		if !strings.HasPrefix(snippet[at:], text) {
			return false
		}
		at += len(text)
	}
	return at == len(snippet)
}

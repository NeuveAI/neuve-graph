package sem

import (
	"sort"
	"strings"
)

// callSiteLineLanguageList names the languages whose call relations say where
// the call is: for a CALLS, CONSTRUCTS or ASYNC_CALLS relation whose caller's
// file is in one of them, a line on the call-site evidence entry is the line of
// the call itself, and an entry with no line is a call whose position the
// engine could not tell. Capability call_site_lines declares it.
//
// Every other language keeps the earlier meaning: the entry of a call made from
// a symbol carries the caller's own span, and one made at file level carries
// none. Their call scanners either cannot tell a call from a nested
// declaration without the syntax tree (`void run() {` inside an anonymous
// class reads as a call to run) or find calls through a dedicated scanner that
// keeps no position.
var callSiteLineLanguageList = []string{"Go", "JavaScript", "Python", "TypeScript"}

var callSiteLineLanguageSet = func() map[string]bool {
	set := make(map[string]bool, len(callSiteLineLanguageList))
	for _, language := range callSiteLineLanguageList {
		set[language] = true
	}
	return set
}()

// CallSiteLineLanguages returns the languages capability call_site_lines lists.
func CallSiteLineLanguages() []string {
	return append([]string(nil), callSiteLineLanguageList...)
}

// callLineLegacy asks for the earlier evidence lines: the caller's own span.
// It is what every call relation of a language outside callSiteLineLanguageList
// keeps.
const callLineLegacy = -1

// callSiteLines turns a position a caller's call scanners recorded into the
// line of the caller's file the call is written on.
//
// text is the source those scanners read for this caller and firstLine the
// file line of its first byte. Every scanner reads a copy of text whose masks
// keep its length and every newline, so an offset into any of them counts the
// same newlines as an offset into text.
type callSiteLines struct {
	declared  bool
	from      SymbolRecord
	text      string
	firstLine int
	// textStart is the file byte of text's first byte when text is a slice
	// of the file, or -1 when its masks moved bytes (they never move lines).
	textStart int
}

func newCallSiteLines(from SymbolRecord, language, text string, firstLine int) callSiteLines {
	return callSiteLines{declared: callSiteLineLanguageSet[language], from: from, text: text, firstLine: firstLine, textStart: -1}
}

// at returns the line of offset in text: callLineLegacy for a language that
// does not promise call lines, 0 when the offset is unknown (negative) or the
// line it gives is not inside the caller.
func (c callSiteLines) at(offset int) int {
	return c.in(c.text, offset)
}

// in is at for an offset into a copy of the caller's text that a scanner
// masked (it keeps every newline where text has one).
func (c callSiteLines) in(text string, offset int) int {
	if !c.declared {
		return callLineLegacy
	}
	if offset < 0 || offset > len(text) {
		return 0
	}
	return c.checked(c.firstLine + strings.Count(text[:offset], "\n"))
}

// site is at for a scanner's recorded site: offset+1, or 0 when the scanner
// kept no position for the call. The site is the called name.
func (c callSiteLines) site(site int) int {
	return c.callIn(c.text, site-1)
}

// callIn is in for the offset of a called name, which it places only when a
// call is written there: in Python and Go a line break between the name and
// its `(` ends the statement (`fixture` then `(home / "x")` on the next
// line), so the scanners' match is not a call and gets no line.
func (c callSiteLines) callIn(text string, offset int) int {
	if c.declared && offset >= 0 && offset < len(text) && lineBreakBeforeCallParen(c.from.Language, text, offset) {
		return 0
	}
	return c.in(text, offset)
}

// lineBreakBeforeCallParen reports, for Python and Go, whether the `(` the call
// scanners matched after the name at offset is on a later line than the name.
func lineBreakBeforeCallParen(language, text string, offset int) bool {
	if language != "Python" && language != "Go" {
		return false
	}
	i := offset
	for i < len(text) && isASCIIIdentifierByte(text[i]) {
		i++
	}
	for ; i < len(text); i++ {
		switch text[i] {
		case '\n':
			return true
		case ' ', '\t', '\r':
			continue
		default:
			return false
		}
	}
	return false
}

// checked keeps a line only when it lies inside the caller. A symbol's calls
// are written inside its span; a line outside it would name another symbol's
// code, so the entry gets no line rather than a wrong one.
func (c callSiteLines) checked(line int) int {
	if !c.declared {
		return callLineLegacy
	}
	if line <= 0 {
		return 0
	}
	if c.from.Kind != "file" && (line < c.from.StartLine || line > c.from.EndLine) {
		return 0
	}
	return line
}

// callLineReference, set on a line, says the target's name is handed over
// there as a value (`xs.map(double)`, `test("case", handler)`) and not called.
// The entry then carries form "reference". It is a bit above any file line, so
// a placement stays one int through every path that passes lines around.
const callLineReference = 1 << 30

// evidenceFormReference is the evidence form of a name handed over as a value.
const evidenceFormReference = "reference"

// referenceLine marks a placed line as a reference; 0 and callLineLegacy stay.
func referenceLine(line int) int {
	if line > 0 {
		return line | callLineReference
	}
	return line
}

// lineOnly drops the reference mark from a placement.
func lineOnly(line int) int {
	if line > 0 {
		return line &^ callLineReference
	}
	return line
}

// isReferenceLine reports whether a placement is a marked reference.
func isReferenceLine(line int) bool {
	return line > 0 && line&callLineReference != 0
}

// callSiteEvidence builds a call relation's one evidence entry. line is a file
// line (marked by referenceLine when the name is handed over, not called), 0
// for none, or callLineLegacy for the caller's span.
func callSiteEvidence(kind string, from SymbolRecord, detail string, line int) Evidence {
	evidence := Evidence{Kind: kind, FilePath: from.FilePath, Detail: detail}
	setEvidenceCallLine(&evidence, from, line)
	return evidence
}

func setEvidenceCallLine(evidence *Evidence, from SymbolRecord, line int) {
	evidence.Form = ""
	switch {
	case line == callLineLegacy:
		evidence.StartLine, evidence.EndLine = from.StartLine, from.EndLine
	case line > 0:
		if isReferenceLine(line) {
			evidence.Form = evidenceFormReference
		}
		line = lineOnly(line)
		evidence.StartLine, evidence.EndLine = line, line
	default:
		evidence.StartLine, evidence.EndLine = 0, 0
	}
}

// callSiteEvidenceKind reports the evidence kinds that say where a call is.
func callSiteEvidenceKind(kind string) bool {
	switch kind {
	case "call_site", "imported_call_site", "async_call_site", "top_level_call_site":
		return true
	}
	return false
}

// withCallLine sets the line of each call-site entry of relations built by a
// helper that does not know where the call is.
func withCallLine(relations []RelationRecord, from SymbolRecord, line int) []RelationRecord {
	for i := range relations {
		for j := range relations[i].Evidence {
			if callSiteEvidenceKind(relations[i].Evidence[j].Kind) {
				setEvidenceCallLine(&relations[i].Evidence[j], from, line)
			}
		}
	}
	return relations
}

// bareCallLine says where the call behind a bare-name relation of caller from
// is written.
//
// In TypeScript and JavaScript the syntax tree is the authority when it was
// read whole: a namespace call (`A.B.f()`) is the first such call of that
// canonical name, any other name its first use -- a call or construction, else
// a callable argument (callsOnly drops the argument for a construction);
// visibleOnly skips a use a nearer binding shadows, which is how an imported
// alias is resolved. When the tree is whole and shows no use, the text scan
// found the name in something that is not a call (an interface method
// signature, JSX text) and the entry gets no line. Without a whole tree, and in
// every other language, it is the scanner's own first match: the generic call
// scan's, else (JavaScript) the callable-argument scan's.
func bareCallLine(sites callSiteLines, callBlock, name string, from SymbolRecord, jsScan *jsScanState, jsNamespaceCalls map[string]struct{}, excluded jsByteSpans, bareSites, argumentSites map[string]int, callsOnly, visibleOnly bool) int {
	if !sites.declared {
		return callLineLegacy
	}
	if jsScan != nil && (from.Language == "JavaScript" || from.Language == "TypeScript") {
		var start, end int
		if from.Kind == "file" {
			start, end = 0, jsScan.contentLen
		} else {
			start, end = jsScan.symbolByteRange(from)
		}
		if _, namespaceCall := jsNamespaceCalls[name]; namespaceCall {
			if position, ok := jsScan.namespaceCallSite(name, start, end, excluded); ok {
				return sites.checked(jsScan.lineOf(position))
			}
			return 0
		}
		if jsScan.parsed && !jsScan.depthTruncated {
			treeLine := 0
			if position, ok, argument := jsScan.callUseSite(name, start, end, excluded, callsOnly, visibleOnly); ok {
				treeLine = sites.checked(jsScan.lineOf(position))
				if argument {
					treeLine = referenceLine(treeLine)
				}
			}
			if jsScan.errorFree || treeLine > 0 {
				return earlierJSTextOnlyCallLine(sites, callBlock, name, jsScan, bareSites, treeLine, visibleOnly)
			}
		}
	}
	if offset, ok := bareSites[name]; ok && offset >= 0 {
		if (from.Language == "JavaScript" || from.Language == "TypeScript") && jsDeclarationShapedMatch(callBlock, offset) {
			// Without a whole tree, `name(...) {` is a method shorthand or
			// declaration as often as a call; it is not offered as one.
			return 0
		}
		return sites.in(callBlock, offset)
	}
	if !callsOnly {
		if offset, ok := argumentSites[name]; ok && offset >= 0 {
			return referenceLine(sites.in(callBlock, offset))
		}
	}
	return 0
}

// jsTextOnlyCallLine places a TypeScript or JavaScript call with type
// arguments (`name<T>(`) that the scope walk does not record because the
// grammar read it as two comparisons, from the text scan's first match. A
// match followed by `{`, `:` or `=` after its arguments is a declaration or an
// arrow head (`name<T>(x) {`) and is not placed. Any other match the tree does
// not show as a use is not a call to place: an interface method signature, a
// word in template text, a parameter of a method shorthand, or a private
// member call (`this.#name(`), which a bare name cannot resolve to with
// certainty. Those get no line.
func jsTextOnlyCallLine(sites callSiteLines, text string, offset int) int {
	if offset < 1 || offset >= len(text) {
		return 0
	}
	end := offset
	for end < len(text) && isASCIIIdentifierByte(text[end]) {
		end++
	}
	open := end
	for open < len(text) && isASCIISpace(text[open]) {
		open++
	}
	if open >= len(text) || text[open] != '<' || text[offset-1] == '#' {
		return 0
	}
	if jsDeclarationShapedMatch(text, offset) {
		return 0
	}
	return sites.in(text, offset)
}

// jsDeclarationShapedMatch reports whether the `name(` (or `name<T>(`) match at
// offset is followed, after its balanced arguments, by `{`, `:` or `=`: the
// shape of a method shorthand, a declaration or an arrow head rather than a
// call. An unbalanced match counts as one too.
func jsDeclarationShapedMatch(text string, offset int) bool {
	if offset < 0 || offset >= len(text) {
		return true
	}
	stripped := stripCodeLiteralsAndComments(text)
	paren := strings.IndexByte(stripped[offset:], '(')
	if paren < 0 {
		return true
	}
	paren += offset
	close := matchingParen(stripped, paren)
	if close < 0 {
		return true
	}
	switch nextNonSpace(stripped, close+1) {
	case '{', ':', '=':
		return true
	}
	return false
}

// earlierJSTextOnlyCallLine returns the line of the first use the syntax tree
// shows (treeLine, 0 for none), unless the text scan's first match is a call
// with type arguments the tree did not record and comes before it. With
// visibleOnly that match must also be one no nearer binding shadows, which can
// be told only where the text is a slice of the file.
func earlierJSTextOnlyCallLine(sites callSiteLines, text, name string, jsScan *jsScanState, bareSites map[string]int, treeLine int, visibleOnly bool) int {
	offset := textSite(bareSites, name)
	textLine := jsTextOnlyCallLine(sites, text, offset)
	// A call beats a reference wherever it is; between calls the first wins.
	if textLine <= 0 || (treeLine > 0 && !isReferenceLine(treeLine) && textLine >= treeLine) {
		return treeLine
	}
	if visibleOnly && (sites.textStart < 0 || jsScan.bindingShadowsName(name, sites.textStart+offset)) {
		return treeLine
	}
	return textLine
}

// textSite returns the text scan's site for name, or -1 when it has none.
func textSite(sites map[string]int, name string) int {
	if offset, ok := sites[name]; ok {
		return offset
	}
	return -1
}

// nextNonSpace returns the first byte at or after position that is not
// white space, or 0.
func nextNonSpace(text string, position int) byte {
	for i := position; i < len(text); i++ {
		if !isASCIISpace(text[i]) {
			return text[i]
		}
	}
	return 0
}

// topLevelCallLine is bareCallLine for the file-level caller: its calls are the
// ones no symbol's source holds, which for TypeScript and JavaScript includes
// every call inside a test callback (`test("...", () => { subject() })`).
func topLevelCallLine(sites callSiteLines, topLevel, name string, jsScan *jsScanState, uses *jsTopLevelUses, jsNamespaceCalls map[string]struct{}, bareSites, argumentSites map[string]int, callsOnly, visibleOnly bool) int {
	if !sites.declared {
		return callLineLegacy
	}
	if jsScan != nil && uses != nil {
		if _, namespaceCall := jsNamespaceCalls[name]; namespaceCall {
			if position, ok := uses.namespace[name]; ok {
				return sites.checked(jsScan.lineOf(position))
			}
			return 0
		}
		if jsScan.parsed && !jsScan.depthTruncated {
			treeLine := 0
			if position, ok, argument := uses.first(name, callsOnly, visibleOnly); ok {
				treeLine = sites.checked(jsScan.lineOf(position))
				if argument {
					treeLine = referenceLine(treeLine)
				}
			}
			if jsScan.errorFree || treeLine > 0 {
				return earlierJSTextOnlyCallLine(sites, topLevel, name, jsScan, bareSites, treeLine, visibleOnly)
			}
		}
	}
	if offset, ok := bareSites[name]; ok && offset >= 0 {
		if jsScan != nil && jsDeclarationShapedMatch(topLevel, offset) {
			return 0
		}
		return sites.in(topLevel, offset)
	}
	if !callsOnly {
		if offset, ok := argumentSites[name]; ok && offset >= 0 {
			return referenceLine(sites.in(topLevel, offset))
		}
	}
	return 0
}

// jsTopLevelUses holds, for each name, where its first file-level use is: the
// uses jsScanState.callUseSite counts, outside every symbol's source.
type jsTopLevelUses struct {
	calls, arguments               map[string]int
	visibleCalls, visibleArguments map[string]int
	namespace                      map[string]int
}

// first returns where name's first file-level use is, and whether that use is
// a callable argument (no call of name is written at file level).
func (u *jsTopLevelUses) first(name string, callsOnly, visibleOnly bool) (int, bool, bool) {
	calls, arguments := u.calls, u.arguments
	if visibleOnly {
		calls, arguments = u.visibleCalls, u.visibleArguments
	}
	if position, ok := calls[name]; ok {
		return position, true, false
	}
	if callsOnly {
		return 0, false, false
	}
	position, ok := arguments[name]
	return position, ok, ok
}

// topLevelUses indexes the file-level uses once per file, so the file-level
// pass does not walk every use of the file once per name.
func (s *jsScanState) topLevelUses(symbols []SymbolRecord) *jsTopLevelUses {
	uses := &jsTopLevelUses{calls: map[string]int{}, arguments: map[string]int{}, visibleCalls: map[string]int{}, visibleArguments: map[string]int{}, namespace: map[string]int{}}
	if s == nil {
		return uses
	}
	spans := s.symbolByteSpans(symbols)
	index := func(list []jsIdentifierUse, all, visible map[string]int) {
		for _, use := range list {
			if spans.contains(use.startByte) {
				continue
			}
			if _, seen := all[use.name]; !seen {
				all[use.name] = use.startByte
			}
			if _, seen := visible[use.name]; !seen && !s.bindingShadowsName(use.name, use.startByte) {
				visible[use.name] = use.startByte
			}
		}
	}
	index(s.bareCalls, uses.calls, uses.visibleCalls)
	index(s.callableArguments, uses.arguments, uses.visibleArguments)
	if len(s.namespaces) > 0 {
		for _, call := range s.calls {
			if spans.contains(call.startByte) {
				continue
			}
			if qualified := s.qualifiedNamespaceCall(call); qualified != "" {
				if _, seen := uses.namespace[qualified]; !seen {
					uses.namespace[qualified] = call.nameByte
				}
			}
		}
	}
	return uses
}

// lineOfByte returns the 1-based line holding byte position in a file whose
// line starts are starts.
func lineOfByte(starts []int, position int) int {
	return sort.SearchInts(starts, position+1)
}

// callDeclarationKeyword reports whether the word before a `name(` match makes
// it a declaration rather than a call (`function name(`, `def name(`,
// `class Name(`, `func name(`). The call scanners still count such a match --
// what they find is unchanged -- but it is never offered as the line of a call.
func callDeclarationKeyword(content string, nameStart int) bool {
	word, _ := identifierBefore(content, nameStart)
	if word == "" {
		// `function* name(`: a generator declaration.
		i := nameStart - 1
		for i >= 0 && isASCIISpace(content[i]) {
			i--
		}
		if i >= 0 && content[i] == '*' {
			word, _ = identifierBefore(content, i)
		}
	}
	switch word {
	case "function", "def", "class", "func":
		return true
	}
	return false
}

//go:build cgo && !windows

package gitutil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
)

// scanTreeFirstPatternLines answers what one `git grep -I -E -m 1 -f -` over
// patterns answers on an immutable tree (each regular, non-binary file's first
// line matching any pattern, in path byte order), reading every blob once.
//
// git grep would read and inflate the whole tree once per pattern, or, as one
// multi-pattern process, re-run every absent pattern after each hit (see
// grepTreeFirstPatternLinesGit). Here one `git cat-file --batch` streams each
// distinct blob once; every pattern's required literals are found in one pass
// over it (literalMatcher), and only lines holding a pattern's literal are
// decided, by the same decision git makes for that pattern:
//
//   - a pattern with no regex-special byte is a byte substring search, as in
//     git (grep.c, is_fixed: compiled as a literal, not as a regex);
//   - any other pattern runs through the platform's POSIX regcomp/regexec with
//     REG_NEWLINE|REG_EXTENDED over the line through REG_STARTEND, as git's
//     regexec_buf does, under the character-type locale a git child would
//     use; an engine error counts as no match, as in git;
//   - a file is skipped as binary when its `diff` attribute says so, or, with
//     no decision there, when a NUL byte is in its first 8000 bytes (git's
//     grep_source_is_binary and buffer_is_binary);
//   - only regular files are searched, never symlinks or submodules;
//   - lines end at LF; a final line without LF counts, and the empty tail
//     after a final LF counts only where git's look-ahead reaches it
//     (phantomLine).
//
// Each pattern's own first matching line is kept (blobAnswer), and a file's
// line is the smallest of them, which is the first line matching any.
//
// With options.Answers, a blob's answer to a pattern computed by any earlier
// scan, in any process, is read instead of computed (see TreeScanAnswers).
func scanTreeFirstPatternLines(ctx context.Context, repo, treeish string, patterns []string, options TreeScanOptions, visit func(GrepMatch) error) (TreeScanStats, error) {
	if len(patterns) == 0 {
		return TreeScanStats{Route: TreeScanRouteOnePass}, nil // git grep is never started
	}
	for _, pattern := range patterns {
		if strings.IndexByte(pattern, 0) >= 0 {
			return TreeScanStats{}, fmt.Errorf("git grep pattern contains a NUL byte")
		}
	}
	plan, err := newTreeScanPlan(gitPatternFileLines(patterns))
	if err != nil {
		return TreeScanStats{}, err
	}
	if plan.hasFixed {
		pcre2, err := gitGrepHasPCRE2(ctx, repo, treeish)
		if err != nil {
			return TreeScanStats{}, err
		}
		if !pcre2 {
			// Without PCRE2, git matches fixed strings through regcomp as basic
			// expressions; only Git itself answers those exactly.
			stats := TreeScanStats{Route: TreeScanRouteGitGrep, Patterns: len(plan.patterns)}
			return stats, grepTreeFirstPatternLinesGit(ctx, repo, treeish, patterns, visit)
		}
	}
	stats := TreeScanStats{Route: TreeScanRouteOnePass, Patterns: len(plan.patterns)}
	entries, err := listTreeRegularFiles(ctx, repo, treeish)
	if err != nil {
		return stats, err
	}
	stats.FilesExamined = len(entries)
	if len(entries) == 0 || len(plan.patterns) == 0 {
		return stats, nil
	}
	if err := treeBinaryDecisions(ctx, repo, treeish, entries); err != nil {
		return stats, err
	}
	// One read per distinct blob; a blob is skipped only when every path that
	// holds it is binary by attribute.
	var blobs []string
	needed := make(map[string]bool)
	for _, entry := range entries {
		if entry.binary == attrBinary || needed[entry.oid] {
			continue
		}
		needed[entry.oid] = true
		blobs = append(blobs, entry.oid)
	}
	answers, err := answerBlobs(ctx, repo, blobs, plan, options, &stats, true)
	if err != nil {
		return stats, err
	}
	sort.Slice(entries, func(a, b int) bool { return entries[a].path < entries[b].path })
	for _, entry := range entries {
		if entry.binary == attrBinary {
			continue
		}
		answer, ok := answers[entry.oid]
		if !ok || (entry.binary == attrUndecided && answer.nulInHead) {
			continue
		}
		line, ok := answer.first()
		if !ok {
			continue
		}
		if err := visit(GrepMatch{Path: entry.path, Text: line.text}); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// answerBlobs answers every blob for every pattern of plan, from the store
// where it can and by scanning the rest. Before scanning, it claims each
// pattern it lacks answers for; a pattern another process holds is waited
// for, and the blobs that need only such patterns are scanned afterwards only
// if that process left them unanswered.
func answerBlobs(ctx context.Context, repo string, blobs []string, plan *treeScanPlan, options TreeScanOptions, stats *TreeScanStats, lineTexts bool) (map[string]blobAnswer, error) {
	store := options.Answers
	// The empty tail git can report (phantomLine) belongs to a whole pattern
	// list, not to one pattern, so such lists are never remembered.
	if plan.matchesEmpty {
		store = nil
	}
	sources := plan.sources()
	answers := make(map[string]blobAnswer, len(blobs))
	known := store.lookup(sources, blobs)
	lacks := func(oid string, index int) bool {
		_, ok := known.answers[oid][index]
		return !ok
	}
	ruledOut := narrowByTextIndex(ctx, blobs, plan, known, lacks, options.Candidates, stats)
	var claims []*answerClaim
	releaseClaims := func() {
		for _, claim := range claims {
			claim.release()
		}
		claims = nil
	}
	defer releaseClaims()
	mine := make([]bool, len(sources))
	var theirs []string
	for index, source := range sources {
		lacking := false
		for _, oid := range blobs {
			if lacks(oid, index) {
				lacking = true
				break
			}
		}
		if !lacking {
			continue
		}
		claim, held := store.tryClaim(source)
		if held {
			theirs = append(theirs, source)
			continue
		}
		mine[index] = true
		if claim != nil {
			claims = append(claims, claim)
		}
	}
	// scan reads oids once and computes, for all of them, every pattern any
	// of them lacks; remembered answers fill in the rest.
	scan := func(oids []string) error {
		if len(oids) == 0 {
			return nil
		}
		var lacking []int
		for index := range sources {
			for _, oid := range oids {
				if lacks(oid, index) {
					lacking = append(lacking, index)
					break
				}
			}
		}
		sub := plan
		if len(lacking) < len(sources) {
			subSources := make([]string, len(lacking))
			for k, index := range lacking {
				subSources[k] = sources[index]
			}
			var err error
			if sub, err = newTreeScanPlan(subSources); err != nil {
				return err
			}
		}
		scanned, read, err := scanBlobs(ctx, repo, oids, sub)
		stats.BlobsRead += read.blobs
		stats.BytesRead += read.bytes
		if err != nil {
			return err
		}
		for oid, partial := range scanned {
			answer := blobAnswer{nulInHead: partial.nulInHead, lines: make([]blobLine, len(sources)), phantom: partial.phantom}
			for index, line := range known.answers[oid] {
				answer.lines[index] = line
			}
			stats.AnswerHits += len(known.answers[oid]) - ruledOut[oid]
			for k, index := range lacking {
				if lacks(oid, index) && store != nil {
					stats.AnswerMisses++
				}
				answer.lines[index] = partial.lines[k]
			}
			answers[oid] = answer
		}
		store.record(sub.sources(), scanned)
		store.evict()
		return nil
	}
	var now, later []string
	for _, oid := range blobs {
		ownWork, waits := false, false
		for index := range sources {
			if lacks(oid, index) {
				if mine[index] {
					ownWork = true
				} else {
					waits = true
				}
			}
		}
		switch {
		case ownWork:
			now = append(now, oid)
		case waits:
			later = append(later, oid)
		}
	}
	if err := scan(now); err != nil {
		return nil, err
	}
	releaseClaims()
	if len(later) > 0 {
		for _, source := range theirs {
			waited, err := store.awaitClaim(ctx, source)
			if err != nil {
				return nil, err
			}
			if waited {
				stats.AnswerWaits++
			}
		}
		again := store.lookup(sources, later)
		var rest []string
		for _, oid := range later {
			if len(again.answers[oid]) < len(sources) {
				rest = append(rest, oid) // the holder covered other blobs, or failed
				continue
			}
			known.answers[oid] = again.answers[oid]
			known.nul[oid] = again.nul[oid]
		}
		if err := scan(rest); err != nil {
			return nil, err
		}
	}
	for _, oid := range blobs {
		if _, scanned := answers[oid]; scanned {
			continue
		}
		answer := blobAnswer{nulInHead: known.nul[oid], lines: make([]blobLine, len(sources))}
		for index, line := range known.answers[oid] {
			answer.lines[index] = line
		}
		answers[oid] = answer
		stats.AnswerHits += len(sources) - ruledOut[oid]
	}
	if !lineTexts {
		return answers, nil // only whether each pattern matched is needed
	}
	if err := resolveLineTexts(ctx, repo, answers, plan, stats); err != nil {
		return nil, err
	}
	return answers, nil
}

// resolveLineTexts reads back, from their blobs, the texts of the lines a
// search will report that the store kept by place only. A remembered place
// that does not hold a whole line of that length is treated as damage: the
// blob is scanned again, which answers it from its content alone.
func resolveLineTexts(ctx context.Context, repo string, answers map[string]blobAnswer, plan *treeScanPlan, stats *TreeScanStats) error {
	var oids []string
	for oid, answer := range answers {
		if line, ok := answer.first(); ok && !line.stored {
			oids = append(oids, oid)
		}
	}
	if len(oids) == 0 {
		return nil
	}
	sort.Strings(oids)
	var damaged []string
	err := readBlobs(ctx, repo, oids, func(oid string, data []byte) error {
		stats.BlobsRead++
		stats.BytesRead += int64(len(data))
		answer := answers[oid]
		line, _ := answer.first()
		start, end := line.offset, line.offset+int64(line.size)
		if start < 0 || end > int64(len(data)) || (start > 0 && data[start-1] != '\n') ||
			(end < int64(len(data)) && data[end] != '\n') || bytes.IndexByte(data[start:end], '\n') >= 0 ||
			bytes.Count(data[:start], []byte{'\n'}) != line.number-1 {
			damaged = append(damaged, oid)
			return nil
		}
		text := string(data[start:end])
		for index := range answer.lines {
			if answer.lines[index].number == line.number {
				answer.lines[index].text, answer.lines[index].stored = text, true
			}
		}
		return nil
	})
	if err != nil || len(damaged) == 0 {
		return err
	}
	rescanned, read, err := scanBlobs(ctx, repo, damaged, plan)
	stats.BlobsRead += read.blobs
	stats.BytesRead += read.bytes
	if err != nil {
		return err
	}
	for oid, answer := range rescanned {
		answers[oid] = answer
	}
	return nil
}

// narrowByTextIndex asks the text index which blobs can match and answers
// "no match" for the rest without reading them. Only a blob all of whose
// unanswered patterns have literals is narrowed: a pattern with no literal
// can match a line the index knows nothing about. It returns, per blob, how
// many answers it supplied, and leaves the scan as it was on any failure.
func narrowByTextIndex(ctx context.Context, blobs []string, plan *treeScanPlan, known knownAnswers, lacks func(string, int) bool, candidates TreeScanCandidates, stats *TreeScanStats) map[string]int {
	ruledOut := map[string]int{}
	if candidates == nil {
		return ruledOut
	}
	literals := make([][]string, len(plan.patterns))
	usable := make([]bool, len(plan.patterns))
	for index, pattern := range plan.patterns {
		if pattern.require == nil {
			continue
		}
		usable[index] = true
		for _, required := range pattern.require {
			literal, ok := indexLiteral(required)
			if !ok {
				usable[index], literals[index] = false, nil
				break
			}
			literals[index] = append(literals[index], literal)
		}
	}
	var narrowable []string
	wanted := map[string]bool{}
	anyLacking := false
	for _, oid := range blobs {
		lacking, allUsable := 0, true
		for index := range plan.patterns {
			if lacks(oid, index) {
				lacking++
				allUsable = allUsable && usable[index]
			}
		}
		if lacking == 0 {
			continue
		}
		anyLacking = true
		if !allUsable {
			continue
		}
		narrowable = append(narrowable, oid)
		for index := range plan.patterns {
			if lacks(oid, index) {
				for _, literal := range literals[index] {
					wanted[literal] = true
				}
			}
		}
	}
	if len(narrowable) == 0 {
		if anyLacking {
			stats.FallbackReason = "no-trigram"
		}
		return ruledOut
	}
	query := make([]string, 0, len(wanted))
	for literal := range wanted {
		query = append(query, literal)
	}
	sort.Strings(query)
	hits, lookups, err := candidates(ctx, narrowable, query)
	if err != nil {
		stats.FallbackReason = "index-error"
		return ruledOut
	}
	stats.TextIndexLookups += lookups
	if lookups == 0 {
		stats.FallbackReason = "index-absent"
		return ruledOut
	}
	stats.TextIndexCandidates += len(hits)
	for _, oid := range narrowable {
		if hits[oid] {
			continue
		}
		byPattern := known.answers[oid]
		if byPattern == nil {
			byPattern = map[int]blobLine{}
			known.answers[oid] = byPattern
		}
		for index := range plan.patterns {
			if _, ok := byPattern[index]; !ok {
				byPattern[index] = blobLine{} // no line can hold any literal
				ruledOut[oid]++
			}
		}
		stats.TextIndexExcluded++
	}
	if stats.TextIndexExcluded > 0 {
		stats.Route = TreeScanRouteTextIndex
	}
	return ruledOut
}

type treeScanPattern struct {
	source  string
	fixed   []byte // non-nil for a git-fixed pattern, matched as bytes
	require []string
	// icase: fixed is matched ignoring ASCII case (an icaseFixedSource).
	icase bool
	// highByte: a line holding any byte at or above 0x80 (highByteSource).
	highByte bool
}

// Two kinds of pattern the scan answers besides git's own, named so no git
// pattern can be one (git refuses a NUL byte in a pattern): a fixed string
// matched ignoring ASCII case, and a line holding a byte at or above 0x80.
// Like any pattern, their answers are remembered per blob.
const (
	icaseFixedPrefix = "\x00icase-fixed\x00"
	highByteSource   = "\x00high-byte\x00"
)

func icaseFixedSource(pattern string) string { return icaseFixedPrefix + pattern }

type treeScanPlan struct {
	patterns []treeScanPattern
	matcher  *literalMatcher
	always   []int // patterns with no literal requirement
	hasFixed bool
	// matchesEmpty is set when some expression matches an empty line, which
	// lets git report the empty tail of a file (phantomLine).
	matchesEmpty bool
}

func (p *treeScanPlan) sources() []string {
	sources := make([]string, len(p.patterns))
	for i, pattern := range p.patterns {
		sources[i] = pattern.source
	}
	return sources
}

// gitPatternFileLines is the pattern list git grep reads from `-f -` when it
// is given patterns joined by LF, as the Git route sends them: one pattern
// per line, a trailing CR dropped (strbuf_getline) and empty lines ignored
// (builtin/grep.c, file_callback).
func gitPatternFileLines(patterns []string) []string {
	var lines []string
	for _, line := range strings.Split(strings.Join(patterns, "\n"), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func newTreeScanPlan(patterns []string) (*treeScanPlan, error) {
	if len(patterns) == 0 {
		return nil, fmt.Errorf("git grep: no pattern given")
	}
	plan := &treeScanPlan{}
	seen := make(map[string]bool, len(patterns))
	for _, source := range patterns {
		if seen[source] {
			continue
		}
		seen[source] = true
		p := treeScanPattern{source: source}
		switch {
		case source == highByteSource:
			p.highByte = true
			plan.patterns = append(plan.patterns, p)
			continue
		case strings.HasPrefix(source, icaseFixedPrefix):
			pattern := strings.TrimPrefix(source, icaseFixedPrefix)
			if pattern == "" || !isASCII(pattern) || strings.IndexByte(pattern, 0) >= 0 || strings.IndexByte(pattern, '\n') >= 0 {
				return nil, fmt.Errorf("an ASCII case-insensitive pattern must be ASCII, without NUL or LF")
			}
			p.icase, p.fixed = true, []byte(asciiOnlyLower(pattern))
			p.require = []string{asciiOnlyLower(pattern)}
			plan.patterns = append(plan.patterns, p)
			continue
		case strings.IndexByte(source, 0) >= 0:
			return nil, fmt.Errorf("git grep pattern contains a NUL byte")
		}
		if isGitFixedPattern(source) {
			plan.hasFixed = true
			p.fixed = []byte(source)
			if isASCII(source) {
				p.require = []string{asciiOnlyLower(source)}
			}
		} else {
			p.require = patternLiteralRequirement(source)
		}
		plan.patterns = append(plan.patterns, p)
	}
	requirements := make([][]string, len(plan.patterns))
	for i, p := range plan.patterns {
		requirements[i] = p.require
		if p.require == nil {
			plan.always = append(plan.always, i)
		}
	}
	plan.matcher = newLiteralMatcher(requirements)
	// Refuse an expression git would refuse before any blob is read. A fixed
	// pattern is never empty here, so only an expression can match an empty
	// line.
	release := scanThread()
	defer release()
	for _, p := range plan.patterns {
		if p.fixed != nil || p.highByte {
			continue
		}
		re, err := compilePOSIXRegex(p.source)
		if err != nil {
			return nil, err
		}
		if re.match(nil) {
			plan.matchesEmpty = true
		}
		re.free()
	}
	return plan, nil
}

// isGitFixedPattern is grep.c's is_fixed: no byte of `$()*+.?[\^{|`.
func isGitFixedPattern(pattern string) bool {
	return !strings.ContainsAny(pattern, "$()*+.?[\\^{|")
}

// containsFoldASCII reports whether line holds lower, a lowercase ASCII
// string, ignoring ASCII case.
func containsFoldASCII(line, lower []byte) bool {
	if len(lower) == 0 {
		return true
	}
	first := lower[0]
	upper := first
	if first >= 'a' && first <= 'z' {
		upper = first - ('a' - 'A')
	}
	for i := 0; i+len(lower) <= len(line); i++ {
		if c := line[i]; c != first && c != upper {
			continue
		}
		match := true
		for k := 1; k < len(lower); k++ {
			if foldASCII(line[i+k]) != lower[k] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func asciiOnlyLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		b[i] = foldASCII(c)
	}
	return string(b)
}

// blobScanner holds one worker's compiled expressions; it lives on one locked
// thread.
type blobScanner struct {
	plan    *treeScanPlan
	regexes []*posixRegex
	pending []int32
	marked  []int
}

func newBlobScanner(plan *treeScanPlan) (*blobScanner, error) {
	s := &blobScanner{plan: plan, regexes: make([]*posixRegex, len(plan.patterns)), marked: make([]int, len(plan.patterns))}
	for i, p := range plan.patterns {
		if p.fixed != nil || p.highByte {
			continue
		}
		re, err := compilePOSIXRegex(p.source)
		if err != nil {
			s.free()
			return nil, err
		}
		s.regexes[i] = re
	}
	return s, nil
}

func (s *blobScanner) free() {
	for _, re := range s.regexes {
		if re != nil {
			re.free()
		}
	}
}

func (s *blobScanner) matches(pattern int, line []byte) bool {
	p := &s.plan.patterns[pattern]
	switch {
	case p.highByte:
		for _, c := range line {
			if c >= 0x80 {
				return true
			}
		}
		return false
	case p.icase:
		return containsFoldASCII(line, p.fixed)
	}
	if fixed := p.fixed; fixed != nil {
		return bytes.Contains(line, fixed)
	}
	return s.regexes[pattern].match(line)
}

// scan answers every pattern for one blob, stopping once each has its first
// matching line. A long blob checks for cancellation as it goes, so a scan
// overruns its deadline by at most a few thousand lines.
func (s *blobScanner) scan(ctx context.Context, data []byte) (blobAnswer, error) {
	plan := s.plan
	answer := blobAnswer{
		nulInHead: bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0,
		lines:     make([]blobLine, len(plan.patterns)),
	}
	unresolved := len(plan.patterns)
	for i := range s.marked {
		s.marked[i] = 0
	}
	state := int32(0)
	number := 1
	start := 0
	for start < len(data) && unresolved > 0 {
		if number%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return blobAnswer{}, err
			}
		}
		end := bytes.IndexByte(data[start:], '\n')
		if end < 0 {
			end = len(data)
		} else {
			end += start
		}
		line := data[start:end]
		s.pending = s.pending[:0]
		for _, b := range line {
			var outputs []int32
			state, outputs = plan.matcher.step(state, b)
			for _, p := range outputs {
				if answer.lines[p].number == 0 && s.marked[p] != number {
					s.marked[p] = number
					s.pending = append(s.pending, p)
				}
			}
		}
		// No literal spans a line: LF is never part of one.
		state = 0
		decide := func(p int) {
			if answer.lines[p].number == 0 && s.matches(p, line) {
				answer.lines[p] = blobLine{number: number, offset: int64(start), size: len(line), text: string(line), stored: true}
				unresolved--
			}
		}
		for _, p := range s.pending {
			decide(int(p))
		}
		for _, p := range plan.always {
			decide(p)
		}
		start = end + 1
		number++
	}
	if plan.matchesEmpty && len(data) > 0 && data[len(data)-1] == '\n' {
		if _, matched := answer.first(); !matched {
			phantom, err := s.phantomLine(ctx, data)
			if err != nil {
				return blobAnswer{}, err
			}
			if phantom {
				answer.phantom = bytes.Count(data, []byte{'\n'}) + 1
			}
		}
	}
	return answer, nil
}

// phantomLine replays git's per-file loop (grep.c, grep_source_1 with
// look_ahead) for a file that ends with LF and no line of which matched, and
// reports whether git then matches the empty tail after the last LF.
//
// look_ahead runs every pattern over the rest of the file as one buffer and
// moves to the line where the earliest match starts. An expression that
// matches the empty string matches at the very end, after the final LF, and
// when that is the earliest match git moves there and matches an empty line
// that is not in the file. An engine error turns the look-ahead off for the
// rest of the file, and "no match" ends the file. Fixed patterns cannot match
// here: any occurrence lies on a line, and none matched.
func (s *blobScanner) phantomLine(ctx context.Context, data []byte) (bool, error) {
	bol := 0
	for bol < len(data) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		earliest := -1
		for _, re := range s.regexes {
			if re == nil {
				continue
			}
			rc, start := re.matchFrom(data[bol:])
			if rc < 0 {
				return false, nil
			}
			if rc == 1 && (earliest < 0 || start < earliest) {
				earliest = start
			}
		}
		if earliest < 0 {
			return false, nil
		}
		lineStart := bol + earliest
		for lineStart > bol && data[lineStart-1] != '\n' {
			lineStart--
		}
		if lineStart == len(data) {
			for _, re := range s.regexes {
				if re != nil && re.match(nil) {
					return true, nil
				}
			}
			return false, nil
		}
		// That line did not match; git resumes after it.
		end := bytes.IndexByte(data[lineStart:], '\n')
		if end < 0 {
			return false, nil
		}
		bol = lineStart + end + 1
	}
	return false, nil
}

type blobReadCounts struct {
	blobs int
	bytes int64
}

// scanBlobs streams blobs through one `git cat-file --batch` and answers them
// on the tree scan's worker budget.
func scanBlobs(ctx context.Context, repo string, blobs []string, plan *treeScanPlan) (map[string]blobAnswer, blobReadCounts, error) {
	answers := make(map[string]blobAnswer, len(blobs))
	var counts blobReadCounts
	if len(blobs) == 0 {
		return answers, counts, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type job struct {
		oid  string
		data []byte
	}
	workers := max(1, min(treePatternGrepBudget().CPU, len(blobs)))
	jobs := make(chan job, workers*2)
	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		mu.Unlock()
	}
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release := scanThread()
			defer release()
			scanner, err := newBlobScanner(plan)
			if err != nil {
				fail(err)
				for range jobs {
				}
				return
			}
			defer scanner.free()
			for j := range jobs {
				if ctx.Err() != nil {
					continue
				}
				answer, err := scanner.scan(ctx, j.data)
				if err != nil {
					fail(err)
					continue
				}
				mu.Lock()
				answers[j.oid] = answer
				mu.Unlock()
			}
		}()
	}

	readErr := readBlobs(ctx, repo, blobs, func(oid string, data []byte) error {
		counts.blobs++
		counts.bytes += int64(len(data))
		select {
		case jobs <- job{oid: oid, data: data}:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return nil, counts, firstErr
	}
	if readErr != nil {
		return nil, counts, readErr
	}
	if err := ctx.Err(); err != nil {
		return nil, counts, err
	}
	return answers, counts, nil
}

var (
	pcre2Mu    sync.Mutex
	pcre2Known = map[string]bool{}
)

// gitGrepHasPCRE2 reports whether this git was built with PCRE2, which
// decides how it matches fixed strings (grep.c, compile_regexp). A build
// without it refuses -P before reading anything, so the probe costs one
// short process, once per git on PATH.
func gitGrepHasPCRE2(ctx context.Context, repo, treeish string) (bool, error) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return false, err
	}
	pcre2Mu.Lock()
	known, ok := pcre2Known[gitPath]
	pcre2Mu.Unlock()
	if ok {
		return known, nil
	}
	cmd := newCmd(ctx, repo, "git", "grep", "-q", "-P", "-e", "x", treeish, "--", ":(literal)\x01graph-pcre2-probe")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	var exitError *exec.ExitError
	switch {
	case err == nil || (errors.As(err, &exitError) && exitError.ExitCode() == 1 && stderr.Len() == 0):
		known = true
	case strings.Contains(stderr.String(), "PCRE"):
		known = false
	default:
		return false, fmt.Errorf("git grep -P probe: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	pcre2Mu.Lock()
	pcre2Known[gitPath] = known
	pcre2Mu.Unlock()
	return known, nil
}

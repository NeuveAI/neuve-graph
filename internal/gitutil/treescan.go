//go:build cgo && !windows

package gitutil

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
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
func scanTreeFirstPatternLines(ctx context.Context, repo, treeish string, patterns []string, visit func(GrepMatch) error) (TreeScanStats, error) {
	if len(patterns) == 0 {
		return TreeScanStats{Route: TreeScanRouteOnePass}, nil // git grep is never started
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
	answers, read, err := scanBlobs(ctx, repo, blobs, plan)
	stats.BlobsRead, stats.BytesRead = read.blobs, read.bytes
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

type treeScanPattern struct {
	source  string
	fixed   []byte // non-nil for a git-fixed pattern, matched as bytes
	require []string
}

type treeScanPlan struct {
	patterns []treeScanPattern
	matcher  *literalMatcher
	always   []int // patterns with no literal requirement
	hasFixed bool
	// matchesEmpty is set when some expression matches an empty line, which
	// lets git report the empty tail of a file (phantomLine).
	matchesEmpty bool
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
		if strings.IndexByte(source, 0) >= 0 {
			return nil, fmt.Errorf("git grep pattern contains a NUL byte")
		}
		p := treeScanPattern{source: source}
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
		if p.fixed != nil {
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

// blobLine is a 1-based line number and the line's text without its LF.
type blobLine struct {
	number int
	text   string
}

// blobAnswer is one blob's result for every pattern of a plan.
type blobAnswer struct {
	nulInHead bool
	// lines[pattern] is that pattern's first matching line; number 0 is none.
	lines []blobLine
	// phantom is the line number git reports for the empty tail of a file no
	// line of which matched (phantomLine), or 0. It belongs to the whole
	// plan, not to one pattern.
	phantom int
}

func (a blobAnswer) first() (blobLine, bool) {
	best := blobLine{}
	for _, line := range a.lines {
		if line.number > 0 && (best.number == 0 || line.number < best.number) {
			best = line
		}
	}
	if best.number == 0 && a.phantom > 0 {
		return blobLine{number: a.phantom}, true
	}
	return best, best.number > 0
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
		if p.fixed != nil {
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
	if fixed := s.plan.patterns[pattern].fixed; fixed != nil {
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
				answer.lines[p] = blobLine{number: number, text: string(line)}
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

// readBlobs writes oids to `git cat-file --batch` from one goroutine and
// reads each object back in order.
func readBlobs(ctx context.Context, repo string, oids []string, visit func(oid string, data []byte) error) error {
	cmd := newCmd(ctx, repo, "git", "cat-file", "--batch")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	writeDone := make(chan error, 1)
	go func() {
		w := bufio.NewWriter(stdin)
		var werr error
		for _, oid := range oids {
			if _, werr = w.WriteString(oid + "\n"); werr != nil {
				break
			}
		}
		if werr == nil {
			werr = w.Flush()
		}
		_ = stdin.Close()
		writeDone <- werr
	}()
	reader := bufio.NewReaderSize(stdout, 1<<20)
	readErr := func() error {
		for _, want := range oids {
			header, err := reader.ReadString('\n')
			if err != nil {
				return fmt.Errorf("git cat-file batch header: %w", err)
			}
			fields := strings.Fields(header)
			if len(fields) != 3 || fields[0] != want || fields[1] != "blob" {
				return fmt.Errorf("git cat-file returned %q for blob %s", strings.TrimSpace(header), want)
			}
			size, err := strconv.ParseInt(fields[2], 10, 64)
			if err != nil || size < 0 {
				return fmt.Errorf("git cat-file returned malformed size %q", fields[2])
			}
			data := make([]byte, size)
			if _, err := io.ReadFull(reader, data); err != nil {
				return fmt.Errorf("git cat-file blob %s: %w", want, err)
			}
			if b, err := reader.ReadByte(); err != nil || b != '\n' {
				return fmt.Errorf("git cat-file blob %s: missing terminator", want)
			}
			if err := visit(want, data); err != nil {
				return err
			}
		}
		return nil
	}()
	if readErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	writeErr := <-writeDone
	if readErr != nil {
		return readErr
	}
	if waitErr != nil {
		return fmt.Errorf("git cat-file --batch: %w: %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	if writeErr != nil && !errors.Is(writeErr, io.ErrClosedPipe) {
		return writeErr
	}
	return nil
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

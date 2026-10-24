//go:build cgo && !windows

package gitutil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// unicodeFoldRunes are the characters through which git's Unicode case
// folding (PCRE2 with PCRE2_UTF) can match ASCII letters where ASCII folding
// does not: the Kelvin sign and the long s fold to k and s, and the dotted
// and dotless i are the partners of i and I.
var unicodeFoldRunes = []string{"\u212a", "\u017f", "\u0130", "\u0131"}

// foldCharacters are the characters a file must hold for git to match it
// where ASCII folding matched no pattern, or nil when any non-ASCII byte may:
//
//   - where git folds Unicode case, the characters that fold to ASCII letters
//     and every non-ASCII character of the patterns (and the partners those
//     fold with, which are ASCII or listed);
//   - in the C locale, git's tables flip ASCII case only, so a pattern's
//     non-ASCII characters match only themselves;
//   - in any other locale, its tables may flip any byte: nil.
func foldCharacters(patterns []string, unicode bool) []string {
	if !unicode && !cLocale() {
		return nil
	}
	seen := map[string]bool{}
	var runes []string
	add := func(r string) {
		if !seen[r] {
			seen[r] = true
			runes = append(runes, r)
		}
	}
	if unicode {
		for _, r := range unicodeFoldRunes {
			add(r)
		}
	}
	for _, pattern := range patterns {
		for _, r := range pattern {
			if r >= 0x80 {
				add(string(r))
			}
		}
	}
	return runes
}

// cLocale reports whether a git child's character-type locale is C, as
// setlocale(LC_CTYPE, "") resolves it: the first of LC_ALL, LC_CTYPE and LANG
// that is set, or none.
func cLocale() bool {
	for _, name := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if value := os.Getenv(name); value != "" {
			return value == "C" || value == "POSIX"
		}
	}
	return true
}

// scanTreePaths answers what `git grep -I -i -F -l` over patterns answers on
// an immutable tree (GrepTreePaths: every regular, non-binary file holding a
// pattern, ignoring case, in path byte order), reading every blob once and
// remembering each blob's answer per pattern.
//
// With PCRE2, git matches a case-insensitive fixed string through PCRE2 with
// PCRE2_CASELESS, and in a UTF-8 locale also with PCRE2_UTF, where Unicode
// case folding applies (an ASCII "k" also matches the Kelvin sign). So:
//
//   - in a file of ASCII bytes only, git's answer is ASCII case folding, for
//     every pattern: a pattern with non-ASCII characters matches ASCII text
//     only where the ASCII term it folds to matches too, so it adds no file;
//   - in any file, an ASCII pattern found ignoring ASCII case is found by git;
//   - a file holding non-ASCII bytes that no ASCII pattern matched is left to
//     git itself, with every pattern, over just those files.
//
// Without PCRE2 git matches through regcomp with REG_ICASE under the locale,
// which only git answers exactly: the whole search is git's then.
func scanTreePaths(ctx context.Context, repo, treeish string, patterns []string, options TreeScanOptions) ([]string, TreeScanStats, error) {
	gitAnswers := func() ([]string, TreeScanStats, error) {
		paths, err := grepTreePaths(ctx, repo, treeish, patterns, true, false, nil)
		return paths, TreeScanStats{}, err // git's own route, named by the caller
	}
	var ascii, other []string
	seen := map[string]bool{}
	for _, pattern := range patterns {
		if pattern == "" || seen[pattern] {
			continue
		}
		seen[pattern] = true
		// git splits a pattern at LF, and quotes a fixed one for PCRE2 as
		// \Q...\E, which a pattern holding \E ends early: git answers those.
		if strings.ContainsAny(pattern, "\x00\n") || strings.Contains(pattern, `\E`) {
			return gitAnswers()
		}
		if isASCII(pattern) {
			ascii = append(ascii, pattern)
		} else {
			other = append(other, pattern)
		}
	}
	// A non-ASCII pattern adds no file matched by ASCII folding alone only
	// when its ASCII form is in the list too (as the variants a search sends
	// always are); otherwise git answers.
	asciiFolded := make(map[string]bool, len(ascii))
	for _, pattern := range ascii {
		asciiFolded[asciiOnlyLower(pattern)] = true
	}
	for _, pattern := range other {
		form, ok := asciiForm(pattern)
		if !ok || !asciiFolded[form] {
			return gitAnswers()
		}
	}
	stats := TreeScanStats{Route: TreeScanRoutePaths, Patterns: len(seen)}
	if len(seen) == 0 {
		return []string{}, stats, nil
	}
	// Without PCRE2, or where the probes cannot tell, git answers.
	pcre2, err := gitGrepHasPCRE2(ctx, repo, treeish)
	if err != nil || !pcre2 {
		return gitAnswers()
	}
	entries, err := listTreeRegularFiles(ctx, repo, treeish)
	if err != nil {
		return nil, stats, err
	}
	stats.FilesExamined = len(entries)
	if len(entries) == 0 {
		return []string{}, stats, nil
	}
	if err := treeBinaryDecisions(ctx, repo, treeish, entries); err != nil {
		return nil, stats, err
	}
	var blobs []string
	needed := make(map[string]bool)
	for _, entry := range entries {
		if entry.binary == attrBinary || needed[entry.oid] {
			continue
		}
		needed[entry.oid] = true
		blobs = append(blobs, entry.oid)
	}
	sources := make([]string, 0, len(ascii)+1)
	for _, pattern := range ascii {
		sources = append(sources, icaseFixedSource(pattern))
	}
	sources = append(sources, highByteSource)
	unicode, err := gitFoldsUnicode(ctx, repo)
	if err != nil {
		return gitAnswers()
	}
	folds := len(sources)
	characters := foldCharacters(patterns, unicode)
	sources = append(sources, characters...)
	plan, err := newTreeScanPlan(sources)
	if err != nil {
		return nil, stats, err
	}
	answers, err := answerBlobs(ctx, repo, blobs, plan, options, &stats, false)
	if err != nil {
		return nil, stats, err
	}
	sort.Slice(entries, func(a, b int) bool { return entries[a].path < entries[b].path })
	high := folds - 1
	var matched, deferred []string
	for _, entry := range entries {
		if entry.binary == attrBinary {
			continue
		}
		answer, ok := answers[entry.oid]
		if !ok || (entry.binary == attrUndecided && answer.nulInHead) {
			continue
		}
		found, folding := false, false
		for index := 0; index < high; index++ {
			if answer.lines[index].number > 0 {
				found = true
				break
			}
		}
		for index := folds; index < len(sources); index++ {
			folding = folding || answer.lines[index].number > 0
		}
		switch {
		case found:
			matched = append(matched, entry.path)
		case answer.lines[high].number > 0 && (characters == nil || folding):
			// Only a fold character (foldCharacters) can make git match a
			// file ASCII folding did not.
			deferred = append(deferred, entry.path)
		}
	}
	stats.Deferred = len(deferred)
	if len(deferred) == 0 {
		return matched, stats, nil
	}
	more, err := grepTreePaths(ctx, repo, treeish, patterns, true, false, deferred)
	if err != nil {
		return nil, stats, err
	}
	all := append(matched, more...)
	sort.Strings(all)
	return all, stats, nil
}

var (
	unicodeFoldMu    sync.Mutex
	unicodeFoldKnown = map[string]bool{}
)

// gitFoldsUnicode reports whether git grep -i -F, run now with the caller's
// locale, folds Unicode case: whether "s" matches the long s. Asked once per
// git and locale.
func gitFoldsUnicode(ctx context.Context, repo string) (bool, error) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return false, err
	}
	key := gitPath + "\x00" + os.Getenv("LC_ALL") + "\x00" + os.Getenv("LC_CTYPE") + "\x00" + os.Getenv("LANG")
	unicodeFoldMu.Lock()
	known, ok := unicodeFoldKnown[key]
	unicodeFoldMu.Unlock()
	if ok {
		return known, nil
	}
	dir, err := os.MkdirTemp("", "graph-fold-probe")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(dir)
	probe := filepath.Join(dir, "probe.txt")
	if err := os.WriteFile(probe, []byte("\u017f\n"), 0o600); err != nil {
		return false, err
	}
	cmd := newGitCmdWithCallerLocale(ctx, dir, "grep", "--no-index", "-q", "-i", "-F", "-e", "s", "--", "probe.txt")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err = cmd.Run()
	var exitError *exec.ExitError
	switch {
	case err == nil:
		known = true
	case errors.As(err, &exitError) && exitError.ExitCode() == 1:
		known = false
	default:
		return false, fmt.Errorf("git grep fold probe: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	unicodeFoldMu.Lock()
	unicodeFoldKnown[key] = known
	unicodeFoldMu.Unlock()
	return known, nil
}

// asciiForm is a pattern with each character that case-folds to an ASCII
// letter (unicodeFoldRunes) replaced by that letter, lowercased: the ASCII
// pattern it matches under Unicode folding, if it is all ASCII then.
func asciiForm(pattern string) (string, bool) {
	var b strings.Builder
	for _, r := range pattern {
		switch r {
		case '\u212a':
			r = 'k'
		case '\u017f':
			r = 's'
		case '\u0130', '\u0131':
			r = 'i'
		}
		if r >= 0x80 {
			return "", false
		}
		b.WriteByte(foldASCII(byte(r)))
	}
	return b.String(), true
}

package gitutil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// TreeScanRouteOnePass names the one-pass blob scan in TreeScanStats.Route.
const TreeScanRouteOnePass = "tree-scan"

// TreeScanRouteGitGrep names the per-pattern git grep route, kept for
// platforms without the system regex engine.
const TreeScanRouteGitGrep = "git-tree-grep"

// TreeScanStats reports what one first-line tree scan did.
type TreeScanStats struct {
	Route         string
	Patterns      int   // distinct patterns matched
	FilesExamined int   // regular files in the tree
	BlobsRead     int   // blobs whose content Graph read
	BytesRead     int64 // their total size
}

// attrDecision is git's userdiff verdict on whether a path is binary.
type attrDecision int8

const (
	attrUndecided attrDecision = iota // decided by content
	attrText
	attrBinary
)

type treeScanEntry struct {
	path   string
	oid    string
	binary attrDecision
}

// listTreeRegularFiles lists the regular files of an immutable tree, the
// entries git grep searches (builtin/grep.c, grep_tree: S_ISREG only).
func listTreeRegularFiles(ctx context.Context, repo, treeish string) ([]treeScanEntry, error) {
	out, err := run(ctx, repo, "git", "ls-tree", "-r", "-z", "--full-tree", treeish)
	if err != nil {
		return nil, err
	}
	var entries []treeScanEntry
	for _, record := range strings.Split(out, "\x00") {
		if record == "" {
			continue
		}
		meta, path, ok := strings.Cut(record, "\t")
		if !ok {
			return nil, fmt.Errorf("git ls-tree returned malformed entry %q", record)
		}
		fields := strings.Fields(meta)
		if len(fields) != 3 {
			return nil, fmt.Errorf("git ls-tree returned malformed entry %q", record)
		}
		mode, err := strconv.ParseUint(fields[0], 8, 32)
		if err != nil {
			return nil, fmt.Errorf("git ls-tree returned malformed mode %q", fields[0])
		}
		if mode&0o170000 != 0o100000 || fields[1] != "blob" {
			continue
		}
		entries = append(entries, treeScanEntry{path: path, oid: fields[2]})
	}
	return entries, nil
}

// treeBinaryDecisions applies the attribute step of git grep's
// grep_source_is_binary to each entry: the `diff` attribute selects a driver
// (userdiff_find_by_path) and a driver with a binary setting decides; every
// other entry is decided by content.
//
//   - `diff` set selects git's "diff=true" driver: text. `diff` unset (also
//     through the `binary` macro) selects "!diff": binary.
//   - A named driver that exists, configured (any diff.<name>.* key) or built
//     in, decides by its diff.<name>.binary when that is set.
//   - No `diff` attribute, or a name no driver has, falls back to git's
//     "default" driver, which decides by diff.default.binary when set.
//
// Attributes are read the way git grep reads them for a tree, from the
// repository's attribute files rather than the tree, which is what
// `git check-attr` reports. check-attr prints a literal value "set" or
// "unset" exactly as it prints the states, so for those entries git grep
// itself is asked which it searches (gitSearchedPaths), as it is for a
// literal "unspecified" when a driver of that name exists and for driver names
// whose meaning depends on git's built-in drivers.
func treeBinaryDecisions(ctx context.Context, repo, treeish string, entries []treeScanEntry) error {
	if len(entries) == 0 {
		return nil
	}
	cmd := newCmd(ctx, repo, "git", "check-attr", "-z", "--stdin", "diff")
	var input bytes.Buffer
	for _, entry := range entries {
		input.WriteString(entry.path)
		input.WriteByte(0)
	}
	cmd.Stdin = &input
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git check-attr diff: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	fields := strings.Split(strings.TrimSuffix(stdout.String(), "\x00"), "\x00")
	if len(fields) != 3*len(entries) {
		return fmt.Errorf("git check-attr returned %d fields for %d paths", len(fields), len(entries))
	}
	drivers, err := readDiffDrivers(ctx, repo)
	if err != nil {
		return err
	}
	var ambiguous []int
	for i := range entries {
		path, attr, value := fields[3*i], fields[3*i+1], fields[3*i+2]
		if path != entries[i].path || attr != "diff" {
			return fmt.Errorf("git check-attr answered %q for %q", path, entries[i].path)
		}
		decision, sure := attrUndecided, false
		switch value {
		case "set", "unset":
			// Also how check-attr prints the literal values "set" and "unset".
		case "unspecified":
			// Also how it prints the literal value, which selects a driver of
			// that name when one is configured.
			if !drivers.configured[value] {
				decision, sure = drivers.decide("default")
			}
		default:
			decision, sure = drivers.decide(value)
		}
		if !sure {
			ambiguous = append(ambiguous, i)
			continue
		}
		entries[i].binary = decision
	}
	if len(ambiguous) == 0 {
		return nil
	}
	paths := make([]string, len(ambiguous))
	for k, i := range ambiguous {
		paths[k] = entries[i].path
	}
	searched, err := gitSearchedPaths(ctx, repo, treeish, paths)
	if err != nil {
		return err
	}
	for _, i := range ambiguous {
		// A path git does not search is binary to it, or empty, which has no
		// line to match either way.
		entries[i].binary = attrBinary
		if searched[entries[i].path] {
			entries[i].binary = attrText
		}
	}
	return nil
}

type diffDrivers struct {
	configured map[string]bool         // names with any diff.<name>.* key
	binary     map[string]attrDecision // names with diff.<name>.binary
}

// decide is the binary verdict for the driver a `diff=<name>` path selects,
// and whether it is certain. A configured driver decides by its own setting.
// An unconfigured name selects one of git's built-in drivers, which have no
// binary setting, or, when git has no driver of that name, the "default"
// driver; the two differ only when diff.default.binary is set, and which
// names git builds in depends on its version, so then git is asked.
func (d diffDrivers) decide(name string) (attrDecision, bool) {
	if d.configured[name] {
		return d.binary[name], true // attrUndecided when unset
	}
	if fallback := d.binary["default"]; fallback != attrUndecided && name != "default" {
		return attrUndecided, false
	}
	return d.binary["default"], true
}

// readDiffDrivers reads the configured diff drivers the way userdiff_config
// does: any diff.<name>.<key> defines driver <name>, and its binary key is
// git's tristate (parse_tristate), the last value winning.
func readDiffDrivers(ctx context.Context, repo string) (diffDrivers, error) {
	drivers := diffDrivers{configured: map[string]bool{}, binary: map[string]attrDecision{}}
	cmd := newCmd(ctx, repo, "git", "config", "-z", "--get-regexp", `^diff\.`)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) && exitError.ExitCode() == 1 && stderr.Len() == 0 {
			return drivers, nil // none configured
		}
		return drivers, fmt.Errorf("git config diff drivers: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	for _, record := range strings.Split(stdout.String(), "\x00") {
		if record == "" {
			continue
		}
		key, value, hasValue := strings.Cut(record, "\n")
		rest := strings.TrimPrefix(key, "diff.")
		dot := strings.LastIndexByte(rest, '.')
		if rest == key || dot < 0 {
			continue // diff.<key>: not a driver
		}
		name, kind := rest[:dot], rest[dot+1:]
		drivers.configured[name] = true
		if kind != "binary" {
			continue
		}
		if hasValue && strings.EqualFold(value, "auto") {
			drivers.binary[name] = attrUndecided
			continue
		}
		truth, err := gitConfigBool(value, hasValue)
		if err != nil {
			return drivers, fmt.Errorf("git config %s: %w", key, err)
		}
		drivers.binary[name] = attrText
		if truth {
			drivers.binary[name] = attrBinary
		}
	}
	return drivers, nil
}

// gitConfigBool is git_config_bool: a key with no value is true, an empty
// value false, the boolean words either way, and otherwise an integer (with
// git's k, m or g unit) that is true when non-zero.
func gitConfigBool(value string, hasValue bool) (bool, error) {
	if !hasValue {
		return true, nil
	}
	switch strings.ToLower(value) {
	case "true", "yes", "on":
		return true, nil
	case "false", "no", "off", "":
		return false, nil
	}
	number := value
	if strings.TrimSpace(number) != number || strings.ContainsAny(number, "_oObB") && !strings.HasPrefix(strings.ToLower(number), "0x") {
		return false, fmt.Errorf("%q is not a boolean", value)
	}
	if n := len(number); n > 0 {
		switch number[n-1] {
		case 'k', 'K', 'm', 'M', 'g', 'G':
			number = number[:n-1]
		}
	}
	parsed, err := strconv.ParseInt(number, 0, 64)
	if err != nil {
		return false, fmt.Errorf("%q is not a boolean", value)
	}
	return parsed != 0, nil
}

// gitSearchedPaths asks git grep which of paths it searches at treeish: an
// empty pattern matches every line, so -I -l lists exactly the non-empty
// files it does not judge binary.
func gitSearchedPaths(ctx context.Context, repo, treeish string, paths []string) (map[string]bool, error) {
	searched := make(map[string]bool, len(paths))
	const chunk = 512
	for start := 0; start < len(paths); start += chunk {
		args := []string{"grep", "--no-recurse-submodules", "--no-color", "--no-full-name", "-z", "-I", "-l", "-F", "-e", "", treeish, "--"}
		for _, path := range paths[start:min(start+chunk, len(paths))] {
			args = append(args, ":(literal)"+path)
		}
		cmd := newCmd(ctx, repo, "git", args...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) || exitError.ExitCode() != 1 || stderr.Len() != 0 {
				return nil, fmt.Errorf("git grep searched paths: %w: %s", err, strings.TrimSpace(stderr.String()))
			}
		}
		for _, field := range strings.Split(stdout.String(), "\x00") {
			if field != "" {
				searched[strings.TrimPrefix(field, treeish+":")] = true
			}
		}
	}
	return searched, nil
}

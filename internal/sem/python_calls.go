package sem

import (
	"path"
	"regexp"
	"strings"
)

var pythonDottedCallRe = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*(?:\s*\.\s*[A-Za-z_][A-Za-z0-9_]*)+)\s*\(`)

// pythonDottedCallImportedNames maps each dotted call's terminal name to the
// candidate modules its callable could live in. It returns two maps keyed by
// that terminal name: allModules feeds local symbol resolution (it includes
// the os.path stdlib fan-out), while externalModules feeds the imported
// external-symbol fallback (it excludes the fan-out, which must never become a
// speculative external edge in an ordinary repo).
func pythonDottedCallImportedNames(block string, importsByName map[string][]string, importFormsByName map[string]map[string]pythonImportForm, moduleExists func(module string) bool) (allModules, externalModules map[string][]string) {
	allModules, externalModules, _ = pythonDottedCallImportedNameSites(block, importsByName, importFormsByName, moduleExists)
	return allModules, externalModules
}

// pythonDottedCallImportedNameSites is pythonDottedCallImportedNames with, for
// each terminal name and each module a dotted call gave it, the offset in
// block where the terminal name of the first such call is written. A name's
// modules can come from several spellings (`a.x.fn()`, `b.y.fn()`), so the
// call that produced a relation is the first one whose module the relation's
// target came from.
func pythonDottedCallImportedNameSites(block string, importsByName map[string][]string, importFormsByName map[string]map[string]pythonImportForm, moduleExists func(module string) bool) (allModules, externalModules map[string][]string, sites map[string]map[string]int) {
	stripped := stripPythonLiteralsAndComments(block)
	allModules = map[string][]string{}
	externalModules = map[string][]string{}
	sites = map[string]map[string]int{}
	for _, m := range pythonDottedCallRe.FindAllStringSubmatchIndex(stripped, -1) {
		if len(m) < 4 {
			continue
		}
		parts := pythonDottedPathParts(stripped[m[2]:m[3]])
		if len(parts) < 2 {
			continue
		}
		alias := parts[0]
		imported := importsByName[alias]
		if len(imported) == 0 {
			continue
		}
		name := parts[len(parts)-1]
		tail := parts[1 : len(parts)-1]
		// An empty tail is a single-selector call `alias.name()`. It only names a
		// module the terminal lives in when the alias renames a module (`import x.y
		// as alias`) or is the os.path facade (`from os import path`); a plain
		// `import json; json.dumps()` (module == alias) is a member call resolved
		// elsewhere. pythonSingleSelectorAliasCall makes that distinction.
		if len(tail) == 0 && !pythonSingleSelectorAliasCall(alias, imported) {
			continue
		}
		all, external := pythonDottedCallModules(alias, tail, imported, importFormsByName[alias], moduleExists)
		allModules[name] = uniqueStrings(append(allModules[name], all...))
		externalModules[name] = uniqueStrings(append(externalModules[name], external...))
		if sites[name] == nil {
			sites[name] = map[string]int{}
		}
		for _, module := range append(all, external...) {
			module = strings.TrimSpace(module)
			if _, seen := sites[name][module]; !seen {
				sites[name][module] = m[3] - len(name)
			}
		}
	}
	return allModules, externalModules, sites
}

// pythonDottedCallSite returns the first site of name among the dotted calls
// whose module satisfies match, or -1.
func pythonDottedCallSite(sites map[string]map[string]int, name string, match func(module string) bool) int {
	site := -1
	for module, offset := range sites[name] {
		if match(module) && (site < 0 || offset < site) {
			site = offset
		}
	}
	return site
}

// pythonDottedCallRelations resolves dotted imported-module calls
// (`pkg.mod.fn()`) through a strictly module-scoped path, independent of the
// generic bare-call resolver. The module qualifier is authoritative: a local
// target is accepted only when its file matches one of the imported modules via
// dottedImportedModuleMatchesFile, a dedicated STRICT matcher that admits only
// the module's own source file or that package's __init__ — no parent-directory
// or basename fallback, so a sibling submodule (`acme.mod.run` resolving to
// acme/service.py) can never match. Because this path never consults same-file
// symbols, the globally-unique-name fallback, the parent-directory package, or
// any sibling submodule, none of those can capture a dotted call whose terminal
// name happens to match an unrelated local, workspace-unique, sibling, or
// package-init symbol. When no file matches the imported module the call falls
// back to an imported external edge, exactly as the bare imported-call fallback
// does. A terminal name that has its own bare import binding is left to the
// generic path so a bare-imported call keeps its established behavior.
//
// line says where a relation's call is written: the line of the first dotted
// call of name whose module the relation's target came from, as
// callSiteLines reports it.
func pythonDottedCallRelations(from SymbolRecord, allModules, externalModules, importsByName map[string][]string, symbolsByShortName map[string][]SymbolRecord, childNames map[string]bool, line func(name string, match func(module string) bool) int) []RelationRecord {
	if typeLikeKind(from.Kind) {
		return nil
	}
	var relations []RelationRecord
	for _, name := range sortedKeysOf(allModules) {
		if name == from.Name || childNames[name] {
			continue
		}
		// Collision rule: a bare import of the terminal name owns the name's
		// resolution and external fallback; forgo the dotted edge so the bare
		// call keeps its established (pre-dotted-call) behavior.
		if len(importsByName[name]) > 0 {
			continue
		}
		modules := allModules[name]
		var local []RelationRecord
		// hadLocalCandidate records whether ANY reachable in-repo Python symbol of
		// the terminal name lives in the imported module or one of its ancestor
		// packages — a genuine in-repo target of this dotted call — BEFORE the kind
		// gate below excludes methods/fields. The dotted tail of
		// `pkg.mod.Class.method()` mixes submodule and class segments; the strict
		// module matcher treats every tail segment as a module path, so a method
		// whose class is a trailing tail segment resolves against an ANCESTOR
		// module's file (`myapp.models.User.save` -> myapp/models.py). When such a
		// member matched but was kind-gated out of the local edge, the external
		// fallback must NOT fire: the call targets an in-repo member and the
		// generic receiver path already emits the correct edge for it.
		hadLocalCandidate := false
		for _, to := range symbolsByShortName[name] {
			// symbolsByShortName is a workspace-wide index across every language, so
			// a Python dotted call whose terminal name and dotted module stem
			// collide with a same-path symbol in another language (`acme.mod.run()`
			// vs a Go `func run` in acme/mod.go) would otherwise resolve
			// cross-language — the sibling Haskell and Perl structural paths gate on
			// to.Language for the same reason, so require the target itself be
			// Python here. An unreachable nested/closure local is never a candidate.
			if to.ID == from.ID || to.Language != "Python" || !localReachable(from, to) {
				continue
			}
			// A local Python symbol of ANY kind whose own SOURCE file is the imported
			// module or an ancestor module (a class member's file, reached by
			// dropping trailing class segments of the dotted tail) is an in-repo
			// target of this dotted call. Record it before the kind gate so a
			// kind-excluded method/field cannot be relabeled as an external call by
			// the fallback below.
			if dottedImportedModuleSourceFileIsAncestor(modules, to.FilePath) {
				hadLocalCandidate = true
			}
			// A bare `name()` call is a function, not a class method (methods carry
			// an explicit receiver and are resolved elsewhere), so mirror the
			// generic resolver's kind gate for Python.
			if to.Kind == "field" || to.Kind == "method" {
				continue
			}
			if !dottedImportedModuleMatchesFile(modules, to.FilePath) {
				continue
			}
			relType := "CALLS"
			if typeLikeKind(to.Kind) {
				relType = "CONSTRUCTS"
			}
			targetFile := to.FilePath
			local = append(local, RelationRecord{
				RecordType:    "relation",
				FromID:        from.ID,
				ToID:          to.ID,
				Type:          relType,
				Confidence:    0.86,
				Reason:        "direct call expression resolved through import path",
				RelationScope: "module",
				Resolution:    "import_resolved",
				TargetKind:    "symbol",
				Evidence: []Evidence{callSiteEvidence("call_site", from, name, line(name, func(module string) bool {
					return dottedImportedModuleMatchesFile([]string{module}, targetFile)
				}))},
				WarningCodes: []string{},
			})
		}
		if len(local) > 0 {
			relations = append(relations, local...)
			continue
		}
		if hadLocalCandidate {
			// A local Python symbol matched the imported module (or an ancestor
			// package) and the terminal name but was excluded from the dotted edge
			// by the kind gate (a method/field, resolved through the generic
			// receiver path). The call targets an in-repo member, so the dotted path
			// emits nothing rather than fabricate an external edge to an in-repo
			// target — the generic pipeline still emits the correct local edge.
			continue
		}
		// importedExternalCallRelationsForName, one module at a time so each
		// external target carries the line of the call its module came from.
		seenTargets := map[string]bool{}
		for _, module := range externalModules[name] {
			target := importedExternalSymbolName(module, name)
			if target == "" || seenTargets[target] {
				continue
			}
			seenTargets[target] = true
			relation := importedExternalCallRelation(from, target, name)
			module := strings.TrimSpace(module)
			relation.Evidence[0] = callSiteEvidence("imported_call_site", from, name, line(name, func(candidate string) bool { return candidate == module }))
			relations = append(relations, relation)
		}
	}
	return relations
}

func pythonDottedPathParts(path string) []string {
	raw := strings.Split(path, ".")
	parts := make([]string, 0, len(raw))
	for _, part := range raw {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil
		}
		parts = append(parts, part)
	}
	return parts
}

// pythonDottedCallModules resolves the candidate modules for a dotted call's
// terminal name. It returns (all, external): all is the full candidate set used
// for local resolution; external is the subset eligible to become an imported
// external-symbol edge. They differ only in the os.path stdlib fan-out, which
// is local-only.
//
// The syntactic FORM of each import, recorded at parse time (forms[module]),
// drives composition without guessing it back from the module string — three
// audit rounds of false edges came from that guess. `from pkg import service`
// and `import pkg as service` record string-identical importsByName bindings but
// mean different things; the form disambiguates them at the source:
//
//   - pythonPlainImport (`import a.b`, alias is the module's leading segment):
//     the literal dotted call path `alias.<tail>` IS the module the terminal
//     lives in (`import pkg.a` + pkg.a.b.fn -> pkg.a.b, never the doubled
//     pkg.a.a.b). This is the zero value, so a nil/absent form map composes as a
//     plain import — the pure-string unit tests rely on that default.
//   - pythonAliasRename (`import a.b as c`): the local name IS the module a.b,
//     so the terminal lives in module.<tail> (`import x.y as x` + x.z.fn ->
//     x.y.z; `import pkg as service` + service.helper.fn -> pkg.helper).
//   - pythonFromImport (`from pkg import name`): name is a MEMBER of pkg. It is a
//     submodule path only when `pkg.name` is a real submodule file (moduleExists,
//     a repo-grounded predicate; nil in pure-string unit tests); then the
//     terminal lives in pkg.name.<tail>. Otherwise name is a class, a function,
//     or a module-level value (a singleton such as `db = SQLAlchemy()`), and
//     `name.attr.fn()` is attribute access, not a `pkg.attr` submodule path —
//     compose NOTHING (no local candidate, no external candidate; precision >
//     recall). This covers every member kind uniformly, so no re-export or
//     callable-kind gate is needed.
func pythonDottedCallModules(alias string, tail []string, imported []string, forms map[string]pythonImportForm, moduleExists func(module string) bool) (all, external []string) {
	addBoth := func(module string) {
		module = strings.TrimSpace(module)
		if module != "" {
			all = append(all, module)
			external = append(external, module)
		}
	}
	addLocalOnly := func(module string) {
		module = strings.TrimSpace(module)
		if module != "" {
			all = append(all, module)
		}
	}
	for _, module := range imported {
		module = strings.TrimSpace(module)
		if module == "" {
			continue
		}
		// The terminal name's defining module is what we must name. `import os`
		// calling `os.path.isdir(x)` looks up isdir under the dotted tail
		// (os.path), never in the bare `os` package, so we compose the module
		// from the alias/module and the tail per the recorded import form rather
		// than adding the bare module on its own.
		if len(tail) > 0 && !strings.Contains(module, "/") {
			tailSpec := strings.Join(tail, ".")
			switch forms[module] {
			case pythonAliasRename:
				// The local name IS the module (`import x.y as x` binds x to x.y),
				// so the terminal lives in module.<tail>.
				addBoth(module + "." + tailSpec)
			case pythonFromImport:
				// The name is a member of `module` and only names a submodule path
				// when module.alias is a real submodule file; otherwise it is a
				// class/function/module-level value and `alias.<tail>.fn()` is
				// attribute access — compose nothing.
				if moduleExists != nil && moduleExists(module+"."+alias) {
					addBoth(module + "." + alias + "." + tailSpec)
				}
			default:
				// pythonPlainImport: the literal dotted call path `alias.<tail>` is
				// the module. Compose that directly instead of appending the tail
				// onto `module`, which would double the shared leading segment(s).
				addBoth(alias + "." + tailSpec)
			}
		} else if len(tail) == 0 {
			// Single-selector call `alias.name()` that reached here past
			// pythonSingleSelectorAliasCall. The os.path facade (`from os import
			// path; path.isdir()`) addresses os.path; otherwise the alias renames a
			// module (`import x.y as alias`) so name is a member of that module.
			if module == "os" && alias == "path" {
				addBoth("os.path")
			} else if module != alias {
				addBoth(module)
			}
		}
		if pythonOSPathDottedCall(alias, tail, module) {
			// The os.path facade is really posixpath/ntpath (genericpath for the
			// shared parts). These candidates exist so os.path.* calls resolve
			// against a vendored CPython stdlib during LOCAL resolution; a normal
			// repo has no genericpath/posixpath/ntpath files, so they must never
			// feed the external-symbol fallback.
			addLocalOnly("genericpath")
			addLocalOnly("posixpath")
			addLocalOnly("ntpath")
		}
	}
	return uniqueStrings(all), uniqueStrings(external)
}

func pythonOSPathDottedCall(alias string, tail []string, module string) bool {
	module = strings.TrimSpace(module)
	switch {
	case module == "os.path":
		return true
	case module == "os" && alias == "os" && len(tail) > 0 && tail[0] == "path":
		return true
	case module == "os" && alias == "path" && len(tail) == 0:
		// `from os import path; path.isdir()` — the single-selector os.path facade.
		return true
	default:
		return false
	}
}

// dottedImportedModuleMatchesFile reports whether any candidate module resolves
// to targetPath under the STRICT module-file rule used exclusively by the
// dotted-call path.
func dottedImportedModuleMatchesFile(modules []string, targetPath string) bool {
	for _, module := range modules {
		if dottedModuleMatchesFileStrict(module, targetPath) {
			return true
		}
	}
	return false
}

// dottedImportedModuleSourceFileIsAncestor reports whether targetPath is the
// module SOURCE file (`.py`/`.pyi`, never an __init__ package file) for one of
// the candidate modules or for one of its ancestor modules. The dotted tail of
// `pkg.mod.Class.method()` mixes submodule and CLASS segments, but the strict
// matcher treats every tail segment as a module path; a class-member call
// therefore resolves against an ancestor module's own source file
// (`myapp.models.User.save` -> myapp/models.py, dropping the class segment
// `User`). Matching is restricted to the module's own source file so that only
// trailing CLASS segments may be dropped: dropping a real submodule segment to a
// package __init__ (a sibling such as `acme.service.zonk` -> acme/__init__.py)
// is NOT a match, and that call's genuine external edge survives. Only the
// external-fallback SUPPRESSION uses this ancestor-aware match; the local EDGE
// emission keeps the strict module matcher.
func dottedImportedModuleSourceFileIsAncestor(modules []string, targetPath string) bool {
	for _, module := range modules {
		for m := strings.TrimSpace(module); m != ""; {
			if dottedModuleSourceFileMatches(m, targetPath) {
				return true
			}
			idx := strings.LastIndex(m, ".")
			if idx < 0 {
				break
			}
			m = m[:idx]
		}
	}
	return false
}

// dottedModuleSourceFileMatches is the module's-own-source-file half of
// dottedModuleMatchesFileStrict, WITHOUT the __init__ package-directory rule:
// module "a.b" matches F iff F's slash-path stem equals "a/b" or ends with
// "/a/b" (which absorbs a source-root prefix such as src/ or Lib/).
func dottedModuleSourceFileMatches(module, targetPath string) bool {
	module = strings.TrimSpace(module)
	if module == "" {
		return false
	}
	moduleStem := strings.ReplaceAll(module, ".", "/")
	target := path.Clean(strings.ReplaceAll(targetPath, "\\", "/"))
	stem := strings.TrimSuffix(target, path.Ext(target))
	return stem == moduleStem || strings.HasSuffix(stem, "/"+moduleStem)
}

// dottedModuleMatchesFileStrict matches a dotted Python module ("a.b") to a
// file for the dedicated dotted-call path ONLY. Unlike importModuleMatchesFile
// it has no parent-directory or basename fallback (that fallback matched any
// sibling submodule): module "a.b" matches F iff F's slash-path stem equals
// "a/b" or ends with "/a/b" — which naturally absorbs a source-root prefix such
// as src/ or Lib/ on the "/" boundary — OR F is ".../a/b/__init__.py" (the
// module IS that package). Nothing else.
func dottedModuleMatchesFileStrict(module, targetPath string) bool {
	module = strings.TrimSpace(module)
	if module == "" {
		return false
	}
	moduleStem := strings.ReplaceAll(module, ".", "/")
	target := path.Clean(strings.ReplaceAll(targetPath, "\\", "/"))
	// F is the module's own source file: .../a/b.py or .../a/b.pyi.
	if stem := strings.TrimSuffix(target, path.Ext(target)); stem == moduleStem || strings.HasSuffix(stem, "/"+moduleStem) {
		return true
	}
	// F is the package's __init__: .../a/b/__init__.py — the module IS the
	// package directory.
	if base := path.Base(target); base == "__init__.py" || base == "__init__.pyi" {
		if dir := path.Dir(target); dir == moduleStem || strings.HasSuffix(dir, "/"+moduleStem) {
			return true
		}
	}
	return false
}

func pythonSingleSelectorAliasCall(alias string, imported []string) bool {
	for _, module := range imported {
		module = strings.TrimSpace(module)
		if module == "" {
			continue
		}
		if module != alias || pythonOSPathDottedCall(alias, nil, module) {
			return true
		}
	}
	return false
}

// stripPythonLiteralsAndComments blanks Python's `#` comments and strings,
// triple-quoted ones across lines, keeping every newline and the length.
//
// Inside every string a backslash takes the byte after it, so `\"""` inside a
// triple-quoted string, like `\"` inside a one-line one, does not close it.
// That holds for raw strings (`r`, `R`, `rb`, `br` and their case variants)
// too: the language reference (Lexical analysis, string and bytes literals)
// says that even in a raw literal a quote can be escaped with a backslash,
// which stays in the value, and that a raw literal cannot end in an odd
// number of backslashes. A prefix changes the value, not where the string
// ends, except for an f-string or t-string (below).
//
// An f-string or t-string (a prefix holding `f`, `F`, `t` or `T`, alone or
// with `r` or `R`) holds replacement fields in braces; `{{` and `}}` are text.
// Inside a field a quote opens a nested string, read by these same rules (an
// f-string nested in it too), and since Python 3.12 (PEP 701) it may be the
// outer string's own quote, so the outer quote ends the string only outside
// every field. Code before 3.12 cannot reuse the quote inside a field, so the
// rule reads every version the same. Here the whole f-string is blanked,
// fields included, as the scanners have always read it; pythonCodeView keeps
// the fields' code.
func stripPythonLiteralsAndComments(content string) string {
	return maskPythonLiteralsAndComments(content, false)
}

// pythonCodeView is Python's code view (callCodeView): the mask above, except
// that an f-string's or t-string's replacement fields are code -- they are
// expressions Python runs -- with their nested strings, comments and format
// specifications blanked.
func pythonCodeView(content string) string {
	return maskPythonLiteralsAndComments(content, true)
}

func maskPythonLiteralsAndComments(content string, fieldsAreCode bool) string {
	bytes := []byte(content)
	for i := 0; i < len(bytes); i++ {
		switch bytes[i] {
		case '"', '\'':
			if formatted, raw := pythonStringPrefix(bytes, i); formatted {
				end, closed := maskPythonFString(bytes, i, raw)
				if !closed {
					// A one-line string a line break ends unclosed: left as
					// is, as for any other string.
					i = end
					continue
				}
				if !fieldsAreCode {
					maskBytes(bytes, i, end)
				}
				i = end - 1
				continue
			}
			quote := bytes[i]
			if i+2 < len(bytes) && bytes[i+1] == quote && bytes[i+2] == quote {
				j := i + 3
				for j+2 < len(bytes) && !(bytes[j] == quote && bytes[j+1] == quote && bytes[j+2] == quote) {
					if bytes[j] == '\\' {
						j++
					}
					j++
				}
				if j+2 < len(bytes) {
					j += 3
				}
				maskBytes(bytes, i, j)
				i = j - 1
				continue
			}
			for j := i + 1; j < len(bytes); j++ {
				if bytes[j] == '\n' || bytes[j] == '\r' {
					i = j
					break
				}
				if bytes[j] == '\\' {
					j++
					continue
				}
				if bytes[j] == quote {
					maskBytes(bytes, i, j+1)
					i = j
					break
				}
			}
		case '#':
			j := i + 1
			for j < len(bytes) && bytes[j] != '\n' && bytes[j] != '\r' {
				j++
			}
			maskBytes(bytes, i, j)
			i = j
		}
	}
	return string(bytes)
}

// pythonStringPrefix reports, for the quote at quote, whether the word written
// right before it is a string prefix that makes an f-string or t-string, and
// whether it makes a raw one. A word that is not a prefix (`if"x"`) gives a
// plain string.
func pythonStringPrefix(b []byte, quote int) (formatted, raw bool) {
	start := quote
	for start > 0 && isASCIIIdentifierByte(b[start-1]) {
		start--
	}
	if quote-start == 0 || quote-start > 2 || (start > 0 && b[start-1] >= 0x80) {
		return false, false
	}
	// The prefixes that make one: f, t, and either with r, in any case and
	// order. b and u never combine with f or t.
	word := b[start:quote]
	for _, c := range word {
		switch c | 0x20 {
		case 'f', 't':
			if formatted {
				return false, false
			}
			formatted = true
		case 'r':
			if raw {
				return false, false
			}
			raw = true
		default:
			return false, false
		}
	}
	return formatted, raw
}

// maskPythonFString blanks the text of the f-string or t-string whose opening
// quote is at start -- its literal parts, its fields' nested strings,
// comments and format specifications -- and keeps its fields' code. It
// returns the index after the closing quote, and whether there was one: a
// one-line string a line break ends unclosed returns that line break's index
// (the end of the content for a string the content ends).
func maskPythonFString(b []byte, start int, raw bool) (int, bool) {
	quote := b[start]
	width := 1
	if start+2 < len(b) && b[start+1] == quote && b[start+2] == quote {
		width = 3
	}
	text := start
	j := start + width
	for j < len(b) {
		c := b[j]
		switch {
		case c == quote && (width == 1 || (j+2 < len(b) && b[j+1] == quote && b[j+2] == quote)):
			maskBytes(b, text, j+width)
			return j + width, true
		case width == 1 && (c == '\n' || c == '\r'):
			return j, false
		case c == '\\':
			if !raw && j+2 < len(b) && b[j+1] == 'N' && b[j+2] == '{' {
				// A named escape, `\N{EM DASH}`: its braces are not a field.
				k := j + 3
				for k < len(b) && b[k] != '}' && b[k] != quote && b[k] != '\n' {
					k++
				}
				if k < len(b) && b[k] == '}' {
					k++
				}
				j = k
				continue
			}
			if j+1 < len(b) && (b[j+1] == '{' || b[j+1] == '}') {
				// A backslash does not escape a brace.
				j++
				continue
			}
			j += 2
		case c == '{' && j+1 < len(b) && b[j+1] == '{':
			j += 2
		case c == '{':
			maskBytes(b, text, j+1)
			end, ok := maskPythonField(b, j+1, quote, width, raw)
			if !ok {
				return end, false
			}
			j, text = end, end
		default:
			j++
		}
	}
	maskBytes(b, text, len(b))
	return len(b), width == 3
}

// maskPythonField reads the replacement field whose code starts at j, inside
// an f-string with the given quote and width. Its code stays; a nested string
// or comment in it, and its format specification after a top-level `:`, are
// blanked. It returns the index where the f-string's text resumes (after the
// field's closing brace), and false when the content ends first.
func maskPythonField(b []byte, j int, quote byte, width int, raw bool) (int, bool) {
	depth := 0
	for j < len(b) {
		c := b[j]
		switch c {
		case '"', '\'':
			if formatted, nestedRaw := pythonStringPrefix(b, j); formatted {
				end, closed := maskPythonFString(b, j, nestedRaw)
				if !closed {
					return end, false
				}
				j = end
				continue
			}
			end, closed := maskPythonPlainString(b, j)
			if !closed {
				return end, false
			}
			j = end
		case '#':
			k := j
			for k < len(b) && b[k] != '\n' && b[k] != '\r' {
				k++
			}
			maskBytes(b, j, k)
			j = k
		case '(', '[', '{':
			depth++
			j++
		case ')', ']':
			if depth > 0 {
				depth--
			}
			j++
		case '}':
			if depth == 0 {
				return j + 1, true
			}
			depth--
			j++
		case ':':
			if depth == 0 {
				return maskPythonFormatSpec(b, j+1, quote, width, raw)
			}
			j++
		default:
			j++
		}
	}
	return len(b), false
}

// maskPythonFormatSpec blanks a field's format specification from j (text,
// with nested fields in braces that are code) up to the field's closing
// brace, and returns where the f-string's text resumes. The f-string's own
// closing quote, or for a one-line string a line break, also ends it; the
// f-string then reads that byte.
func maskPythonFormatSpec(b []byte, j int, quote byte, width int, raw bool) (int, bool) {
	text := j
	for j < len(b) {
		c := b[j]
		switch {
		case c == '}':
			maskBytes(b, text, j)
			return j + 1, true
		case c == '{':
			maskBytes(b, text, j+1)
			end, ok := maskPythonField(b, j+1, quote, width, raw)
			if !ok {
				return end, false
			}
			j, text = end, end
		case c == quote && (width == 1 || (j+2 < len(b) && b[j+1] == quote && b[j+2] == quote)), width == 1 && (c == '\n' || c == '\r'):
			maskBytes(b, text, j)
			return j, true
		default:
			j++
		}
	}
	return len(b), false
}

// maskPythonPlainString blanks the string without f or t prefix whose opening
// quote is at start, by the rules of stripPythonLiteralsAndComments, and
// returns the index after its closing quote and whether there was one.
func maskPythonPlainString(b []byte, start int) (int, bool) {
	quote := b[start]
	if start+2 < len(b) && b[start+1] == quote && b[start+2] == quote {
		j := start + 3
		for j+2 < len(b) && !(b[j] == quote && b[j+1] == quote && b[j+2] == quote) {
			if b[j] == '\\' {
				j++
			}
			j++
		}
		if j+2 >= len(b) {
			maskBytes(b, start, len(b))
			return len(b), false
		}
		maskBytes(b, start, j+3)
		return j + 3, true
	}
	for j := start + 1; j < len(b); j++ {
		if b[j] == '\n' || b[j] == '\r' {
			return j, false
		}
		if b[j] == '\\' {
			j++
			continue
		}
		if b[j] == quote {
			maskBytes(b, start, j+1)
			return j + 1, true
		}
	}
	return len(b), false
}

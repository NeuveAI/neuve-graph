package sem

// Erlang call-site extraction. The generic identifier scanner cannot use the
// information an Erlang call site carries: a remote call `mod:fun(Args)` names
// the target module explicitly (and by language convention module `mod` lives
// in `mod.erl`), while a bare `fun(Args)` call is module-local by language
// rule (barring the rarely used -import attribute). Erlang also has its own
// lexical noise the generic scanner trips over: capitalized identifiers are
// variables (`Fun(Args)` is a call through a variable, not to a named
// function), `?MACRO(...)` is preprocessor expansion, `#rec{...}` is record
// syntax, and comments start with `%`. Everything here is gated to
// Language == "Erlang" so no other language's extraction shifts.

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
)

type erlangSelfCallWitness struct {
	StartLine int `json:"start_line"`
	EndLine   int `json:"end_line"`
}

func (w erlangSelfCallWitness) contained(start, end int) bool {
	return w.StartLine >= start && w.StartLine > 0 && w.EndLine >= w.StartLine && w.EndLine <= end
}

func erlangKnownArgsArity(args *sitter.Node, macroDependent bool) (int, bool) {
	if !validNode(args) || args.IsMissing() || args.HasError() || macroDependent || args.ChildCount() < 2 {
		return 0, false
	}
	if args.Child(0).Type() != "(" || args.Child(int(args.ChildCount())-1).Type() != ")" {
		return 0, false
	}
	count := 0
	for i := 0; i < int(args.NamedChildCount()); i++ {
		if args.NamedChild(i).Type() != "comment" {
			count++
		}
	}
	return count, true
}

// Each scoped subtree is visited once. Macro summaries flow upward instead of
// rescanning every nested call's arguments; anonymous callable bodies are skipped.
func erlangSelfCallWalk(node *sitter.Node, src []byte, name string, arity, depth int) (erlangSelfCallWitness, bool, bool) {
	if !validNode(node) || node.IsMissing() || node.Type() == "ERROR" || depth >= maxParseWalkDepth {
		return erlangSelfCallWitness{}, false, false
	}
	if node.Type() == "anonymous_fun" {
		return erlangSelfCallWitness{}, false, true
	}
	macro := strings.HasPrefix(node.Type(), "macro_")
	var witness erlangSelfCallWitness
	argsMacro := false
	args := node.ChildByFieldName("args")
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		w, m, complete := erlangSelfCallWalk(child, src, name, arity, depth+1)
		if !complete {
			return erlangSelfCallWitness{}, false, false
		}
		macro = macro || m
		if validNode(args) && child.StartByte() == args.StartByte() && child.EndByte() == args.EndByte() {
			argsMacro = m
		}
		if w.StartLine > 0 && (witness.StartLine == 0 || w.StartLine < witness.StartLine) {
			witness = w
		}
	}
	if node.Type() == "call" && name != "" {
		expr := node.ChildByFieldName("expr")
		parent := node.Parent()
		if (!validNode(parent) || parent.Type() != "remote") && validNode(expr) && expr.Type() == "atom" && expr.Content(src) == name {
			if n, known := erlangKnownArgsArity(args, argsMacro); known && n == arity {
				w := erlangSelfCallWitness{int(node.StartPoint().Row) + 1, int(node.EndPoint().Row) + 1}
				if witness.StartLine == 0 || w.StartLine <= witness.StartLine {
					witness = w
				}
			}
		}
	}
	return witness, macro, true
}

func collectErlangSelfCalls(root *sitter.Node, src []byte, entities []Entity) {
	type owner struct {
		name               string
		arity, oldArity    int
		known, conditional bool
		forms              []*sitter.Node
	}
	var owners []owner
	byName := map[string][]int{}
	conditionalDepth := 0
	previousFun := false
	for i := 0; i < int(root.NamedChildCount()); i++ {
		form := root.NamedChild(i)
		switch form.Type() {
		case "pp_ifdef", "pp_ifndef", "pp_if":
			conditionalDepth++
			previousFun = false
			continue
		case "pp_endif":
			conditionalDepth--
			if conditionalDepth < 0 {
				return
			}
			previousFun = false
			continue
		}
		if form.Type() != "fun_decl" {
			previousFun = false
			continue
		}
		name, oldArity := erlangFunClauseNameArity(form, src)
		clause := firstNamedChildOfType(form, "function_clause")
		if !validNode(clause) || name == "" {
			return
		}
		args := clause.ChildByFieldName("args")
		_, macro, complete := erlangSelfCallWalk(args, src, "", 0, 0)
		if !complete {
			return
		}
		arity, known := erlangKnownArgsArity(args, macro)
		if previousFun && len(owners) > 0 && owners[len(owners)-1].name == name && owners[len(owners)-1].oldArity == oldArity {
			o := &owners[len(owners)-1]
			o.known = o.known && known && o.arity == arity
			o.conditional = o.conditional || conditionalDepth > 0
			o.forms = append(o.forms, form)
		} else {
			byName[name] = append(byName[name], len(owners))
			owners = append(owners, owner{name, arity, oldArity, known, conditionalDepth > 0, []*sitter.Node{form}})
		}
		previousFun = true
	}
	if conditionalDepth != 0 {
		return
	}
	// Counts are computed once, then each fold uses constant-time ambiguity checks.
	type counts struct {
		unknown int
		arities map[int]int
	}
	countsByName := map[string]counts{}
	for name, indices := range byName {
		c := counts{arities: map[int]int{}}
		for _, index := range indices {
			o := owners[index]
			if !o.known {
				c.unknown++
			} else {
				c.arities[o.arity]++
			}
		}
		countsByName[name] = c
	}
	entityByOrigin := map[int]int{}
	for i, e := range entities {
		if e.Kind == "function" {
			entityByOrigin[e.sourceStartByte] = i
		}
	}
	for _, o := range owners {
		c := countsByName[o.name]
		if !o.known || o.conditional || c.unknown != 0 || c.arities[o.arity] != 1 {
			continue
		}
		index, ok := entityByOrigin[int(o.forms[0].StartByte())]
		if !ok || entities[index].Name != o.name {
			continue
		}
		var witness erlangSelfCallWitness
		complete := true
		for _, form := range o.forms {
			clause := firstNamedChildOfType(form, "function_clause")
			body := clause.ChildByFieldName("body")
			w, _, done := erlangSelfCallWalk(body, src, o.name, o.arity, 0)
			if !done {
				complete = false
				break
			}
			if w.StartLine > 0 && (witness.StartLine == 0 || w.StartLine < witness.StartLine) {
				witness = w
			}
		}
		if complete && witness.contained(entities[index].StartLine, entities[index].EndLine) {
			entities[index].erlangSelfCall = witness
		}
	}
}

func finalizeErlangSelfCalls(entities []Entity, status ParseStatus) {
	if status.ParseError || status.Partial || status.DepthExceeded {
		for i := range entities {
			entities[i].erlangSelfCall = erlangSelfCallWitness{}
		}
	}
}

// erlangCallSite is one call expression found in an Erlang block: a remote
// call `Module:Name(...)`, or a local call `Name(...)` when Module is empty.
type erlangCallSite struct {
	Module string
	Name   string
}

var (
	// Atoms: lowercase-initial identifier (quoted atoms are masked away by
	// stripErlangCodeText and are not resolved).
	erlangRemoteCallRe = regexp.MustCompile(`\b([a-z][A-Za-z0-9_@]*)\s*:\s*([a-z][A-Za-z0-9_@]*)\s*\(`)
	erlangLocalCallRe  = regexp.MustCompile(`\b([a-z][A-Za-z0-9_@]*)\s*\(`)
	// `?MODULE:name(...)` always targets the enclosing module, so it is the one
	// macro-qualified call that resolves reliably — as a local call.
	erlangModuleMacroCallRe = regexp.MustCompile(`\?MODULE\s*:\s*([a-z][A-Za-z0-9_@]*)\s*\(`)
)

// erlangKeyword reports reserved words that can precede `(` without being a
// call (`fun (X) -> ...`, `case (X) of`, `not (A)`).
func erlangKeyword(name string) bool {
	switch name {
	case "fun", "case", "if", "receive", "after", "begin", "end", "try", "catch", "when", "of",
		"andalso", "orelse", "not", "and", "or", "xor", "band", "bor", "bxor", "bnot", "bsl", "bsr", "div", "rem":
		return true
	}
	return false
}

// stripErlangCodeText masks the Erlang syntaxes the generic stripper does not
// know: `%` line comments, `$c` character literals (whose payload could
// otherwise open a bogus string or comment mask, e.g. `$"` or `$%`), and
// string/quoted-atom literals, which may span lines. Newlines are preserved.
func stripErlangCodeText(content string) string {
	bytes := []byte(content)
	for i := 0; i < len(bytes); i++ {
		switch bytes[i] {
		case '$':
			// Character literal: `$c` or escaped `$\c`.
			end := i + 2
			if i+1 < len(bytes) && bytes[i+1] == '\\' {
				end = i + 3
			}
			maskBytes(bytes, i, end)
			i = end - 1
		case '%':
			j := i
			for j < len(bytes) && bytes[j] != '\n' && bytes[j] != '\r' {
				j++
			}
			maskBytes(bytes, i, j)
			i = j
		case '"', '\'':
			quote := bytes[i]
			j := i + 1
			for j < len(bytes) {
				if bytes[j] == '\\' {
					j += 2
					continue
				}
				if bytes[j] == quote {
					break
				}
				j++
			}
			if j >= len(bytes) {
				j = len(bytes) - 1
			}
			maskBytes(bytes, i, j+1)
			i = j
		}
	}
	return string(bytes)
}

// erlangCallSites scans an Erlang block for call expressions, deduplicated and
// in deterministic order. Remote calls yield (module, name) pairs; local calls
// yield bare names. Variables (capitalized, so never matched by the
// lowercase-initial atom pattern), `?MACRO(...)` expansions, `#rec` record
// syntax, and `fun name/arity` references (no argument list) are not calls.
func erlangCallSites(block string) []erlangCallSite {
	stripped := stripErlangCodeText(block)
	seen := map[erlangCallSite]bool{}
	remoteName := map[int]bool{} // start offsets of remote-call function names
	for _, m := range erlangRemoteCallRe.FindAllStringSubmatchIndex(stripped, -1) {
		if erlangCallPrefixIgnored(stripped, m[2]) {
			continue // e.g. `?mod:fun(` macro-built qualifier
		}
		seen[erlangCallSite{Module: stripped[m[2]:m[3]], Name: stripped[m[4]:m[5]]}] = true
		remoteName[m[4]] = true
	}
	for _, m := range erlangModuleMacroCallRe.FindAllStringSubmatchIndex(stripped, -1) {
		seen[erlangCallSite{Name: stripped[m[2]:m[3]]}] = true
	}
	for _, m := range erlangLocalCallRe.FindAllStringSubmatchIndex(stripped, -1) {
		start := m[2]
		name := stripped[m[2]:m[3]]
		// A `name(` at column 0 is a function clause head (`name(Args) ->`),
		// not a call site: definitions start at the left margin and body
		// expressions are indented. Without this, every folded clause head
		// would fabricate a call edge to same-named sibling-arity functions.
		if start == 0 || stripped[start-1] == '\n' || stripped[start-1] == '\r' {
			continue
		}
		if remoteName[start] || erlangKeyword(name) || erlangCallPrefixIgnored(stripped, start) {
			continue
		}
		seen[erlangCallSite{Name: name}] = true
	}
	sites := make([]erlangCallSite, 0, len(seen))
	for site := range seen {
		sites = append(sites, site)
	}
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].Module != sites[j].Module {
			return sites[i].Module < sites[j].Module
		}
		return sites[i].Name < sites[j].Name
	})
	return sites
}

// erlangCallPrefixIgnored reports whether the character preceding a candidate
// call name disqualifies it: `:` (the function part of a remote call, handled
// by the remote pattern), `?` (macro expansion), `#` (record syntax), `.` or
// `'` (masked quoted-atom remainder / dotted context).
func erlangCallPrefixIgnored(content string, start int) bool {
	if start == 0 {
		return false
	}
	switch content[start-1] {
	case ':', '?', '#', '.', '\'':
		return true
	}
	return false
}

// erlangFileDefinesModule reports whether an Erlang source file defines module
// `module`: by filename convention (`mod` lives in `mod.erl`) or by the
// extracted `-module(mod).` attribute symbol.
func erlangFileDefinesModule(path, module string, symbolsByShortName map[string][]SymbolRecord) bool {
	base := filepath.Base(path)
	if strings.HasSuffix(base, ".erl") && strings.TrimSuffix(base, ".erl") == module {
		return true
	}
	for _, s := range symbolsByShortName[module] {
		if s.Language == "Erlang" && s.Kind == "module" && s.FilePath == path {
			return true
		}
	}
	return false
}

// erlangCallRelations resolves the call sites in an Erlang function's block.
// Local calls resolve within the same file only (a bare call is module-local
// by language rule); remote calls resolve to functions defined in the file of
// the module named by the qualifier. Erlang identifies functions by
// name/arity and the parser folds each arity into its own symbol, so a name
// may match several arity symbols in the target module — each is emitted,
// matching the conservative name-level resolution used elsewhere. Self calls
// require the live parser's admitted witness; the legacy scanner skips the
// enclosing symbol itself. A same-name call landing
// on a sibling arity (`f(X) -> f(X, default)`) is a real edge and kept.
func erlangCallRelations(from SymbolRecord, block string, sameFile []SymbolRecord, symbolsByShortName map[string][]SymbolRecord) []RelationRecord {
	var relations []RelationRecord
	if from.Language == "Erlang" && from.Kind == "function" && from.erlangSelfCall.contained(from.StartLine, from.EndLine) {
		relations = append(relations, RelationRecord{RecordType: "relation", FromID: from.ID, ToID: from.ID, Type: "CALLS", Confidence: .92, Reason: "source-proven named body self call", RelationScope: "file", Resolution: "exact", TargetKind: "symbol", Evidence: []Evidence{{Kind: "call_site", FilePath: from.FilePath, StartLine: from.erlangSelfCall.StartLine, EndLine: from.erlangSelfCall.EndLine, Detail: from.Name}}, WarningCodes: []string{}})
	}
	for _, site := range erlangCallSites(block) {
		var targets []resolvedCallTarget
		if site.Module == "" {
			for _, to := range sameFile {
				if to.ID == from.ID || to.Kind != "function" || to.Name != site.Name {
					continue
				}
				targets = append(targets, resolvedCallTarget{
					SymbolRecord: to,
					Confidence:   0.92,
					Reason:       "local call resolved to same-module function",
					Resolution:   "exact",
					Scope:        "file",
				})
			}
		} else {
			for _, to := range symbolsByShortName[site.Name] {
				if to.ID == from.ID || to.Language != "Erlang" || to.Kind != "function" {
					continue
				}
				if !erlangFileDefinesModule(to.FilePath, site.Module, symbolsByShortName) {
					continue
				}
				targets = append(targets, resolvedCallTarget{
					SymbolRecord: to,
					Confidence:   0.9,
					Reason:       "remote call resolved to the module named by the qualifier",
					Resolution:   "exact",
					Scope:        "module",
				})
			}
		}
		detail := site.Name
		if site.Module != "" {
			detail = site.Module + ":" + site.Name
		}
		for _, to := range targets {
			relations = append(relations, RelationRecord{
				RecordType:    "relation",
				FromID:        from.ID,
				ToID:          to.ID,
				Type:          "CALLS",
				Confidence:    to.Confidence,
				Reason:        to.Reason,
				RelationScope: to.Scope,
				Resolution:    to.Resolution,
				TargetKind:    "symbol",
				Evidence: []Evidence{{
					Kind:      "call_site",
					FilePath:  from.FilePath,
					StartLine: from.StartLine,
					EndLine:   from.EndLine,
					Detail:    detail,
				}},
				WarningCodes: []string{},
			})
		}
	}
	return relations
}

package sem

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"path"
	"sort"
	"strings"
	"unicode"

	sitter "github.com/smacker/go-tree-sitter"
)

// FileTokensRecord is the opt-in companion of FileOutlineRecord: every leaf of
// the file's syntax tree, the identifier names the file uses and, for the
// languages that have them, its import statements. Like the outline it is a
// symbols-stream record only, never a cached snapshot payload.
//
// Leaves are positional rows; see tokenLeaf* for the columns. No source text is
// carried beyond identifier names and import specifiers and bindings.
type FileTokensRecord struct {
	RecordType string `json:"record_type"`
	FilePath   string `json:"file_path"`
	Language   string `json:"language"`
	// Status says whether the leaves were extracted: tokensExtracted, or why
	// not (tokensLanguageNotCovered, tokensOverSizeLimit). The leaf fields and
	// imports are present only when they were.
	Status          string   `json:"status"`
	HasErrors       bool     `json:"has_errors"`
	IdentifierNames []string `json:"identifier_names,omitzero"`
	LeafTypes       []string `json:"leaf_types,omitzero"`
	Ancestors       []string `json:"ancestors,omitzero"`
	Leaves          [][8]int `json:"leaves,omitzero"`
	ImportBindings  string   `json:"import_bindings"`
	// Imports is [] for a covered language with no import statement and
	// absent (nil) for a language whose bindings are not extracted.
	Imports []ImportStatementRecord `json:"imports,omitzero"`
}

// ImportStatementRecord is one imported specifier of one import statement.
type ImportStatementRecord struct {
	StartLine      int                   `json:"start_line"`
	EndLine        int                   `json:"end_line"`
	Specifier      string                `json:"specifier"`
	Bindings       []ImportBindingRecord `json:"bindings"`
	SideEffectOnly bool                  `json:"side_effect_only"`
	NamesUnknown   bool                  `json:"names_unknown"`
}

// ImportBindingRecord is one name an import statement brings into the file.
type ImportBindingRecord struct {
	Imported string `json:"imported"`
	Local    string `json:"local"`
	TypeOnly bool   `json:"type_only"`
	// LocalInferred marks a Go import without an explicit name, whose local
	// name is the one Go tooling assumes from the import path.
	LocalInferred bool `json:"local_inferred,omitempty"`
}

// Leaf row flags.
const (
	tokenFlagIdentifier = 1
	tokenFlagComment    = 2
	tokenFlagError      = 4
	tokenFlagNamed      = 8
	tokenFlagRewritten  = 16
)

const (
	importBindingsExtracted    = "extracted"
	importBindingsNotExtracted = "not_extracted"
)

const (
	tokensExtracted          = "extracted"
	tokensLanguageNotCovered = "language_not_covered"
	tokensOverSizeLimit      = "over_size_limit"
)

// tokensSourceByteLimit is the largest file, in bytes as given, whose leaves
// are extracted, and tokensLeafLimit the most leaf rows one record carries.
// Ordinary code runs to about 6 to 11 output bytes per source byte, but dense
// tables (numbers, punctuation) reach 23, so bytes alone cannot bound the
// output: a row encodes in at most about 45 bytes, and 100,000 rows with their
// tables stay under a consumer's 8 MiB per-file cap by construction. A file
// past either limit gets a record with tokensOverSizeLimit and nothing else;
// its outline is unaffected.
const (
	tokensSourceByteLimit = 512 << 10
	tokensLeafLimit       = 100_000
)

// tokenClasses is one language's leaf classification. A leaf is an identifier
// when it is a named node whose type is listed and it does not lie under a
// comment; it is a comment when it, or one of its ancestors, is a named node
// whose type is listed under comments. Keywords are anonymous tokens in every
// grammar listed here, so the tree, not the text, decides whether a contextual
// keyword is a name.
//
// content lists the node types whose own text (bytes between their children
// with no node of their own) is program content, such as a string literal's
// characters: every such byte is a row, whitespace included. Own text of any
// other node that is all whitespace is layout and has no row.
type tokenClasses struct {
	identifiers []string
	comments    []string
	content     []string
}

// tokenClassTable is the one table that classifies leaves, keyed by the
// language label the outline reports. docs/snapshot-format.md mirrors it and
// TestTokenClassificationDocMirrorsTable keeps the two equal.
//
// A language is listed only once TestFileTokensLiteralWhitespaceIsContent
// proves, for each of its string-like forms, that a whitespace change inside
// the literal changes the leaves while layout outside it does not. Every other
// language gets a record with tokensLanguageNotCovered.
var tokenClassTable = map[string]tokenClasses{
	"C":          {identifiers: []string{"field_identifier", "identifier", "statement_identifier", "type_identifier"}, comments: []string{"comment"}, content: []string{"char_literal", "string_literal"}},
	"C#":         {identifiers: []string{"identifier"}, comments: []string{"comment"}, content: []string{"character_literal", "interpolated_string_expression", "raw_string_literal", "string_literal", "verbatim_string_literal"}},
	"Elixir":     {identifiers: []string{"alias", "identifier"}, comments: []string{"comment"}, content: []string{"charlist", "sigil", "string"}},
	"Java":       {identifiers: []string{"identifier", "type_identifier"}, comments: []string{"block_comment", "line_comment"}, content: []string{"character_literal", "string_literal"}},
	"Kotlin":     {identifiers: []string{"interpolated_identifier", "simple_identifier", "type_identifier"}, comments: []string{"line_comment", "multiline_comment"}, content: []string{"character_literal", "string_literal"}},
	"PHP":        {identifiers: []string{"name"}, comments: []string{"comment"}, content: []string{"encapsed_string", "heredoc", "heredoc_body", "nowdoc", "nowdoc_body", "string"}},
	"Ruby":       {identifiers: []string{"class_variable", "constant", "global_variable", "hash_key_symbol", "identifier", "instance_variable"}, comments: []string{"comment"}, content: []string{"character", "delimited_symbol", "heredoc_body", "regex", "string"}},
	"Go":         {identifiers: []string{"blank_identifier", "field_identifier", "identifier", "label_name", "package_identifier", "type_identifier"}, comments: []string{"comment"}, content: []string{"interpreted_string_literal", "raw_string_literal", "rune_literal"}},
	"JavaScript": {identifiers: []string{"identifier", "private_property_identifier", "property_identifier", "shorthand_property_identifier", "shorthand_property_identifier_pattern", "statement_identifier", "type_identifier"}, comments: []string{"comment", "html_comment"}, content: []string{"jsx_text", "regex", "string", "template_string"}},
	"Python":     {identifiers: []string{"identifier"}, comments: []string{"comment"}, content: []string{"string"}},
	"Rust":       {identifiers: []string{"field_identifier", "identifier", "metavariable", "shorthand_field_identifier", "type_identifier"}, comments: []string{"block_comment", "doc_comment", "inner_doc_comment_marker", "line_comment", "outer_doc_comment_marker"}, content: []string{"char_literal", "raw_string_literal", "string_literal"}},
	"TypeScript": {identifiers: []string{"identifier", "private_property_identifier", "property_identifier", "shorthand_property_identifier", "shorthand_property_identifier_pattern", "statement_identifier", "type_identifier"}, comments: []string{"comment", "html_comment"}, content: []string{"jsx_text", "regex", "string", "template_string"}},
}

// importBindingLanguages are the languages whose import statements the tokens
// record extracts. Every other language reports importBindingsNotExtracted.
var importBindingLanguages = []string{"Go", "JavaScript", "Python", "TypeScript"}

// TokenLanguages lists the languages the tokens read covers: the
// classification table's keys, each proven by the literal-whitespace test.
func TokenLanguages() []string {
	languages := make([]string, 0, len(tokenClassTable))
	for language := range tokenClassTable {
		languages = append(languages, language)
	}
	sort.Strings(languages)
	return languages
}

// ImportBindingLanguages lists the languages whose tokens record carries import
// statements.
func ImportBindingLanguages() []string {
	return append([]string(nil), importBindingLanguages...)
}

func importBindingsCovered(language string) bool {
	for _, covered := range importBindingLanguages {
		if covered == language {
			return true
		}
	}
	return false
}

// tokenSourceView maps the parse view back to the file as given. Masks are
// same-length rewrites; Protocol Buffers additionally inserts a synthetic
// syntax line of insertLen bytes at insertAt, which no row may describe.
type tokenSourceView struct {
	file     []byte
	parsed   []byte
	insertAt int
	insertN  int
	grammar  *sitter.Language
}

func (v tokenSourceView) fileOffset(parsed int) (int, bool) {
	if parsed < v.insertAt {
		return parsed, true
	}
	if parsed < v.insertAt+v.insertN {
		return v.insertAt, false
	}
	return parsed - v.insertN, true
}

func (v tokenSourceView) parsedOffset(file int) int {
	if file < v.insertAt {
		return file
	}
	return file + v.insertN
}

type tokenRow struct {
	typ, start, end, flags, ancestors, opens int
}

type tokenFrame struct {
	node     *sitter.Node
	pos      int // parse-view byte up to which this node's children have been seen
	end      int
	chainEnd int // length of the encoded ancestor chain including this node
	hash     int // ancestors-table index of the chain ending at this node, -1 until needed
	comment  bool
	content  bool // the node's own text is content, whitespace included
	err      bool
}

// extractFileTokens walks the whole tree once. Every leaf becomes a row, and
// text a node owns between its children becomes a row typed with that node
// (all-whitespace text only inside a content node), so the rows tile the file.
func extractFileTokens(path, language string, root *sitter.Node, view tokenSourceView) FileTokensRecord {
	record := FileTokensRecord{
		RecordType: "file_tokens", FilePath: path, Language: language,
		HasErrors: root.HasError(), ImportBindings: importBindingsNotExtracted,
	}
	classes, covered := tokenClassTable[language]
	switch {
	case !covered:
		record.Status = tokensLanguageNotCovered
		return record
	case len(view.file) > tokensSourceByteLimit:
		record.Status = tokensOverSizeLimit
		return record
	}
	record.Status = tokensExtracted
	record.IdentifierNames, record.LeafTypes, record.Ancestors, record.Leaves = []string{}, []string{}, []string{}, [][8]int{}
	contentTypes := map[string]bool{}
	for _, typ := range classes.content {
		contentTypes[typ] = true
	}
	identifierTypes := map[string]bool{}
	for _, typ := range classes.identifiers {
		identifierTypes[typ] = true
	}
	commentTypes := map[string]bool{}
	for _, typ := range classes.comments {
		commentTypes[typ] = true
	}
	typeIndex := map[string]int{}
	internType := func(typ string) int {
		if index, ok := typeIndex[typ]; ok {
			return index
		}
		typeIndex[typ] = len(record.LeafTypes)
		record.LeafTypes = append(record.LeafTypes, typ)
		return typeIndex[typ]
	}
	ancestorIndex := map[string]int{}
	chain := []byte{}
	internChain := func(end int) int {
		sum := sha256.Sum256(chain[:end])
		key := hex.EncodeToString(sum[:])[:16]
		if index, ok := ancestorIndex[key]; ok {
			return index
		}
		ancestorIndex[key] = len(record.Ancestors)
		record.Ancestors = append(record.Ancestors, key)
		return ancestorIndex[key]
	}
	appendChain := func(node *sitter.Node) int {
		if node.IsNamed() {
			chain = append(chain, 'n')
		} else {
			chain = append(chain, 'a')
		}
		typ := node.Type()
		chain = binary.AppendUvarint(chain, uint64(len(typ)))
		chain = append(chain, typ...)
		return len(chain)
	}

	var rows []tokenRow
	var importNodes []*sitter.Node
	stack := []tokenFrame{}
	firstUnopened := 0
	emit := func(parent int, typ string, start, end, flags int) {
		frame := &stack[parent]
		if frame.hash < 0 {
			frame.hash = internChain(frame.chainEnd)
		}
		opens := 0
		if parent+1 > firstUnopened {
			opens = parent + 1 - firstUnopened
			firstUnopened = parent + 1
		}
		rows = append(rows, tokenRow{typ: internType(typ), start: start, end: end, flags: flags, ancestors: frame.hash, opens: opens})
	}
	// ownText emits the text of [from, to) owned by the node on top of the
	// stack as one row typed with that node, covering exactly those bytes. A
	// gap that is all whitespace is content inside a content node and layout
	// (no row) anywhere else. A byte-order mark at byte 0 is never a row.
	ownText := func(from, to int) {
		if from == 0 && bytes.HasPrefix(view.parsed, utf8BOM) && bytes.HasPrefix(view.file, utf8BOM) {
			from = len(utf8BOM)
		}
		if from >= to || to > len(view.parsed) {
			return
		}
		top := len(stack) - 1
		if !stack[top].content && len(bytes.TrimLeftFunc(view.parsed[from:to], unicode.IsSpace)) == 0 {
			return
		}
		start, end := from, to
		frame := stack[top]
		flags := 0
		if frame.node.IsNamed() {
			flags |= tokenFlagNamed
		}
		if frame.comment {
			flags |= tokenFlagComment
		} else if frame.node.IsNamed() && identifierTypes[frame.node.Type()] {
			flags |= tokenFlagIdentifier
		}
		if frame.err {
			flags |= tokenFlagError
		}
		emit(top, frame.node.Type(), start, end, flags)
	}
	push := func(node *sitter.Node, pos, end int) {
		parentComment, parentErr := false, false
		if len(stack) > 0 {
			parentComment, parentErr = stack[len(stack)-1].comment, stack[len(stack)-1].err
		}
		stack = append(stack, tokenFrame{
			node: node, pos: pos, end: end, chainEnd: appendChain(node), hash: -1,
			comment: parentComment || (node.IsNamed() && commentTypes[node.Type()]),
			content: node.IsNamed() && contentTypes[node.Type()],
			err:     parentErr || node.IsError(),
		})
	}
	pop := func() {
		top := len(stack) - 1
		frame := stack[top]
		ownText(frame.pos, frame.end)
		stack = stack[:top]
		if firstUnopened > top {
			firstUnopened = top
		}
		if top > 0 {
			chain = chain[:stack[top-1].chainEnd]
		} else {
			chain = chain[:0]
		}
		if top > 0 && int(frame.node.EndByte()) > stack[top-1].pos {
			stack[top-1].pos = int(frame.node.EndByte())
		}
	}

	// The root chain is always entry 0, the fallback for rewritten rows.
	appendChain(root)
	internChain(len(chain))
	chain = chain[:0]
	if root.ChildCount() == 0 {
		// A tree that is a single leaf: its row has the empty chain, and any
		// text outside it is the root's own as well.
		stack = append(stack, tokenFrame{node: root, end: len(view.parsed), hash: -1})
		ownText(0, int(root.StartByte()))
		if root.EndByte() > root.StartByte() {
			flags := 0
			if root.IsNamed() {
				flags |= tokenFlagNamed
			}
			emit(0, root.Type(), int(root.StartByte()), int(root.EndByte()), flags)
		}
		ownText(int(root.EndByte()), len(view.parsed))
		stack = stack[:0]
	} else {
		cursor := sitter.NewTreeCursor(root)
		defer cursor.Close()
		push(root, 0, len(view.parsed))
		cursor.GoToFirstChild()
	walk:
		for {
			node := cursor.CurrentNode()
			parent := len(stack) - 1
			ownText(stack[parent].pos, int(node.StartByte()))
			if importNode(language, node.Type()) {
				importNodes = append(importNodes, node)
			}
			if node.ChildCount() > 0 && cursor.GoToFirstChild() {
				push(node, int(node.StartByte()), int(node.EndByte()))
				continue
			}
			flags := 0
			if node.IsNamed() {
				flags |= tokenFlagNamed
			}
			if stack[parent].comment || node.IsNamed() && commentTypes[node.Type()] {
				flags |= tokenFlagComment
			} else if node.IsNamed() && identifierTypes[node.Type()] {
				flags |= tokenFlagIdentifier
			}
			if node.IsMissing() || node.IsError() || stack[parent].err {
				flags |= tokenFlagError
			}
			emit(parent, node.Type(), int(node.StartByte()), int(node.EndByte()), flags)
			if end := int(node.EndByte()); end > stack[parent].pos {
				stack[parent].pos = end
			}
			// Advance to the next node in preorder, closing every frame whose
			// children are exhausted on the way up.
			for !cursor.GoToNextSibling() {
				pop()
				if len(stack) == 0 || !cursor.GoToParent() {
					break walk
				}
			}
		}
		for len(stack) > 0 {
			pop()
		}
	}

	if len(rows) > tokensLeafLimit {
		return FileTokensRecord{
			RecordType: record.RecordType, FilePath: path, Language: language, Status: tokensOverSizeLimit,
			HasErrors: record.HasErrors, ImportBindings: importBindingsNotExtracted,
		}
	}
	rows = mapTokenRows(rows, view)
	rows, rewritten := mergeRewrittenTokenRows(rows, view, internType)
	lineStarts := []int{0}
	for i, b := range view.file {
		if b == '\n' {
			lineStarts = append(lineStarts, i+1)
		}
	}
	names := map[string]bool{}
	for _, row := range rows {
		startLine := outlineSourceLine(lineStarts, row.start)
		endLine := startLine
		if row.end > row.start {
			endLine = outlineSourceLine(lineStarts, row.end-1)
		}
		record.Leaves = append(record.Leaves, [8]int{row.typ, row.start, row.end, startLine, endLine, row.flags, row.ancestors, row.opens})
		if row.flags&tokenFlagIdentifier != 0 {
			names[string(view.file[row.start:row.end])] = true
		}
	}
	if len(rewritten) > 0 {
		for _, name := range rewrittenIdentifierNames(view, rewritten, identifierTypes, commentTypes) {
			names[name] = true
		}
	}
	for name := range names {
		record.IdentifierNames = append(record.IdentifierNames, name)
	}
	sort.Strings(record.IdentifierNames)
	if importBindingsCovered(language) {
		record.ImportBindings = importBindingsExtracted
		record.Imports = extractImportStatements(language, importNodes, view, lineStarts)
		if record.Imports == nil {
			record.Imports = []ImportStatementRecord{}
		}
	}
	return record
}

// mapTokenRows converts parse-view offsets to file offsets, dropping rows that
// describe only the synthetic Protocol Buffers syntax line.
func mapTokenRows(rows []tokenRow, view tokenSourceView) []tokenRow {
	if view.insertN == 0 {
		return rows
	}
	syntheticEnd := view.insertAt + view.insertN
	out := rows[:0]
	for _, row := range rows {
		if row.start >= view.insertAt && row.end <= syntheticEnd && !(row.start == row.end && row.start == syntheticEnd) {
			continue
		}
		row.start, _ = view.fileOffset(row.start)
		row.end, _ = view.fileOffset(row.end)
		out = append(out, row)
	}
	return out
}

type tokenSpan struct{ start, end int }

// mergeRewrittenTokenRows replaces the rows overlapping each span where the
// parse view's bytes differ from the file's (other than whitespace against
// whitespace) by one rewritten row covering the span and those rows.
func mergeRewrittenTokenRows(rows []tokenRow, view tokenSourceView, internType func(string) int) ([]tokenRow, []tokenSpan) {
	same := func(f int) bool {
		if f < len(utf8BOM) && bytes.HasPrefix(view.file, utf8BOM) {
			return true // a leading byte-order mark counts as whitespace
		}
		p := view.parsedOffset(f)
		return p < len(view.parsed) && (view.parsed[p] == view.file[f] || tokenSpace(view.file[f]) && tokenSpace(view.parsed[p]))
	}
	var spans []tokenSpan
	for f := 0; f < len(view.file); {
		if same(f) {
			f++
			continue
		}
		start := f
		for f < len(view.file) && !same(f) {
			f++
		}
		spans = append(spans, tokenSpan{start, f})
	}
	if len(spans) == 0 {
		return rows, nil
	}
	out := make([]tokenRow, 0, len(rows))
	i := 0
	for _, span := range spans {
		// Rows wholly before the span; a zero-width row at its start is inside.
		for i < len(rows) && rows[i].start < span.start && rows[i].end <= span.start {
			out = append(out, rows[i])
			i++
		}
		merged := tokenRow{typ: internType(""), start: span.start, end: span.end, flags: tokenFlagRewritten | tokenFlagError}
		for first := true; i < len(rows) && rows[i].start < span.end; i++ {
			if first {
				merged.ancestors, merged.opens = rows[i].ancestors, rows[i].opens
				first = false
			}
			merged.start = min(merged.start, rows[i].start)
			merged.end = max(merged.end, rows[i].end)
		}
		if last := len(out) - 1; last >= 0 && out[last].flags&tokenFlagRewritten != 0 && out[last].end >= merged.start {
			out[last].end = max(out[last].end, merged.end)
			continue
		}
		out = append(out, merged)
	}
	out = append(out, rows[i:]...)
	spans = spans[:0]
	for _, row := range out {
		if row.flags&tokenFlagRewritten != 0 {
			spans = append(spans, tokenSpan{row.start, row.end})
		}
	}
	return out, spans
}

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

func tokenSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f' || b == '\v'
}

// rewrittenIdentifierNames parses the file as given and returns the identifier
// names inside rewritten spans, so identifier_names stays complete where the
// parse view had to differ from the file.
func rewrittenIdentifierNames(view tokenSourceView, spans []tokenSpan, identifierTypes, commentTypes map[string]bool) []string {
	if view.grammar == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), treeSitterParseTimeout)
	defer cancel()
	parser := sitter.NewParser()
	defer parser.Close()
	parser.SetLanguage(view.grammar)
	structuralWorkCounts.parses.Add(1)
	tree, err := parser.ParseCtx(ctx, nil, view.file)
	if tree != nil {
		defer tree.Close()
	}
	if err != nil || tree == nil {
		return nil
	}
	var names []string
	cursor := sitter.NewTreeCursor(tree.RootNode())
	defer cursor.Close()
	commentDepth := -1
	depth := 0
	for {
		node := cursor.CurrentNode()
		if commentDepth < 0 && node.IsNamed() && commentTypes[node.Type()] {
			commentDepth = depth
		}
		if commentDepth < 0 && node.ChildCount() == 0 && node.IsNamed() && identifierTypes[node.Type()] {
			start, end := int(node.StartByte()), int(node.EndByte())
			for _, span := range spans {
				if start < span.end && end > span.start {
					names = append(names, string(view.file[start:end]))
					break
				}
			}
		}
		if cursor.GoToFirstChild() {
			depth++
			continue
		}
		for {
			if commentDepth == depth {
				commentDepth = -1
			}
			if cursor.GoToNextSibling() {
				break
			}
			if !cursor.GoToParent() {
				return names
			}
			depth--
		}
	}
}

func importNode(language, typ string) bool {
	switch language {
	case "TypeScript", "JavaScript":
		return typ == "import_statement"
	case "Go":
		return typ == "import_spec"
	case "Python":
		return typ == "import_statement" || typ == "import_from_statement" || typ == "future_import_statement"
	}
	return false
}

func extractImportStatements(language string, nodes []*sitter.Node, view tokenSourceView, lineStarts []int) []ImportStatementRecord {
	var out []ImportStatementRecord
	text := func(node *sitter.Node) string {
		if node == nil || node.IsNull() {
			return ""
		}
		start, end := int(node.StartByte()), int(node.EndByte())
		if end > len(view.file) || start > end {
			return ""
		}
		return string(view.file[start:end])
	}
	lines := func(node *sitter.Node) (int, int) {
		start := outlineSourceLine(lineStarts, int(node.StartByte()))
		end := start
		if node.EndByte() > node.StartByte() {
			end = outlineSourceLine(lineStarts, int(node.EndByte())-1)
		}
		return start, end
	}
	for _, node := range nodes {
		startLine, endLine := lines(node)
		switch language {
		case "TypeScript", "JavaScript":
			if statement, ok := jsImportStatement(node, text); ok {
				statement.StartLine, statement.EndLine = startLine, endLine
				out = append(out, statement)
			}
		case "Go":
			statement := goImportStatement(node, text)
			statement.StartLine, statement.EndLine = startLine, endLine
			out = append(out, statement)
		case "Python":
			for _, statement := range pythonImportStatements(node, text) {
				statement.StartLine, statement.EndLine = startLine, endLine
				out = append(out, statement)
			}
		}
	}
	return out
}

func unquoteImportSpecifier(literal string) string {
	if len(literal) >= 2 {
		first, last := literal[0], literal[len(literal)-1]
		if (first == '"' || first == '\'' || first == '`') && last == first {
			return literal[1 : len(literal)-1]
		}
	}
	return literal
}

// jsImportStatement reads an import_statement of the TypeScript, TSX or
// JavaScript grammar: `import type` and `import typeof` make every binding
// type-only, a specifier's own `type`/`typeof` makes that binding type-only.
func jsImportStatement(node *sitter.Node, text func(*sitter.Node) string) (ImportStatementRecord, bool) {
	statement := ImportStatementRecord{Bindings: []ImportBindingRecord{}}
	typeOnly := false
	var clause, require *sitter.Node
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		switch child.Type() {
		case "type", "typeof":
			if !child.IsNamed() {
				typeOnly = true
			}
		case "import_clause":
			clause = child
		case "import_require_clause":
			require = child
		}
	}
	if source := node.ChildByFieldName("source"); source != nil {
		statement.Specifier = unquoteImportSpecifier(text(source))
	}
	if require != nil {
		var local string
		for i := 0; i < int(require.ChildCount()); i++ {
			child := require.Child(i)
			switch child.Type() {
			case "identifier":
				if local == "" {
					local = text(child)
				}
			case "string":
				statement.Specifier = unquoteImportSpecifier(text(child))
			}
		}
		if source := require.ChildByFieldName("source"); source != nil {
			statement.Specifier = unquoteImportSpecifier(text(source))
		}
		statement.Bindings = append(statement.Bindings, ImportBindingRecord{Imported: "=", Local: local, TypeOnly: typeOnly})
		return statement, statement.Specifier != ""
	}
	if statement.Specifier == "" {
		return statement, false
	}
	if clause != nil {
		for i := 0; i < int(clause.ChildCount()); i++ {
			child := clause.Child(i)
			switch child.Type() {
			case "identifier":
				statement.Bindings = append(statement.Bindings, ImportBindingRecord{Imported: "default", Local: text(child), TypeOnly: typeOnly})
			case "namespace_import":
				for j := 0; j < int(child.NamedChildCount()); j++ {
					if name := child.NamedChild(j); name.Type() == "identifier" {
						statement.Bindings = append(statement.Bindings, ImportBindingRecord{Imported: "*", Local: text(name), TypeOnly: typeOnly})
					}
				}
			case "named_imports":
				for j := 0; j < int(child.NamedChildCount()); j++ {
					specifier := child.NamedChild(j)
					if specifier.Type() != "import_specifier" {
						continue
					}
					specifierTypeOnly := typeOnly
					for k := 0; k < int(specifier.ChildCount()); k++ {
						if part := specifier.Child(k); !part.IsNamed() && (part.Type() == "type" || part.Type() == "typeof") {
							specifierTypeOnly = true
						}
					}
					name := specifier.ChildByFieldName("name")
					if name == nil {
						continue
					}
					imported := text(name)
					if name.Type() == "string" {
						imported = unquoteImportSpecifier(imported)
					}
					local := imported
					if alias := specifier.ChildByFieldName("alias"); alias != nil {
						local = text(alias)
					}
					statement.Bindings = append(statement.Bindings, ImportBindingRecord{Imported: imported, Local: local, TypeOnly: specifierTypeOnly})
				}
			}
		}
	}
	statement.SideEffectOnly = len(statement.Bindings) == 0
	return statement, true
}

// goImportStatement reads one import_spec. Every Go import binds the whole
// package under one name: the explicit name, or the one Go tooling assumes.
func goImportStatement(node *sitter.Node, text func(*sitter.Node) string) ImportStatementRecord {
	statement := ImportStatementRecord{Bindings: []ImportBindingRecord{}}
	if pathNode := node.ChildByFieldName("path"); pathNode != nil {
		statement.Specifier = unquoteImportSpecifier(text(pathNode))
	}
	name := node.ChildByFieldName("name")
	switch {
	case name == nil:
		if local := goAssumedPackageName(statement.Specifier); local != "" {
			statement.Bindings = append(statement.Bindings, ImportBindingRecord{Imported: "*", Local: local, LocalInferred: true})
		} else {
			statement.NamesUnknown = true
		}
	case name.Type() == "blank_identifier":
		statement.SideEffectOnly = true
	case name.Type() == "dot":
		statement.NamesUnknown = true
	default:
		statement.Bindings = append(statement.Bindings, ImportBindingRecord{Imported: "*", Local: text(name)})
	}
	return statement
}

// goAssumedPackageName is goimports' ImportPathToAssumedName: the last path
// element, skipping a trailing vN element, without a go- prefix, cut at the
// first character that cannot appear in an identifier.
func goAssumedPackageName(importPath string) string {
	if importPath == "" {
		return ""
	}
	base := path.Base(importPath)
	if strings.HasPrefix(base, "v") && len(base) > 1 && strings.Trim(base[1:], "0123456789") == "" {
		if dir := path.Dir(importPath); dir != "." {
			base = path.Base(dir)
		}
	}
	base = strings.TrimPrefix(base, "go-")
	if i := strings.IndexFunc(base, func(r rune) bool {
		return !(r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r))
	}); i >= 0 {
		base = base[:i]
	}
	if base == "" || unicode.IsDigit(rune(base[0])) {
		return ""
	}
	return base
}

// pythonImportStatements reads import, from-import and __future__ statements.
// `import a, b` yields one entry per module.
func pythonImportStatements(node *sitter.Node, text func(*sitter.Node) string) []ImportStatementRecord {
	var out []ImportStatementRecord
	switch node.Type() {
	case "import_statement":
		for i := 0; i < int(node.NamedChildCount()); i++ {
			child := node.NamedChild(i)
			switch child.Type() {
			case "dotted_name":
				module := text(child)
				local, _, _ := strings.Cut(module, ".")
				out = append(out, ImportStatementRecord{Specifier: module, Bindings: []ImportBindingRecord{{Imported: "*", Local: local}}})
			case "aliased_import":
				name, alias := child.ChildByFieldName("name"), child.ChildByFieldName("alias")
				if name == nil || alias == nil {
					continue
				}
				out = append(out, ImportStatementRecord{Specifier: text(name), Bindings: []ImportBindingRecord{{Imported: "*", Local: text(alias)}}})
			}
		}
	case "import_from_statement", "future_import_statement":
		statement := ImportStatementRecord{Bindings: []ImportBindingRecord{}}
		module := node.ChildByFieldName("module_name")
		if node.Type() == "future_import_statement" {
			statement.Specifier = "__future__"
		} else if module != nil {
			statement.Specifier = text(module)
		}
		for i := 0; i < int(node.NamedChildCount()); i++ {
			child := node.NamedChild(i)
			if module != nil && child.Equal(module) {
				continue
			}
			switch child.Type() {
			case "wildcard_import":
				statement.NamesUnknown = true
			case "dotted_name":
				name := text(child)
				statement.Bindings = append(statement.Bindings, ImportBindingRecord{Imported: name, Local: name})
			case "aliased_import":
				name, alias := child.ChildByFieldName("name"), child.ChildByFieldName("alias")
				if name == nil || alias == nil {
					continue
				}
				statement.Bindings = append(statement.Bindings, ImportBindingRecord{Imported: text(name), Local: text(alias)})
			}
		}
		out = append(out, statement)
	}
	return out
}

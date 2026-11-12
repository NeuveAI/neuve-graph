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
// StartByte and EndByte are the statement node's range in the file as given,
// end exclusive like a leaf's, and the lines are that range's lines.
type ImportStatementRecord struct {
	StartLine      int                   `json:"start_line"`
	EndLine        int                   `json:"end_line"`
	StartByte      int                   `json:"start_byte"`
	EndByte        int                   `json:"end_byte"`
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
	// tokenFlagDirective marks the leaves of a comment that a compiler or a
	// tool reads, or may read (directives.go). It is set only with
	// tokenFlagComment.
	tokenFlagDirective = 32
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
// characters: every such byte is a row, whitespace included, and so is every
// byte of every node inside one (its interpolations and format clauses), unless
// interpolationLayout is set. Own text of any other node that is all
// whitespace is layout and has no row.
//
// interpolationLayout is set only for a language whose interpolations are
// plain expressions, so whitespace inside them never changes the value, and
// only with the test pairs that show it (TestFileTokensLiteralWhitespaceIsContent).
type tokenClasses struct {
	identifiers         []string
	comments            []string
	content             []string
	interpolationLayout bool
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
	"Ruby":       {identifiers: []string{"class_variable", "constant", "global_variable", "hash_key_symbol", "identifier", "instance_variable"}, comments: []string{"comment"}, content: []string{"character", "delimited_symbol", "heredoc_body", "regex", "string"}},
	"Go":         {identifiers: []string{"blank_identifier", "field_identifier", "identifier", "label_name", "package_identifier", "type_identifier"}, comments: []string{"comment"}, content: []string{"interpreted_string_literal", "raw_string_literal", "rune_literal"}},
	"JavaScript": {identifiers: []string{"identifier", "private_property_identifier", "property_identifier", "shorthand_property_identifier", "shorthand_property_identifier_pattern", "statement_identifier", "type_identifier"}, comments: []string{"comment", "html_comment"}, content: []string{"jsx_text", "regex", "string", "template_string"}, interpolationLayout: true},
	"Python":     {identifiers: []string{"identifier"}, comments: []string{"comment"}, content: []string{"string"}},
	"Rust":       {identifiers: []string{"field_identifier", "identifier", "metavariable", "shorthand_field_identifier", "type_identifier"}, comments: []string{"block_comment", "doc_comment", "inner_doc_comment_marker", "line_comment", "outer_doc_comment_marker"}, content: []string{"char_literal", "raw_string_literal", "string_literal"}},
	"TypeScript": {identifiers: []string{"identifier", "private_property_identifier", "property_identifier", "shorthand_property_identifier", "shorthand_property_identifier_pattern", "statement_identifier", "type_identifier"}, comments: []string{"comment", "html_comment"}, content: []string{"jsx_text", "regex", "string", "template_string"}, interpolationLayout: true},
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
	// directive: the comment this frame lies in is a directive comment.
	directive bool
	content   bool // the node's own text is content, whitespace included
	err       bool
}

// extractFileTokens walks the whole tree once. Every leaf becomes a row, and
// text a node owns between its children becomes a row typed with that node
// (all-whitespace text only inside a content node), so the rows tile the file.
// importNodes are the tree's import statement nodes as collectImportNodes
// gives them; the relation side reads the same list (treeImportStatements).
func extractFileTokens(path, language string, root *sitter.Node, view tokenSourceView, importNodes []*sitter.Node) FileTokensRecord {
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
	// stack as one row typed with that node, covering exactly those bytes.
	// Outside a content node a gap made only of layout bytes (space, tab,
	// carriage return, line feed) has no row; any other byte, every non-ASCII
	// space included (U+00A0, U+2028, U+2029, a byte-order mark after byte
	// 0), makes the gap a row. A byte-order mark at byte 0 is never a row.
	ownText := func(from, to int) {
		if from == 0 && bytes.HasPrefix(view.parsed, utf8BOM) && bytes.HasPrefix(view.file, utf8BOM) {
			from = len(utf8BOM)
		}
		if from >= to || to > len(view.parsed) {
			return
		}
		top := len(stack) - 1
		if !stack[top].content && onlyLayoutBytes(view.parsed[from:to]) {
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
			if frame.directive {
				flags |= tokenFlagDirective
			}
		} else if frame.node.IsNamed() && identifierTypes[frame.node.Type()] {
			flags |= tokenFlagIdentifier
		}
		if frame.err {
			flags |= tokenFlagError
		}
		emit(top, frame.node.Type(), start, end, flags)
	}
	push := func(node *sitter.Node, pos, end int) {
		parentComment, parentErr, parentContent, parentDirective := false, false, false, false
		if len(stack) > 0 {
			parent := stack[len(stack)-1]
			parentComment, parentErr, parentDirective = parent.comment, parent.err, parent.directive
			parentContent = parent.content && !classes.interpolationLayout
		}
		opensComment := !parentComment && node.IsNamed() && commentTypes[node.Type()]
		stack = append(stack, tokenFrame{
			directive: parentDirective || opensComment && directiveComment(language, string(view.parsed[node.StartByte():node.EndByte()])),
			node:      node, pos: pos, end: end, chainEnd: appendChain(node), hash: -1,
			comment: parentComment || (node.IsNamed() && commentTypes[node.Type()]),
			content: parentContent || node.IsNamed() && contentTypes[node.Type()],
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
				if stack[parent].directive || !stack[parent].comment && directiveComment(language, string(view.parsed[node.StartByte():node.EndByte()])) {
					flags |= tokenFlagDirective
				}
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
	if language == "Go" {
		markCgoPreamble(rows, importNodes, view.file)
		markExampleOutput(rows, view.file)
	}
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
		record.Imports = extractImportStatements(language, importNodes, view, lineStarts, record.Leaves)
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

// collectImportNodes gives the tree's import statement nodes (importNode) in
// source preorder. It is the one list both readers of a file's imports use:
// the tokens record's statements (extractImportStatements) and the relation
// side's (treeImportStatements). Only the languages whose bindings are
// extracted have such nodes.
func collectImportNodes(language string, root *sitter.Node) []*sitter.Node {
	if !importBindingsCovered(language) || root == nil || root.IsNull() {
		return nil
	}
	var nodes []*sitter.Node
	cursor := sitter.NewTreeCursor(root)
	defer cursor.Close()
	if !cursor.GoToFirstChild() {
		return nil
	}
	depth := 1
	for {
		node := cursor.CurrentNode()
		typ := node.Type()
		if importNode(language, typ) {
			nodes = append(nodes, node)
		}
		// Go's grammar allows an import_spec only inside an import_declaration,
		// and that only at the top level, so no other top-level node is
		// entered. Every other language's import statements can nest.
		descend := language != "Go" || depth > 1 || typ == "import_declaration"
		if descend && node.ChildCount() > 0 && cursor.GoToFirstChild() {
			depth++
			continue
		}
		for !cursor.GoToNextSibling() {
			if depth == 1 || !cursor.GoToParent() {
				return nodes
			}
			depth--
		}
	}
}

// importNodeText reads a node's text from the file as given; the parse view
// differs from it only by same-length masks in these languages.
func importNodeText(file []byte) func(*sitter.Node) string {
	return func(node *sitter.Node) string {
		if node == nil || node.IsNull() {
			return ""
		}
		start, end := int(node.StartByte()), int(node.EndByte())
		if end > len(file) || start > end {
			return ""
		}
		return string(file[start:end])
	}
}

// importStatementsOf reads one import statement node: its specifiers and
// bindings, without ranges. A node gives none when it names no specifier.
func importStatementsOf(language string, node *sitter.Node, text func(*sitter.Node) string) []ImportStatementRecord {
	switch language {
	case "TypeScript", "JavaScript":
		if statement, ok := jsImportStatement(node, text); ok {
			return []ImportStatementRecord{statement}
		}
	case "Go":
		return []ImportStatementRecord{goImportStatement(node, text)}
	case "Python":
		return pythonImportStatements(node, text)
	}
	return nil
}

// treeImportStatement is what the relation side keeps of one tokens-record
// import statement: its specifier as written and, for a Python `from` import,
// the names it imports, which the relation side's Python convention reads.
type treeImportStatement struct {
	Specifier  string   `json:"specifier"`
	PythonFrom bool     `json:"python_from,omitempty"`
	Names      []string `json:"names,omitempty"`
}

// treeImportStatements reads the statements the tokens record would give for
// these nodes, in the same order, keeping what the relation side needs.
func treeImportStatements(language string, nodes []*sitter.Node, file []byte) []treeImportStatement {
	text := importNodeText(file)
	var out []treeImportStatement
	for _, node := range nodes {
		pythonFrom := language == "Python" && (node.Type() == "import_from_statement" || node.Type() == "future_import_statement")
		for _, statement := range importStatementsOf(language, node, text) {
			kept := treeImportStatement{Specifier: statement.Specifier, PythonFrom: pythonFrom}
			if pythonFrom {
				for _, binding := range statement.Bindings {
					kept.Names = append(kept.Names, binding.Imported)
				}
			}
			out = append(out, kept)
		}
	}
	return out
}

func extractImportStatements(language string, nodes []*sitter.Node, view tokenSourceView, lineStarts []int, leaves [][8]int) []ImportStatementRecord {
	var out []ImportStatementRecord
	text := importNodeText(view.file)
	for _, node := range nodes {
		// The statement is the grammar's import statement node. For Go it is
		// the import_spec, except that a declaration holding one spec without
		// parentheses (`import "fmt"`) is the statement as a whole, keyword
		// included; in a grouped `import ( … )` the keyword and parentheses
		// belong to no statement.
		span := node
		if language == "Go" {
			if parent := node.Parent(); parent != nil && parent.Type() == "import_declaration" {
				span = parent
			}
		}
		// The range runs from the start of the node's first non-comment leaf
		// to the end of its last, so a leading or trailing comment the grammar
		// puts inside the node is outside; a comment in the middle stays in.
		startByte, endByte := int(span.StartByte()), int(span.EndByte())
		first, last := -1, -1
		for i := sort.Search(len(leaves), func(i int) bool { return leaves[i][1] >= startByte }); i < len(leaves) && leaves[i][2] <= endByte; i++ {
			if leaves[i][5]&tokenFlagComment != 0 || leaves[i][1] == leaves[i][2] {
				continue
			}
			if first < 0 {
				first = i
			}
			last = i
		}
		if first >= 0 {
			startByte, endByte = leaves[first][1], leaves[last][2]
		}
		startLine := outlineSourceLine(lineStarts, startByte)
		endLine := startLine
		if endByte > startByte {
			endLine = outlineSourceLine(lineStarts, endByte-1)
		}
		for _, statement := range importStatementsOf(language, node, text) {
			statement.StartLine, statement.EndLine = startLine, endLine
			statement.StartByte, statement.EndByte = startByte, endByte
			out = append(out, statement)
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
// `import a, b` yields one entry per module. Module names, binding names and
// specifiers are built from the identifier leaves that spell them, joined with
// ".", never from raw text, so spacing around dots, a backslash continuation,
// parentheses and comments never reach a name.
func pythonImportStatements(node *sitter.Node, text func(*sitter.Node) string) []ImportStatementRecord {
	var out []ImportStatementRecord
	switch node.Type() {
	case "import_statement":
		for i := 0; i < int(node.NamedChildCount()); i++ {
			child := node.NamedChild(i)
			switch child.Type() {
			case "dotted_name":
				module := pythonDottedName(child, text)
				local, _, _ := strings.Cut(module, ".")
				out = append(out, ImportStatementRecord{Specifier: module, Bindings: []ImportBindingRecord{{Imported: "*", Local: local}}})
			case "aliased_import":
				name, alias := child.ChildByFieldName("name"), child.ChildByFieldName("alias")
				if name == nil || alias == nil {
					continue
				}
				out = append(out, ImportStatementRecord{Specifier: pythonDottedName(name, text), Bindings: []ImportBindingRecord{{Imported: "*", Local: pythonDottedName(alias, text)}}})
			}
		}
	case "import_from_statement", "future_import_statement":
		statement := ImportStatementRecord{Bindings: []ImportBindingRecord{}}
		module := node.ChildByFieldName("module_name")
		if node.Type() == "future_import_statement" {
			statement.Specifier = "__future__"
		} else if module != nil {
			statement.Specifier = pythonModuleName(module, text)
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
				name := pythonDottedName(child, text)
				statement.Bindings = append(statement.Bindings, ImportBindingRecord{Imported: name, Local: name})
			case "aliased_import":
				name, alias := child.ChildByFieldName("name"), child.ChildByFieldName("alias")
				if name == nil || alias == nil {
					continue
				}
				statement.Bindings = append(statement.Bindings, ImportBindingRecord{Imported: pythonDottedName(name, text), Local: pythonDottedName(alias, text)})
			}
		}
		out = append(out, statement)
	}
	return out
}

// pythonDottedName joins the identifier leaves under node with ".".
func pythonDottedName(node *sitter.Node, text func(*sitter.Node) string) string {
	if node.Type() == "identifier" {
		return text(node)
	}
	var parts []string
	for i := 0; i < int(node.NamedChildCount()); i++ {
		if child := node.NamedChild(i); child.Type() == "identifier" {
			parts = append(parts, text(child))
		}
	}
	return strings.Join(parts, ".")
}

// pythonModuleName is a from-import's module: a dotted name, or a relative
// import, whose leading dots are counted from the dot tokens of its prefix
// (`from . . import x` is level two) and followed by its dotted name, if any.
func pythonModuleName(node *sitter.Node, text func(*sitter.Node) string) string {
	if node.Type() != "relative_import" {
		return pythonDottedName(node, text)
	}
	dots, name := 0, ""
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		switch child.Type() {
		case "import_prefix":
			dots += strings.Count(text(child), ".")
		case "dotted_name":
			name = pythonDottedName(child, text)
		}
	}
	return strings.Repeat(".", dots) + name
}

// markCgoPreamble flags as directives the comment group directly above each
// `import "C"`: cgo compiles it as C. The group is the run of comment rows
// that ends on the line before the import declaration, each row separated from
// the next by whitespace holding at most one line break.
func markCgoPreamble(rows []tokenRow, importNodes []*sitter.Node, file []byte) {
	for _, spec := range importNodes {
		path := spec.ChildByFieldName("path")
		if path == nil || unquoteImportSpecifier(string(file[path.StartByte():path.EndByte()])) != "C" {
			continue
		}
		declaration := spec
		for declaration != nil && declaration.Type() != "import_declaration" {
			declaration = declaration.Parent()
		}
		if declaration == nil {
			continue
		}
		next := int(declaration.StartByte())
		i := sort.Search(len(rows), func(i int) bool { return rows[i].start >= next }) - 1
		for ; i >= 0 && rows[i].flags&tokenFlagComment != 0; i-- {
			if bytes.Count(file[rows[i].end:next], []byte("\n")) > 1 || len(bytes.TrimSpace(file[rows[i].end:next])) > 0 {
				break
			}
			rows[i].flags |= tokenFlagDirective
			next = rows[i].start
		}
	}
}

// onlyLayoutBytes reports whether a gap holds only the bytes that are layout
// in every declared language: space, tab, carriage return and line feed.
// Form feed, vertical tab and every non-ASCII space are not layout: a gap that
// holds one is a row, so a change to it reads as a token change.
func onlyLayoutBytes(gap []byte) bool {
	for _, b := range gap {
		if b != ' ' && b != '\t' && b != '\r' && b != '\n' {
			return false
		}
	}
	return true
}

// markExampleOutput flags every comment in a comment group that holds a Go
// example's "// Output:" or "// Unordered output:" line: go test compares the
// lines that follow it with the example's output. A group is a run of comment
// rows separated by whitespace holding at most one line break.
func markExampleOutput(rows []tokenRow, file []byte) {
	isOutput := func(row tokenRow) bool {
		text := bytes.TrimSpace(bytes.TrimPrefix(file[row.start:row.end], []byte("//")))
		return bytes.HasPrefix(text, []byte("Output:")) || bytes.HasPrefix(text, []byte("Unordered output:"))
	}
	joined := func(a, b tokenRow) bool {
		gap := file[a.end:b.start]
		return bytes.Count(gap, []byte("\n")) <= 1 && len(bytes.TrimSpace(gap)) == 0
	}
	for i := 0; i < len(rows); i++ {
		if rows[i].flags&tokenFlagComment == 0 {
			continue
		}
		j := i
		for j+1 < len(rows) && rows[j+1].flags&tokenFlagComment != 0 && joined(rows[j], rows[j+1]) {
			j++
		}
		output := false
		for k := i; k <= j; k++ {
			output = output || isOutput(rows[k])
		}
		if output {
			for k := i; k <= j; k++ {
				rows[k].flags |= tokenFlagDirective
			}
		}
		i = j
	}
}

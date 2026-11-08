package sem

import (
	"bytes"
	"path/filepath"
	"sort"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
)

// FileOutlineRecord is an opt-in symbols-stream record, never a cached snapshot
// payload. Identifier rows have a 1-based line number followed by distinct names.
type FileOutlineRecord struct {
	RecordType  string        `json:"record_type"`
	FilePath    string        `json:"file_path"`
	Language    string        `json:"language"`
	TopLevel    []OutlineNode `json:"top_level"`
	Identifiers [][]any       `json:"identifiers"`
}

type OutlineNode struct {
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Type      string `json:"type"`
	Imports   bool   `json:"imports"`
}

func parseWithOutline(spec profileSpec, langSpec languageSpec, path, content string) ([]Entity, string, ParseStatus, *FileOutlineRecord) {
	if !spec.outline {
		entities, language, status := parseWithProfile(TreeSitterParser{}, spec, langSpec, path, content)
		return entities, language, status, nil
	}
	outline := &FileOutlineRecord{}
	entities, language, status := (TreeSitterParser{outline: outline}).ParseWithStatus(path, content)
	if useFastCFamilyParser(spec, langSpec) {
		// These profiles normally use structural extraction without a tree. Keep
		// their symbol inventory while parsing once for the requested outline.
		entities = fastCFamilyEntities(path, content, langSpec.language)
	}
	if outline.RecordType == "" || status.ParseError || status.Partial {
		outline = nil
	}
	return entities, language, status, outline
}

func extractFileOutline(path, language string, root *sitter.Node, src, parsed []byte, lineOffset int) FileOutlineRecord {
	outline := FileOutlineRecord{
		RecordType: "file_outline", FilePath: path, Language: language,
		TopLevel: []OutlineNode{}, Identifiers: [][]any{},
	}
	lineStarts := []int{0}
	sameLines := len(src) == len(parsed)
	for i, b := range src {
		if b == '\n' {
			lineStarts = append(lineStarts, i+1)
		}
		if i >= len(parsed) || (b == '\n') != (parsed[i] == '\n') {
			sameLines = false
		}
	}
	// Scan once with the provider's full-file context, before assigning source
	// lines to root children. The protobuf source view includes a synthetic line.
	importLines := map[int]bool{}
	if scan := importScanners[strings.ToLower(filepath.Ext(path))]; scan != nil {
		scan(string(src), func(_ string, line int) { importLines[line-lineOffset] = true })
	}
	lines := make([]int, 0, len(importLines))
	for line := range importLines {
		lines = append(lines, line)
	}
	sort.Ints(lines)
	cursor := sitter.NewTreeCursor(root)
	defer cursor.Close()
	depth := 0
	lastLine := 0
	seen := map[string]bool{}
	for {
		node := cursor.CurrentNode()
		typ := node.Type()
		comment := strings.Contains(typ, "comment")
		line := int(node.StartPoint().Row) + 1 - lineOffset
		if !sameLines {
			line = outlineSourceLine(lineStarts, int(node.StartByte())) - lineOffset
		}
		if depth == 1 && node.IsNamed() && !comment && line > 0 && (sameLines || outlineAuthoredNode(node, src, parsed)) {
			// EndByte is exclusive. Resolve both boundaries in the original
			// source, not in a mask that may relocate a newline within a span.
			endLine := outlineSourceLine(lineStarts, int(node.EndByte())-1) - lineOffset
			outline.TopLevel = append(outline.TopLevel, OutlineNode{
				StartLine: line, EndLine: endLine, Type: typ,
				Imports: outlineHasImport(lines, line, endLine),
			})
		}
		skip := comment || outlineLiteralNode(typ) || line <= 0 && depth > 0
		if !skip && node.IsNamed() && node.NamedChildCount() == 0 && outlineIdentifierNode(typ) && outlineAuthoredNode(node, src, parsed) {
			name := node.Content(src)
			if line != lastLine {
				outline.Identifiers = append(outline.Identifiers, []any{line})
				lastLine = line
				clear(seen)
			}
			if !seen[name] {
				row := len(outline.Identifiers) - 1
				outline.Identifiers[row] = append(outline.Identifiers[row], name)
				seen[name] = true
			}
		}
		if !skip && cursor.GoToFirstChild() {
			depth++
			continue
		}
		for !cursor.GoToNextSibling() {
			if !cursor.GoToParent() {
				return outline
			}
			depth--
		}
	}
}

func outlineSourceLine(starts []int, offset int) int {
	return sort.Search(len(starts), func(i int) bool { return starts[i] > offset })
}

func outlineHasImport(lines []int, start, end int) bool {
	i := sort.SearchInts(lines, start)
	return i < len(lines) && lines[i] <= end
}

func outlineAuthoredNode(node *sitter.Node, src, parsed []byte) bool {
	start, end := node.StartByte(), node.EndByte()
	return start < end && uint64(end) <= uint64(len(src)) && uint64(end) <= uint64(len(parsed)) &&
		bytes.Equal(src[start:end], parsed[start:end])
}

func outlineIdentifierNode(typ string) bool {
	return strings.HasSuffix(typ, "identifier") || typ == "variable_name" || typ == "name"
}

func outlineLiteralNode(typ string) bool {
	// These are literal grammar nodes, not substring matches: Elixir's
	// bitstring and OCaml's string_get_expression contain ordinary code.
	switch typ {
	case "string", "string_literal", "string_lit", "template_string",
		"interpreted_string_literal", "raw_string_literal", "verbatim_string_literal",
		"line_string_literal", "multi_line_string_literal", "multiline_string_literal",
		"interpolated_string", "interpolated_string_expression", "concatenated_string",
		"encapsed_string", "nowdoc_string", "quoted_string", "quoted_template",
		"heredoc", "heredoc_body", "heredoc_template", "heredocument",
		"raw_string", "ansi_c_string", "translated_string", "string_array",
		"bare_string", "chained_string", "string_scalar", "system_lib_string",
		"char_literal", "character_literal", "character", "char", "rune_literal",
		"regex", "regex_literal", "sigil", "charlist", "quoted_atom", "quoted_keyword":
		return true
	}
	return false
}

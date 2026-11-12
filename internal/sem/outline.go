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
	StartLine int           `json:"start_line"`
	EndLine   int           `json:"end_line"`
	Type      string        `json:"type"`
	Imports   bool          `json:"imports"`
	Children  []OutlineNode `json:"children,omitempty"`
}

func parseWithOutline(spec profileSpec, langSpec languageSpec, path, content string) ([]Entity, string, ParseStatus, *FileOutlineRecord) {
	entities, language, status, outline, _ := parseWithOutlineTokens(spec, langSpec, path, content)
	return entities, language, status, outline
}

// parseWithOutlineTokens also returns the tokens record when spec.tokens asks
// for it, extracted from the same tree as the outline.
func parseWithOutlineTokens(spec profileSpec, langSpec languageSpec, path, content string) ([]Entity, string, ParseStatus, *FileOutlineRecord, *FileTokensRecord) {
	entities, language, status, outline, tokens, _ := parseWithOutlineTokensImports(spec, langSpec, path, content)
	return entities, language, status, outline, tokens
}

// parseWithOutlineTokensImports also returns the file's import statements as
// the tokens record reads them, from the same parse. They are nil whenever the
// file would have no tokens record imports for want of a usable tree: no tree,
// a tree with an error, a partial parse, or a language whose bindings are not
// extracted. Unlike the tokens record they do not depend on --tokens or on the
// tokens record's size limits.
func parseWithOutlineTokensImports(spec profileSpec, langSpec languageSpec, path, content string) ([]Entity, string, ParseStatus, *FileOutlineRecord, *FileTokensRecord, []treeImportStatement) {
	var imports []treeImportStatement
	if !spec.outline {
		entities, language, status := parseWithProfile(TreeSitterParser{imports: &imports}, spec, langSpec, path, content)
		if status.ParseError || status.Partial {
			imports = nil
		}
		return entities, language, status, nil, nil, imports
	}
	outline := &FileOutlineRecord{}
	var tokens *FileTokensRecord
	if spec.tokens {
		tokens = &FileTokensRecord{}
	}
	entities, language, status := (TreeSitterParser{outline: outline, tokens: tokens, imports: &imports}).ParseWithStatus(path, content)
	if status.ParseError || status.Partial {
		imports = nil
	}
	if useFastCFamilyParser(spec, langSpec) {
		// These profiles normally use structural extraction without a tree. Keep
		// their symbol inventory while parsing once for the requested outline.
		entities = fastCFamilyEntities(path, content, langSpec.language)
	}
	// A file without an outline has no tokens record either, for the same
	// reason, which the file's partial failure already states.
	if outline.RecordType == "" || status.ParseError || status.Partial {
		outline, tokens = nil, nil
	}
	if tokens != nil && tokens.RecordType == "" {
		tokens = nil
	}
	return entities, language, status, outline, tokens, imports
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
	// Retain only named-node metadata until the extraction tree closes. Candidate
	// lists update their ancestors on exit, so selection needs one cursor walk.
	nodes := []outlineSyntaxNode{}
	stack := []int{}
	cursor := sitter.NewTreeCursor(root)
	defer cursor.Close()
	depth, opaqueDepth := 0, -1
	lastLine := 0
	seen := map[string]bool{}
	for {
		node := cursor.CurrentNode()
		typ := node.Type()
		comment := strings.Contains(typ, "comment")
		line := outlineSourceLine(lineStarts, int(node.StartByte())) - lineOffset
		index := -1
		if node.IsNamed() && !comment {
			index = len(nodes)
			nodes = append(nodes, outlineSyntaxNode{node: node, best: -1})
			if depth > 0 && stack[depth-1] >= 0 {
				parent := &nodes[stack[depth-1]]
				parent.children = append(parent.children, index)
				parent.childBytes += uint64(node.EndByte() - node.StartByte())
			}
		}
		stack = append(stack, index)
		if opaqueDepth < 0 && outlineLiteralNode(typ) {
			opaqueDepth = depth
		}
		if opaqueDepth < 0 && !comment && line > 0 && node.IsNamed() && node.NamedChildCount() == 0 && outlineIdentifierNode(typ) && outlineAuthoredNode(node, src, parsed) {
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
		if !comment && cursor.GoToFirstChild() {
			depth++
			continue
		}
		for {
			index = stack[depth]
			if index >= 0 {
				current := &nodes[index]
				if len(current.children) >= 2 {
					for ancestorDepth := depth; ancestorDepth >= 0 && depth-ancestorDepth <= 12; ancestorDepth-- {
						if ancestor := stack[ancestorDepth]; ancestor >= 0 {
							held := &nodes[ancestor]
							if held.best < 0 || current.childBytes > nodes[held.best].childBytes ||
								current.childBytes == nodes[held.best].childBytes && index < held.best {
								held.best = index
							}
						}
					}
				}
			}
			stack = stack[:depth]
			if opaqueDepth == depth {
				opaqueDepth = -1
			}
			if cursor.GoToNextSibling() {
				break
			}
			if !cursor.GoToParent() {
				for _, child := range nodes[0].children {
					n := nodes[child].node
					if outlineSourceLine(lineStarts, int(n.StartByte())) > lineOffset && (sameLines || outlineAuthoredNode(n, src, parsed)) {
						outline.TopLevel = append(outline.TopLevel, outlineNode(nodes, child, 0, 0, lineStarts, lines, lineOffset))
					}
				}
				return outline
			}
			depth--
		}
	}
}

// best identifies the descendant owning the largest sibling list within twelve
// tree edges. Ties use source preorder, including the node itself first.
type outlineSyntaxNode struct {
	node       *sitter.Node
	children   []int
	best       int
	childBytes uint64
}

func outlineNode(nodes []outlineSyntaxNode, index, splitDepth, edgeDepth int, starts, imports []int, lineOffset int) OutlineNode {
	entry := nodes[index]
	n := entry.node
	start := outlineSourceLine(starts, int(n.StartByte())) - lineOffset
	end := outlineSourceLine(starts, int(n.EndByte())-1) - lineOffset
	out := OutlineNode{StartLine: start, EndLine: end, Type: n.Type(), Imports: outlineHasImport(imports, start, end)}
	if splitDepth < 4 && edgeDepth < 16 && start < end && entry.best >= 0 && nodes[entry.best].childBytes*2 >= uint64(n.EndByte()-n.StartByte()) {
		chosen := nodes[entry.best]
		// Match the harness's 256-byte small-unit threshold: exactly one child
		// at least 256 bytes and all others smaller makes a pass-through list.
		// Only real splits consume the four-level budget; all edges count to 16.
		substantial := 0
		for _, child := range chosen.children {
			c := nodes[child].node
			if c.EndByte()-c.StartByte() >= 256 {
				substantial++
			}
		}
		if substantial != 1 {
			splitDepth++
		}
		for _, child := range chosen.children {
			out.Children = append(out.Children, outlineNode(nodes, child, splitDepth, edgeDepth+1, starts, imports, lineOffset))
		}
	}
	return out
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

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
	Label     string        `json:"label,omitempty"`
	Callback  bool          `json:"callback,omitempty"`
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
	// Retain only named-node metadata until the extraction tree closes. Candidate
	// lists update their ancestors on exit, so selection needs one cursor walk.
	nodes := []outlineSyntaxNode{}
	literals := []int{}
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
			nodes = append(nodes, outlineSyntaxNode{node: node, best: -1,
				interpolated: strings.Contains(typ, "interpolation") || strings.Contains(typ, "substitution") ||
					strings.Contains(typ, "expansion") || typ == "interpolated_expression" || typ == "interpolated_identifier" ||
					opaqueDepth >= 0 && depth > opaqueDepth && outlineIdentifierNode(typ)})
			if depth > 0 && stack[depth-1] >= 0 {
				parent := &nodes[stack[depth-1]]
				parent.children = append(parent.children, index)
				parent.childBytes += uint64(node.EndByte() - node.StartByte())
			}
			if opaqueDepth < 0 && outlineStringNode(typ) {
				literals = append(literals, index)
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
				if depth > 0 && stack[depth-1] >= 0 && current.interpolated {
					nodes[stack[depth-1]].interpolated = true
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
						outline.TopLevel = append(outline.TopLevel, outlineNode(nodes, literals, child, 0, language, src, parsed, lineStarts, lines, lineOffset))
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
	node         *sitter.Node
	children     []int
	best         int
	childBytes   uint64
	interpolated bool
}

func outlineNode(nodes []outlineSyntaxNode, literals []int, index, depth int, language string, src, parsed []byte, starts, imports []int, lineOffset int) OutlineNode {
	entry := nodes[index]
	n := entry.node
	start := outlineSourceLine(starts, int(n.StartByte())) - lineOffset
	end := outlineSourceLine(starts, int(n.EndByte())-1) - lineOffset
	out := OutlineNode{StartLine: start, EndLine: end, Type: n.Type(), Imports: outlineHasImport(imports, start, end)}
	cutoff := n.EndByte()
	if depth < 4 && start < end && entry.best >= 0 && nodes[entry.best].childBytes*2 >= uint64(n.EndByte()-n.StartByte()) {
		chosen := nodes[entry.best]
		cutoff = nodes[chosen.children[0]].node.StartByte()
		for _, child := range chosen.children {
			out.Children = append(out.Children, outlineNode(nodes, literals, child, depth+1, language, src, parsed, starts, imports, lineOffset))
		}
		out.Callback = outlineCallback(n, chosen.node, language)
	}
	first := sort.Search(len(literals), func(i int) bool { return nodes[literals[i]].node.StartByte() >= n.StartByte() })
	for _, literal := range literals[first:] {
		if nodes[literal].node.StartByte() >= cutoff || outlineSourceLine(starts, int(nodes[literal].node.StartByte()))-lineOffset != start {
			break
		}
		entry := nodes[literal]
		if entry.node.EndByte() <= n.EndByte() && !entry.interpolated && outlineAuthoredNode(entry.node, src, parsed) {
			out.Label = outlineLabel(entry.node.Content(src), entry.node.Type())
		}
		break
	}
	return out
}

func outlineCallback(node, body *sitter.Node, language string) bool {
	fn := body.Parent()
	if !functionLikeValue(fn) || fn.StartByte() < node.StartByte() || fn.EndByte() > node.EndByte() {
		return false
	}
	// function_definition is also a named declaration in several grammars;
	// Graph knows it as an anonymous function value specifically in R.
	if fn.Type() == "function_definition" && language != "R" {
		return false
	}
	return validNode(fn.ChildByFieldName("body")) && fn.ChildByFieldName("body").Equal(body)
}

func outlineStringNode(typ string) bool {
	if !outlineLiteralNode(typ) {
		return false
	}
	switch typ {
	case "char_literal", "character_literal", "character", "char", "rune_literal",
		"regex", "regex_literal", "sigil", "charlist", "quoted_atom", "quoted_keyword", "string_array":
		return false
	}
	return true
}

// Strip delimiters, not escapes: labels are authored inner text, not evaluated
// values. Prefixes cover raw/byte/unicode strings and C#/Python quote prefixes.
func outlineLabel(text, typ string) string {
	inner, ok := outlineStringInner(text, typ)
	if !ok {
		return ""
	}
	label := strings.TrimSpace(inner)
	count := 0
	for offset := range label {
		if count == 80 {
			label = label[:offset]
			break
		}
		count++
	}
	return strings.Clone(label)
}

func outlineStringInner(text, typ string) (string, bool) {
	// Lua long strings, OCaml quoted strings, SQL dollar strings, and C-family
	// include literals use balanced delimiters rather than quote characters.
	if strings.HasPrefix(text, "[") {
		if end := strings.Index(text[1:], "["); end >= 0 && strings.Trim(text[1:1+end], "=") == "" {
			open := text[:end+2]
			close := "]" + open[1:len(open)-1] + "]"
			if len(text) >= len(open)+len(close) && strings.HasSuffix(text, close) {
				return text[len(open) : len(text)-len(close)], true
			}
		}
	}
	if strings.HasPrefix(text, "{") {
		if end := strings.IndexByte(text, '|'); end >= 1 {
			close := "|" + text[1:end] + "}"
			if len(text) >= end+1+len(close) && strings.HasSuffix(text, close) {
				return text[end+1 : len(text)-len(close)], true
			}
		}
	}
	if strings.HasPrefix(text, "$") {
		if end := strings.IndexByte(text[1:], '$'); end >= 0 {
			delim := text[:end+2]
			if len(text) >= 2*len(delim) && strings.HasSuffix(text, delim) {
				return text[len(delim) : len(text)-len(delim)], true
			}
		}
	}
	if typ == "system_lib_string" && strings.HasPrefix(text, "<") && strings.HasSuffix(text, ">") {
		return text[1 : len(text)-1], true
	}
	if typ == "bare_string" || typ == "string_scalar" || typ == "heredoc_body" {
		if !strings.HasPrefix(text, "\"") && !strings.HasPrefix(text, "'") {
			return text, true
		}
	}
	quote := strings.IndexAny(text, "\"'`")
	if quote < 0 {
		return "", false
	}
	prefix := text[:quote]
	if strings.Trim(prefix, "rRuUbBfFcCsS@$#") != "" {
		return "", false
	}
	delim := text[quote : quote+1]
	if strings.HasPrefix(text[quote:], strings.Repeat(delim, 3)) {
		delim = strings.Repeat(delim, 3)
	}
	closing := delim + strings.Repeat("#", strings.Count(prefix, "#"))
	if len(text) < quote+len(delim)+len(closing) || !strings.HasSuffix(text, closing) {
		return "", false
	}
	return text[quote+len(delim) : len(text)-len(closing)], true
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

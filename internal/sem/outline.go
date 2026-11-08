package sem

import (
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

func extractFileOutline(path, language string, root *sitter.Node, src []byte, lineOffset int) FileOutlineRecord {
	outline := FileOutlineRecord{
		RecordType: "file_outline", FilePath: path, Language: language,
		TopLevel: []OutlineNode{}, Identifiers: [][]any{},
	}
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
		if depth == 1 && node.IsNamed() && !comment && line > 0 {
			end := node.EndPoint()
			endLine := int(end.Row) + 1 - lineOffset
			if end.Column == 0 && endLine > line {
				endLine-- // tree-sitter's end position is exclusive
			}
			outline.TopLevel = append(outline.TopLevel, OutlineNode{
				StartLine: line, EndLine: endLine, Type: typ,
				Imports: len(importsFor(path, node.Content(src))) > 0,
			})
		}
		skip := comment || outlineLiteralNode(typ) || line <= 0 && depth > 0
		if !skip && node.IsNamed() && node.NamedChildCount() == 0 && outlineIdentifierNode(typ) {
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

func outlineIdentifierNode(typ string) bool {
	return strings.HasSuffix(typ, "identifier") || typ == "variable_name" || typ == "name"
}

func outlineLiteralNode(typ string) bool {
	if outlineIdentifierNode(typ) {
		return false
	}
	return strings.Contains(typ, "string") || strings.Contains(typ, "heredoc") ||
		strings.Contains(typ, "heredocument") || strings.Contains(typ, "quoted") ||
		typ == "char_literal" || typ == "character_literal" || typ == "regex" ||
		typ == "regex_literal" || typ == "sigil" || typ == "charlist"
}

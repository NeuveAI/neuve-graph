package sem

import sitter "github.com/smacker/go-tree-sitter"

func jsStaticObjectKey(node *sitter.Node, src []byte) string {
	if !validNode(node) {
		return ""
	}
	key := node.ChildByFieldName("key")
	if !validNode(key) {
		return ""
	}
	switch key.Type() {
	case "property_identifier", "identifier":
		return key.Content(src)
	case "string":
		text := key.Content(src)
		if len(text) < 2 || text[0] != text[len(text)-1] || text[0] != '\'' && text[0] != '"' {
			return ""
		}
		text = text[1 : len(text)-1]
		if text != "" && isJSIdentifierStart(text[0]) {
			for index := 1; index < len(text); index++ {
				if !isJSIdentifierPart(text[index]) {
					return ""
				}
			}
			return text
		}
	}
	return ""
}

func jsObjectContainsNamedCallable(node *sitter.Node, src []byte, depth int) bool {
	if !validNode(node) || node.Type() != "object" || depth >= maxParseWalkDepth {
		return false
	}
	for index := 0; index < int(node.NamedChildCount()); index++ {
		child := node.NamedChild(index)
		if child.Type() != "pair" || jsStaticObjectKey(child, src) == "" {
			continue
		}
		value := child.ChildByFieldName("value")
		if functionLikeValue(value) || jsObjectContainsNamedCallable(value, src, depth+1) {
			return true
		}
	}
	return false
}

func jsNamedObjectBinding(node *sitter.Node, src []byte, language string) bool {
	if !functionLocalScopeResets(language) || !validNode(node) || node.Type() != "variable_declarator" {
		return false
	}
	name := node.ChildByFieldName("name")
	return validNode(name) && name.Type() == "identifier" && jsObjectContainsNamedCallable(node.ChildByFieldName("value"), src, 0)
}

// New property identities retain their static object path and nearest named
// lexical scope. Existing declarations keep the parser's established names.
func jsObjectPropertyQualifiedName(node *sitter.Node, src []byte, scope string) string {
	name := jsStaticObjectKey(node, src)
	if name == "" {
		return ""
	}
	current := node.Parent()
	for depth := 0; validNode(current) && depth < maxParseWalkDepth; depth++ {
		if functionLikeValue(current) || current.Type() == "function_declaration" || current.Type() == "method_definition" {
			break
		}
		if current.Type() == "pair" {
			key := jsStaticObjectKey(current, src)
			if key == "" {
				return ""
			}
			name = qualify(key, name)
		}
		if current.Type() == "variable_declarator" {
			binding := current.ChildByFieldName("name")
			if !validNode(binding) || binding.Type() != "identifier" {
				return ""
			}
			name = qualify(binding.Content(src), name)
			break
		}
		current = current.Parent()
	}
	return qualify(jsObjectLexicalScope(node, src, scope), name)
}

func jsObjectBindingQualifiedName(node *sitter.Node, src []byte, scope string) string {
	return qualify(jsObjectLexicalScope(node, src, scope), nodeName(node, src))
}

func jsObjectPropertyHasNamedContainer(node *sitter.Node, src []byte) bool {
	object := node.Parent()
	if !validNode(object) || object.Type() != "object" {
		return false
	}
	owner := object.Parent()
	if !validNode(owner) {
		return false
	}
	if owner.Type() == "variable_declarator" {
		name := owner.ChildByFieldName("name")
		return validNode(name) && name.Type() == "identifier"
	}
	return owner.Type() == "pair" && jsStaticObjectKey(owner, src) != ""
}

func jsObjectPropertyInStaticBlock(node *sitter.Node) bool {
	for current, depth := node.Parent(), 0; validNode(current) && depth < maxParseWalkDepth; current, depth = current.Parent(), depth+1 {
		if current.Type() == "class_static_block" {
			return true
		}
	}
	return false
}

func jsObjectLexicalScope(node *sitter.Node, src []byte, scope string) string {
	if scope != "" {
		return scope
	}
	for current, depth := node.Parent(), 0; validNode(current) && depth < maxParseWalkDepth; current, depth = current.Parent(), depth+1 {
		switch current.Type() {
		case "function_declaration", "generator_function_declaration", "method_definition":
			return nodeName(current, src)
		case "arrow_function", "function_expression", "generator_function":
			if parent := current.Parent(); validNode(parent) && parent.Type() == "variable_declarator" {
				return nodeName(parent, src)
			}
		}
	}
	return ""
}

func jsObjectCallableSpans(object SymbolRecord, symbols []SymbolRecord) jsByteSpans {
	var spans jsByteSpans
	for _, symbol := range symbols {
		if registrationBindableKind(symbol.Kind) && symbol.sourceStartByte >= object.sourceStartByte && symbol.sourceEndByte <= object.sourceEndByte && symbol.sourceEndByte > symbol.sourceStartByte {
			spans = append(spans, struct{ start, end int }{symbol.sourceStartByte, symbol.sourceEndByte})
		}
	}
	return spans
}

func jsMaskObjectCallables(block string, start int, spans jsByteSpans) string {
	if len(spans) == 0 {
		return block
	}
	masked := []byte(block)
	for _, span := range spans {
		for index := maxInt(0, span.start-start); index < minInt(len(masked), span.end-start); index++ {
			if masked[index] != '\n' {
				masked[index] = ' '
			}
		}
	}
	return string(masked)
}

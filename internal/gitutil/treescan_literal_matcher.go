package gitutil

// literalMatcher finds every pattern's required literals in one pass over a
// blob: an Aho-Corasick automaton over ASCII-folded bytes, with the alphabet
// compressed to the bytes the literals use, so a tree scan reads each byte
// once whatever the number of patterns.
type literalMatcher struct {
	classes    [256]uint8
	classCount int
	// next[state*classCount+class] is the state after reading a byte of class.
	next []int32
	// outputs[state] lists the patterns whose literals end at that state,
	// including those reached through failure links.
	outputs [][]int32
}

// newLiteralMatcher builds the automaton for requirements[pattern], each a set
// of folded ASCII strings. Patterns with no requirement take no part.
func newLiteralMatcher(requirements [][]string) *literalMatcher {
	m := &literalMatcher{}
	// Class 0 is every byte no literal uses; folding maps A-Z onto a-z first.
	used := [256]bool{}
	for _, set := range requirements {
		for _, literal := range set {
			for i := 0; i < len(literal); i++ {
				used[literal[i]] = true
			}
		}
	}
	m.classCount = 1
	for b := 0; b < 256; b++ {
		if used[b] {
			m.classes[b] = uint8(m.classCount)
			m.classCount++
		}
	}
	for b := 'A'; b <= 'Z'; b++ {
		m.classes[b] = m.classes[b+('a'-'A')]
	}

	// Trie.
	type node struct {
		children map[uint8]int32
		out      []int32
	}
	nodes := []node{{children: map[uint8]int32{}}}
	for pattern, set := range requirements {
		for _, literal := range set {
			state := int32(0)
			for i := 0; i < len(literal); i++ {
				class := m.classes[literal[i]]
				child, ok := nodes[state].children[class]
				if !ok {
					child = int32(len(nodes))
					nodes = append(nodes, node{children: map[uint8]int32{}})
					nodes[state].children[class] = child
				}
				state = child
			}
			nodes[state].out = appendPattern(nodes[state].out, int32(pattern))
		}
	}

	// Breadth-first failure links, folded into a dense transition table.
	m.next = make([]int32, len(nodes)*m.classCount)
	m.outputs = make([][]int32, len(nodes))
	fail := make([]int32, len(nodes))
	queue := make([]int32, 0, len(nodes))
	for class := 0; class < m.classCount; class++ {
		if child, ok := nodes[0].children[uint8(class)]; ok {
			m.next[class] = child
			queue = append(queue, child)
		}
	}
	m.outputs[0] = nodes[0].out
	for len(queue) > 0 {
		state := queue[0]
		queue = queue[1:]
		m.outputs[state] = nodes[state].out
		for _, p := range m.outputs[fail[state]] {
			m.outputs[state] = appendPattern(m.outputs[state], p)
		}
		for class := 0; class < m.classCount; class++ {
			child, ok := nodes[state].children[uint8(class)]
			if !ok {
				m.next[int(state)*m.classCount+class] = m.next[int(fail[state])*m.classCount+class]
				continue
			}
			fail[child] = m.next[int(fail[state])*m.classCount+class]
			m.next[int(state)*m.classCount+class] = child
			queue = append(queue, child)
		}
	}
	return m
}

func appendPattern(list []int32, pattern int32) []int32 {
	for _, p := range list {
		if p == pattern {
			return list
		}
	}
	return append(list, pattern)
}

// step advances from state over b and returns the new state with the patterns
// whose literals end there.
func (m *literalMatcher) step(state int32, b byte) (int32, []int32) {
	state = m.next[int(state)*m.classCount+int(m.classes[b])]
	return state, m.outputs[state]
}

package sem

import (
	"strings"
	"sync"
)

// jsModuleFacts is what resolving imports through JavaScript re-exports
// needs of a module: whether it can be read, its named re-exports and its
// star re-exports. Resolving one importing file's imports walks the modules it
// reaches, and the same modules (a package's index, its shared types) are
// reached from every file that imports them; each walk read and scanned them
// again. The facts are a function of a module's content alone, so the
// relation phase finds them once per module and keeps them instead of the
// content. As with the phase's string constants, a path's content is the same
// at every read within the phase.
type jsModuleFacts struct {
	read   contentReader
	mu     sync.Mutex
	byPath map[string]*jsModuleFactsEntry
}

type jsModuleFactsEntry struct {
	once sync.Once
	ok   bool
	// named is each `export {…} from "…"` in order: its module and every
	// name an item imports or binds locally.
	named []jsNamedReexport
	star  []string
}

type jsNamedReexport struct {
	module string
	names  map[string]bool
}

func newJSModuleFacts(read contentReader) *jsModuleFacts {
	return &jsModuleFacts{read: read, byPath: map[string]*jsModuleFactsEntry{}}
}

// get is path's facts, read and derived on first ask. The re-export facts
// are derived only for a module that can re-export.
func (m *jsModuleFacts) get(path string) *jsModuleFactsEntry {
	m.mu.Lock()
	entry := m.byPath[path]
	if entry == nil {
		entry = &jsModuleFactsEntry{}
		m.byPath[path] = entry
	}
	m.mu.Unlock()
	entry.once.Do(func() {
		content, ok := m.read(path)
		entry.ok = ok
		if !ok || !javaScriptReexportCandidate(path) {
			return
		}
		for _, match := range jsNamedReexportPattern.FindAllStringSubmatch(content, -1) {
			if len(match) != 3 {
				continue
			}
			reexport := jsNamedReexport{module: match[2], names: map[string]bool{}}
			for _, item := range strings.Split(stripJSLineComments(match[1]), ",") {
				imported, local := javascriptImportNames(item)
				reexport.names[imported], reexport.names[local] = true, true
			}
			entry.named = append(entry.named, reexport)
		}
		entry.star = javascriptStarReexportModules(content)
	})
	return entry
}

// namedFor is javascriptNamedReexportModulesForName of the module's content.
func (e *jsModuleFactsEntry) namedFor(name string) []string {
	var modules []string
	for _, reexport := range e.named {
		if reexport.names[name] {
			modules = append(modules, reexport.module)
		}
	}
	return uniqueStrings(modules)
}

// exists is a reader that answers only whether path can be read, for the
// import probes that ask nothing else; its content is always empty.
func (m *jsModuleFacts) exists(path string) (string, bool) {
	return "", m.get(path).ok
}

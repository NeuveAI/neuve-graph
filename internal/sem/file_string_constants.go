package sem

import (
	"sync"
	"sync/atomic"
)

// fileStringConstants derives each file's static string constants, and its
// JavaScript import bindings, once for the whole relation phase.
//
// Several passes over the same files want them. The HANDLES_ROUTE passes
// (Go, direct JavaScript and cross-file Express routes) read the constants to
// resolve a route spelled as a constant; the per-file loop reads them again
// for the same reason, and both it and the Express route pass read each
// JavaScript file's import bindings, which resolve module constants too. Each
// derived its own value from the same content, and the derivation is a regex
// scan of the whole file, so it was among the most expensive things the
// relation phase did.
//
// Both are functions of the file's content alone, and every caller passes the
// content the phase read for that path, which is the same at every read: at a
// commit it is the blob, and a working-tree file changed during the build
// leaves that build inconsistent whichever read a pass saw. Entries are derived
// outside the lock, once each, so workers asking for different files never
// wait on each other. A nil memo derives every value afresh.
type fileStringConstants struct {
	mu       sync.Mutex
	by       map[string]*fileConstantsEntry
	bindings map[string]*fileBindingsEntry
}

type fileConstantsEntry struct {
	once      sync.Once
	derived   atomic.Bool
	constants map[string]string
}

type fileBindingsEntry struct {
	once     sync.Once
	bindings map[string][]jsImportBinding
}

func newFileStringConstants() *fileStringConstants {
	return &fileStringConstants{by: map[string]*fileConstantsEntry{}, bindings: map[string]*fileBindingsEntry{}}
}

// forFile returns path's constants, deriving them from content on first ask.
//
// The returned map is shared with every other caller for the same path and must
// not be mutated. Its consumers read it to expand a constant reference.
func (c *fileStringConstants) forFile(path, content string) map[string]string {
	if c == nil {
		return staticStringConstants(content)
	}
	c.mu.Lock()
	entry := c.by[path]
	if entry == nil {
		entry = &fileConstantsEntry{}
		c.by[path] = entry
	}
	c.mu.Unlock()
	entry.once.Do(func() {
		entry.constants = staticStringConstants(content)
		entry.derived.Store(true)
	})
	return entry.constants
}

// forUnreadFile is the constants for a caller that did not read path and
// holds no content for it: those already derived from what another pass read,
// or else content's own, which are not kept, so no later pass is answered from
// text that was never the file's.
func (c *fileStringConstants) forUnreadFile(path, content string) map[string]string {
	if c != nil {
		c.mu.Lock()
		entry := c.by[path]
		c.mu.Unlock()
		if entry != nil && entry.derived.Load() {
			return entry.constants
		}
	}
	return staticStringConstants(content)
}

// importBindings returns path's JavaScript import bindings
// (importedJavaScriptBindings), deriving them from content on first ask. The
// returned map and its slices are shared and must not be mutated.
func (c *fileStringConstants) importBindings(path, content string) map[string][]jsImportBinding {
	if c == nil {
		return importedJavaScriptBindings(content)
	}
	c.mu.Lock()
	entry := c.bindings[path]
	if entry == nil {
		entry = &fileBindingsEntry{}
		c.bindings[path] = entry
	}
	c.mu.Unlock()
	entry.once.Do(func() {
		entry.bindings = importedJavaScriptBindingsWithConstants(content, c.forFile(path, content))
	})
	return entry.bindings
}

package sem

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// Resolving imports through re-exports from the relation phase's module
// facts answers what reading each module again did. The "Before" functions
// are the implementation that replaced, copied verbatim but for the names of
// the functions it calls.

func resolvedImportedNameModulesBefore(importingPath string, imports map[string][]string, manifestImports manifestImportResolver, knownFiles map[string]bool, readContent contentReader) map[string][]string {
	if len(imports) == 0 {
		return imports
	}
	readCached := cachingContentReaderBefore(readContent)
	reexportCache := map[string][]string{}
	resolved := make(map[string][]string, len(imports))
	for name, modules := range imports {
		resolved[name] = append(resolved[name], modules...)
		for _, module := range modules {
			targetPath, _ := resolveImportSpecPath(importingPath, module, manifestImports, knownFiles, readCached)
			if targetPath == "" {
				continue
			}
			resolved[name] = append(resolved[name], resolvedImportPathPrefix+targetPath)
			for _, reexport := range resolvedJavaScriptReexportTargetsBefore(targetPath, name, manifestImports, knownFiles, readCached, map[string]bool{}, reexportCache, 0) {
				resolved[name] = append(resolved[name], resolvedImportPathPrefix+reexport)
			}
		}
		resolved[name] = uniqueStrings(resolved[name])
	}
	return resolved
}

func resolvedJavaScriptReexportTargetsBefore(path, name string, manifestImports manifestImportResolver, knownFiles map[string]bool, readContent contentReader, seen map[string]bool, cache map[string][]string, depth int) []string {
	if path == "" || depth > 8 || seen[path] {
		return nil
	}
	// Only a JS/TS module can re-export (`export { x } from "./y"`), but this
	// ran for every resolved import in every language: it opened the target
	// file and scanned it for re-export syntax, recursing up to eight levels.
	// On a Rust repository that was 32% of a fast-profile snapshot spent
	// looking for JavaScript in `use` targets. Nothing downstream changes for
	// JS/TS, which still takes the same path.
	if !javaScriptReexportCandidate(path) {
		return nil
	}
	cacheKey := path + "\x00" + name
	if cached, ok := cache[cacheKey]; ok {
		return cached
	}
	seen[path] = true
	content, ok := readContent(path)
	if !ok {
		return nil
	}
	var out []string
	for _, reexport := range javascriptNamedReexportModulesForName(content, name) {
		targetPath, ok := resolveImportSpecPath(path, reexport, manifestImports, knownFiles, readContent)
		if !ok || targetPath == "" {
			continue
		}
		out = append(out, targetPath)
		out = append(out, resolvedJavaScriptReexportTargetsBefore(targetPath, name, manifestImports, knownFiles, readContent, seen, cache, depth+1)...)
	}
	out = append(out, resolvedJavaScriptStarReexportClosureBefore(path, manifestImports, knownFiles, readContent, map[string]bool{}, cache, 0)...)
	out = uniqueStrings(out)
	cache[cacheKey] = out
	return out
}

func resolvedJavaScriptStarReexportClosureBefore(path string, manifestImports manifestImportResolver, knownFiles map[string]bool, readContent contentReader, seen map[string]bool, cache map[string][]string, depth int) []string {
	if path == "" || depth > 8 || seen[path] {
		return nil
	}
	// Guarded like resolvedJavaScriptReexportTargets: today every caller is
	// already behind that gate, but this recurses into resolved targets and is
	// the other entry point, so it should not depend on the caller for it.
	if !javaScriptReexportCandidate(path) {
		return nil
	}
	cacheKey := "\x00star\x00" + path
	if cached, ok := cache[cacheKey]; ok {
		return cached
	}
	seen[path] = true
	content, ok := readContent(path)
	if !ok {
		return nil
	}
	var out []string
	for _, reexport := range javascriptStarReexportModules(content) {
		targetPath, ok := resolveImportSpecPath(path, reexport, manifestImports, knownFiles, readContent)
		if !ok || targetPath == "" {
			continue
		}
		out = append(out, targetPath)
		out = append(out, resolvedJavaScriptStarReexportClosureBefore(targetPath, manifestImports, knownFiles, readContent, seen, cache, depth+1)...)
	}
	out = uniqueStrings(out)
	cache[cacheKey] = out
	return out
}

func cachingContentReaderBefore(readContent contentReader) contentReader {
	type cached struct {
		content string
		ok      bool
	}
	cache := map[string]cached{}
	return func(path string) (string, bool) {
		if hit, ok := cache[path]; ok {
			return hit.content, hit.ok
		}
		content, ok := readContent(path)
		cache[path] = cached{content: content, ok: ok}
		return content, ok
	}
}

// A generated TypeScript workspace: modules that re-export each other by
// name, under aliases, by star, in chains and in cycles, some missing, some
// with comments inside the export list.
func reexportWorkspace(rng *rand.Rand) (map[string]string, []string) {
	paths := []string{"src/index.ts", "src/a.ts", "src/b.ts", "src/c/index.ts", "src/c/d.tsx", "src/e.js", "lib/f.ts", "lib/g/index.js"}
	names := []string{"alpha", "beta", "gamma", "delta", "Widget", "default"}
	specs := []string{"./a", "./b", "./c", "./c/d", "./e", "../lib/f", "../lib/g", "./missing", "./a.js", "../src/index"}
	files := map[string]string{}
	for _, path := range paths {
		var b strings.Builder
		for range 1 + rng.Intn(5) {
			spec := specs[rng.Intn(len(specs))]
			switch rng.Intn(5) {
			case 0:
				fmt.Fprintf(&b, "export * from '%s';\n", spec)
			case 1:
				fmt.Fprintf(&b, "export { %s as %s, %s } from \"%s\";\n", names[rng.Intn(len(names))], names[rng.Intn(len(names))], names[rng.Intn(len(names))], spec)
			case 2:
				fmt.Fprintf(&b, "export type {\n  %s, // a comment\n  %s\n} from '%s'\n", names[rng.Intn(len(names))], names[rng.Intn(len(names))], spec)
			case 3:
				fmt.Fprintf(&b, "export function %s() {}\n", names[rng.Intn(len(names))])
			default:
				fmt.Fprintf(&b, "import { %s } from '%s';\n", names[rng.Intn(len(names))], spec)
			}
		}
		if rng.Intn(6) != 0 {
			files[path] = b.String()
		}
	}
	return files, paths
}

func TestReexportResolutionFromModuleFactsIsTheReads(t *testing.T) {
	rng := rand.New(rand.NewSource(23))
	names := []string{"alpha", "beta", "gamma", "delta", "Widget", "default", "absent"}
	specs := []string{"./a", "./b", "./c", "./c/d", "./e", "../lib/f", "../lib/g", "./missing", "../src/index"}
	resolved := 0
	for round := 0; round < 300; round++ {
		files, paths := reexportWorkspace(rng)
		reads := 0
		read := func(path string) (string, bool) {
			reads++
			content, ok := files[path]
			return content, ok
		}
		known := map[string]bool{}
		for path := range files {
			if rng.Intn(3) != 0 {
				known[path] = true
			}
		}
		// One phase's facts across every importing file, as the relation
		// phase holds them.
		phase := newJSModuleFacts(read)
		for _, importing := range paths {
			imports := map[string][]string{}
			for range 1 + rng.Intn(4) {
				name := names[rng.Intn(len(names))]
				imports[name] = append(imports[name], specs[rng.Intn(len(specs))])
			}
			want := resolvedImportedNameModulesBefore(importing, imports, manifestImportResolver{}, known, read)
			got := resolvedImportedNameModules(importing, imports, manifestImportResolver{}, known, read, phase)
			alone := resolvedImportedNameModules(importing, imports, manifestImportResolver{}, known, read, nil)
			if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(alone, want) {
				t.Fatalf("round %d, %s importing %v over %v:\n got %v\nalone %v\nwant %v", round, importing, imports, files, got, alone, want)
			}
			for _, modules := range want {
				for _, module := range modules {
					if strings.HasPrefix(module, resolvedImportPathPrefix) {
						resolved++
					}
				}
			}
		}
		_ = reads
	}
	if resolved == 0 {
		t.Fatal("no import resolved to a file")
	}
}

package sem

import (
	"reflect"
	"strings"
	"testing"
)

// markerLine returns the 1-based line of content that holds marker, failing
// the test when there is not exactly one.
func markerLine(t *testing.T, content, marker string) int {
	t.Helper()
	line, found := 0, 0
	for i, text := range strings.Split(content, "\n") {
		if strings.Contains(text, marker) {
			line = i + 1
			found++
		}
	}
	if found != 1 {
		t.Fatalf("marker %q found %d times", marker, found)
	}
	return line
}

// callEntry returns the one call-site evidence entry of the relation of
// relationType from the symbol fromName in fromFile (or the file itself when
// fromName is "") to a target whose ID ends with toSuffix. It fails when the
// relation does not exist, so no assertion below can pass by its absence.
func callEntry(t *testing.T, snapshot ProviderSnapshot, relationType, fromFile, fromName, toSuffix string) (RelationRecord, Evidence) {
	t.Helper()
	symbols := map[string]SymbolRecord{}
	for _, symbol := range snapshot.Symbols {
		symbols[symbol.ID] = symbol
	}
	fileIDs := map[string]string{}
	for _, file := range snapshot.Files {
		fileIDs[file.Path] = file.ID
	}
	var found []RelationRecord
	for _, relation := range snapshot.Relations {
		if relation.Type != relationType || !strings.HasSuffix(relation.ToID, toSuffix) {
			continue
		}
		if fromName == "" {
			if relation.FromID == fileIDs[fromFile] {
				found = append(found, relation)
			}
			continue
		}
		if from, ok := symbols[relation.FromID]; ok && from.Name == fromName && from.FilePath == fromFile {
			found = append(found, relation)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s %s:%s -> *%s: %d relations, want 1", relationType, fromFile, fromName, toSuffix, len(found))
	}
	if len(found[0].Evidence) != 1 {
		t.Fatalf("%s %s:%s -> *%s: %d evidence entries, want 1", relationType, fromFile, fromName, toSuffix, len(found[0].Evidence))
	}
	return found[0], found[0].Evidence[0]
}

// assertCallLine checks that the entry says a call is written on want, one
// line, and is not marked as a reference.
func assertCallLine(t *testing.T, snapshot ProviderSnapshot, relationType, fromFile, fromName, toSuffix, kind string, want int) {
	t.Helper()
	assertCallForm(t, snapshot, relationType, fromFile, fromName, toSuffix, kind, want, "")
}

// assertCallReference checks that the entry says the target's name is handed
// over by name on want, and not called.
func assertCallReference(t *testing.T, snapshot ProviderSnapshot, relationType, fromFile, fromName, toSuffix, kind string, want int) {
	t.Helper()
	assertCallForm(t, snapshot, relationType, fromFile, fromName, toSuffix, kind, want, evidenceFormReference)
}

func assertCallForm(t *testing.T, snapshot ProviderSnapshot, relationType, fromFile, fromName, toSuffix, kind string, want int, form string) {
	t.Helper()
	_, evidence := callEntry(t, snapshot, relationType, fromFile, fromName, toSuffix)
	if evidence.Form != form {
		t.Errorf("%s %s:%s -> *%s: form %q, want %q", relationType, fromFile, fromName, toSuffix, evidence.Form, form)
	}
	if evidence.Kind != kind {
		t.Errorf("%s %s:%s -> *%s: kind %q, want %q", relationType, fromFile, fromName, toSuffix, evidence.Kind, kind)
	}
	if evidence.StartLine != want || evidence.EndLine != want {
		t.Errorf("%s %s:%s -> *%s: lines %d-%d, want %d", relationType, fromFile, fromName, toSuffix, evidence.StartLine, evidence.EndLine, want)
	}
}

// assertCallLinesInsideCallers is the property every snapshot of a declared
// language must hold: a call entry's line is one line, inside a symbol
// caller's span, and on that line the callee's name is written. An entry the
// engine cannot place has no line at all. It returns how many call entries it
// checked, so a caller can tell the property was not vacuous.
func assertCallLinesInsideCallers(t *testing.T, snapshot ProviderSnapshot, sources map[string]string) int {
	t.Helper()
	symbols := map[string]SymbolRecord{}
	for _, symbol := range snapshot.Symbols {
		symbols[symbol.ID] = symbol
	}
	checked := 0
	for _, relation := range snapshot.Relations {
		if relation.Type != "CALLS" && relation.Type != "CONSTRUCTS" && relation.Type != "ASYNC_CALLS" {
			continue
		}
		for _, evidence := range relation.Evidence {
			if !callSiteEvidenceKind(evidence.Kind) {
				continue
			}
			from, symbolCaller := symbols[relation.FromID]
			language := from.Language
			if !symbolCaller {
				for _, file := range snapshot.Files {
					if file.ID == relation.FromID {
						language = file.Language
					}
				}
			}
			if !callSiteLineLanguageSet[language] {
				continue
			}
			checked++
			if evidence.StartLine == 0 && evidence.EndLine == 0 {
				continue
			}
			if evidence.StartLine != evidence.EndLine {
				t.Errorf("%s %s -> %s: lines %d-%d, want one line", relation.Type, relation.FromID, relation.ToID, evidence.StartLine, evidence.EndLine)
			}
			if symbolCaller && (evidence.StartLine < from.StartLine || evidence.StartLine > from.EndLine) {
				t.Errorf("%s %s -> %s: line %d outside caller %d-%d", relation.Type, relation.FromID, relation.ToID, evidence.StartLine, from.StartLine, from.EndLine)
			}
			source, ok := sources[evidence.FilePath]
			if !ok {
				continue
			}
			lines := strings.Split(source, "\n")
			if evidence.StartLine > len(lines) {
				t.Errorf("%s %s -> %s: line %d past the end of %s", relation.Type, relation.FromID, relation.ToID, evidence.StartLine, evidence.FilePath)
				continue
			}
			name := evidence.Detail
			if i := strings.LastIndexAny(name, ".>"); i >= 0 {
				name = name[i+1:]
			}
			if !strings.Contains(lines[evidence.StartLine-1], name) {
				t.Errorf("%s %s -> %s: line %d %q does not write %q", relation.Type, relation.FromID, relation.ToID, evidence.StartLine, lines[evidence.StartLine-1], name)
			}
		}
	}
	return checked
}

func buildCallLineFixture(t *testing.T, files map[string]string) ProviderSnapshot {
	t.Helper()
	repo := t.TempDir()
	for path, content := range files {
		writeFile(t, repo, path, content)
	}
	snapshot, err := BuildProviderSnapshot(t.Context(), repo, "test-version")
	if err != nil {
		t.Fatal(err)
	}
	if checked := assertCallLinesInsideCallers(t, snapshot, files); checked == 0 {
		t.Fatal("the fixture produced no call entry in a declared language")
	}
	return snapshot
}

const callLinesTypeScriptLib = `export function imported(): void {}
`

const callLinesTypeScript = `import { imported } from "./lib";
import { readFileSync } from "fs";

export class Store {
  save(): void {}
}

export function pick<T>(value: T): T {
  return value;
}

export function generic(): number {
  const b = 1;
  return pick<number>(b); // generic-call
}

export function double(value: number): number {
  return value * 2;
}

export function passes(xs: number[]): number[] {
  const k = 1;
  return xs.map(double).map((v) => v + k); // passed-by-name
}

export function both(xs: number[]): number {
  const m = xs.map(double); // passed-first
  return double(m.length); // called-later
}

export function helper(): number {
  return 1;
}

export async function work(): Promise<number> {
  return 1;
}

export function caller(store: Store): number {
  const x = 1;
  if (x > 0) {
    helper(); // nested-first
  }
  helper(); // second-call
  store.save(); // receiver-call
  imported(); // imported-call
  readFileSync( // external-call
    "x",
  );
  const s = new Store(); // construction
  return x + (s ? 0 : 1);
}

export function spans(store: Store): void {
  const y = 2;
  store
    .save(); // spanning-name
}

export async function asyncCaller(): Promise<void> {
  const y = 2;
  await work(); // async-call
}
`

const callLinesTypeScriptTest = `import { helper } from "./a";

describe("suite", () => {
  it("works", () => {
    const z = 1;
    helper(); // callback-call
    helper();
  });
});
`

const callLinesTypeScriptHandler = `import { double } from "./a";

const doubled = [1, 2].map(double); // file-level-by-name
`

// The pinned engine's text scan reads no identifier argument after a string
// literal one, and does not look inside the arguments of a call it has read,
// so neither of these gives a relation: there is no entry to mark.
const callLinesTypeScriptUnseenHandler = `import { double } from "./a";

test("doubles", double);
describe("suite", () => {
  it("case", () => [1].map(double));
});
`

const callLinesTypeScriptTopLevel = `import { helper, Store } from "./a";

const first = 1;
helper(); // top-level-call
new Store(); // top-level-construction
`

// Every TypeScript call shape the engine resolves says the line of the call,
// never the caller's first line. Reason: the defect was the caller's span in
// place of the call, and a call several lines into its caller is where the two
// differ.
func TestCallSiteLinesTypeScript(t *testing.T) {
	files := map[string]string{
		"lib.ts": callLinesTypeScriptLib, "a.ts": callLinesTypeScript,
		"a.test.ts": callLinesTypeScriptTest, "top.ts": callLinesTypeScriptTopLevel,
		"handler.test.ts": callLinesTypeScriptHandler, "unseen.test.ts": callLinesTypeScriptUnseenHandler,
	}
	snapshot := buildCallLineFixture(t, files)
	src := callLinesTypeScript
	assertCallLine(t, snapshot, "CALLS", "a.ts", "caller", ":helper", "call_site", markerLine(t, src, "nested-first"))
	assertCallLine(t, snapshot, "CALLS", "a.ts", "caller", ":Store.save", "call_site", markerLine(t, src, "receiver-call"))
	assertCallLine(t, snapshot, "CALLS", "a.ts", "caller", ":imported", "call_site", markerLine(t, src, "imported-call"))
	assertCallLine(t, snapshot, "CALLS", "a.ts", "caller", "readFileSync", "imported_call_site", markerLine(t, src, "external-call"))
	assertCallLine(t, snapshot, "CONSTRUCTS", "a.ts", "caller", ":Store", "call_site", markerLine(t, src, "construction"))
	assertCallLine(t, snapshot, "CALLS", "a.ts", "spans", ":Store.save", "call_site", markerLine(t, src, "spanning-name"))
	assertCallLine(t, snapshot, "ASYNC_CALLS", "a.ts", "asyncCaller", ":work", "async_call_site", markerLine(t, src, "async-call"))
	assertCallLine(t, snapshot, "CALLS", "a.ts", "generic", ":pick", "call_site", markerLine(t, src, "generic-call"))
	// A function handed over by name is a reference on the line it is passed;
	// a caller that passes and calls it reports the call.
	assertCallReference(t, snapshot, "CALLS", "a.ts", "passes", ":double", "call_site", markerLine(t, src, "passed-by-name"))
	assertCallLine(t, snapshot, "CALLS", "a.ts", "both", ":double", "call_site", markerLine(t, src, "called-later"))
	assertCallReference(t, snapshot, "CALLS", "handler.test.ts", "", ":double", "top_level_call_site", markerLine(t, callLinesTypeScriptHandler, "file-level-by-name"))
	for _, relation := range snapshot.Relations {
		if strings.Contains(relation.FromID, "unseen.test.ts") && strings.HasSuffix(relation.ToID, ":double") && relation.Type == "CALLS" {
			t.Errorf("a handler passed after a string argument now gives a relation: %+v", relation)
		}
	}
	assertCallLine(t, snapshot, "CALLS", "a.test.ts", "", ":helper", "top_level_call_site", markerLine(t, callLinesTypeScriptTest, "callback-call"))
	assertCallLine(t, snapshot, "CALLS", "top.ts", "", ":helper", "top_level_call_site", markerLine(t, callLinesTypeScriptTopLevel, "top-level-call"))
	assertCallLine(t, snapshot, "CONSTRUCTS", "top.ts", "", ":Store", "top_level_call_site", markerLine(t, callLinesTypeScriptTopLevel, "top-level-construction"))
}

const callLinesJavaScript = `import { imported } from "./lib.js";

export class Ledger {
  add() {}
}

export function helper() {
  return 1;
}

export function caller() {
  const x = 1;
  for (const y of [x]) {
    helper(y); // nested-first
  }
  helper();
  const ledger = new Ledger(); // construction
  ledger.add(); // receiver-call
  imported(); // imported-call
  [x].map(helper);
  return x;
}

export function passer(items) {
  const n = 1;
  return items.filter(helper).length + n; // passed-by-name
}

test("helper", () => {
  const z = 1;
  helper(); // callback-call
});
`

// JavaScript takes the same path as TypeScript; a test callback's call is the
// file's, at the call's own line. Reason: test rows are the ones Neuve wants
// to count as evidence, and every one of them is a file-level call.
func TestCallSiteLinesJavaScript(t *testing.T) {
	files := map[string]string{"lib.js": "export function imported() {}\n", "a.js": callLinesJavaScript}
	snapshot := buildCallLineFixture(t, files)
	src := callLinesJavaScript
	assertCallLine(t, snapshot, "CALLS", "a.js", "caller", ":helper", "call_site", markerLine(t, src, "nested-first"))
	assertCallLine(t, snapshot, "CONSTRUCTS", "a.js", "caller", ":Ledger", "call_site", markerLine(t, src, "construction"))
	assertCallLine(t, snapshot, "CALLS", "a.js", "caller", ":Ledger.add", "call_site", markerLine(t, src, "receiver-call"))
	assertCallLine(t, snapshot, "CALLS", "a.js", "caller", ":imported", "call_site", markerLine(t, src, "imported-call"))
	assertCallLine(t, snapshot, "CALLS", "a.js", "", ":helper", "top_level_call_site", markerLine(t, src, "callback-call"))
	assertCallReference(t, snapshot, "CALLS", "a.js", "passer", ":helper", "call_site", markerLine(t, src, "passed-by-name"))
	// caller passes helper on one line and calls it on two others: the entry is the first call.
}

const callLinesPython = `import json
from lib import imported


class Store:
    def save(self):
        return 1


def helper():
    return 1


async def work():
    return 1


def caller():
    x = 1
    if x:
        helper()  # nested-first
    helper()
    store = Store()  # construction
    store.save()  # receiver-call
    imported()  # imported-call
    json.loads(  # external-call
        "1",
    )
    return x


async def async_caller():
    y = 2
    await work()  # async-call


if __name__ == "__main__":
    helper()  # main-call
`

// Python: calls in a function several lines down, a construction, a receiver
// call by inferred type, imported and external calls, an async call, and the
// call under ` + "`if __name__ == \"__main__\":`" + `, which is the file's.
// Reason: the four required languages each have their own scanners.
func TestCallSiteLinesPython(t *testing.T) {
	files := map[string]string{"lib.py": "def imported():\n    return 1\n", "app.py": callLinesPython}
	snapshot := buildCallLineFixture(t, files)
	src := callLinesPython
	assertCallLine(t, snapshot, "CALLS", "app.py", "caller", ":helper", "call_site", markerLine(t, src, "nested-first"))
	assertCallLine(t, snapshot, "CONSTRUCTS", "app.py", "caller", ":Store", "call_site", markerLine(t, src, "construction"))
	assertCallLine(t, snapshot, "CALLS", "app.py", "caller", ":Store.save", "call_site", markerLine(t, src, "receiver-call"))
	assertCallLine(t, snapshot, "CALLS", "app.py", "caller", ":imported", "call_site", markerLine(t, src, "imported-call"))
	assertCallLine(t, snapshot, "CALLS", "app.py", "caller", "json.loads", "imported_call_site", markerLine(t, src, "external-call"))
	assertCallLine(t, snapshot, "ASYNC_CALLS", "app.py", "async_caller", ":work", "async_call_site", markerLine(t, src, "async-call"))
	assertCallLine(t, snapshot, "CALLS", "app.py", "", ":helper", "top_level_call_site", markerLine(t, src, "main-call"))
}

const callLinesGo = `package main

import (
	"fmt"

	"example.com/m/lib"
)

type Store struct{}

type Count int

func (s *Store) Save() int { return 1 }

func helper() int { return 1 }

func worker() {}

func caller(store *Store) int {
	x := 1
	if x > 0 {
		helper() // nested-first
	}
	helper()
	store.Save() // receiver-call
	lib.Imported() // imported-call
	fmt.Println( // external-call
		"x",
	)
	go worker() // async-call
	n := Count(x) // construction
	return int(n)
}

func main() {
	caller(&Store{})
}
`

// Go: a call several lines down, a method call by receiver type, a call into
// an in-module package, an external call spanning lines, a goroutine and a
// conversion. Reason: Go reaches its calls through the receiver scanners more
// than any other language.
func TestCallSiteLinesGo(t *testing.T) {
	files := map[string]string{
		"go.mod":     "module example.com/m\n\ngo 1.22\n",
		"lib/lib.go": "package lib\n\nfunc Imported() {}\n",
		"main.go":    callLinesGo,
	}
	snapshot := buildCallLineFixture(t, files)
	src := callLinesGo
	assertCallLine(t, snapshot, "CALLS", "main.go", "caller", ":helper", "call_site", markerLine(t, src, "nested-first"))
	assertCallLine(t, snapshot, "CALLS", "main.go", "caller", ":Store.Save", "call_site", markerLine(t, src, "receiver-call"))
	assertCallLine(t, snapshot, "CALLS", "main.go", "caller", ":Imported", "call_site", markerLine(t, src, "imported-call"))
	assertCallLine(t, snapshot, "CALLS", "main.go", "caller", "fmt.Println", "imported_call_site", markerLine(t, src, "external-call"))
	assertCallLine(t, snapshot, "ASYNC_CALLS", "main.go", "caller", ":worker", "async_call_site", markerLine(t, src, "async-call"))
	assertCallLine(t, snapshot, "CONSTRUCTS", "main.go", "caller", ":Count", "call_site", markerLine(t, src, "construction"))
}

// A name the text scan finds in something that is not a call gets no line,
// never the caller's span. Here the TypeScript text scan reads an interface
// method signature inside a function as a call to the same-named function;
// the syntax tree shows no call there. Reason: point 3 of the brief -- an
// unknown position is no line.
func TestCallSiteLinesUnknownPositionHasNoLine(t *testing.T) {
	src := `export function helper(): number {
  return 1;
}

export function caller(): number {
  interface Shape {
    helper(): number;
  }
  const value = 1;
  return value;
}
`
	snapshot := buildCallLineFixture(t, map[string]string{"a.ts": src})
	_, evidence := callEntry(t, snapshot, "CALLS", "a.ts", "caller", ":helper")
	if evidence.StartLine != 0 || evidence.EndLine != 0 {
		t.Fatalf("a signature read as a call got lines %d-%d, want none", evidence.StartLine, evidence.EndLine)
	}
}

// A language outside the declared set keeps the earlier meaning: the caller's
// span. Reason: a consumer treats such a language as giving no call line, and
// nothing else about it may move.
func TestCallSiteLinesUndeclaredLanguageKeepsTheSpan(t *testing.T) {
	src := "public class Ledger {\n    void helper() {}\n\n    void caller() {\n        int x = 1;\n        helper();\n    }\n}\n"
	snapshot := buildCallLineFixture(t, map[string]string{
		"Ledger.java": src,
		"a.ts":        "export function helper(): number {\n  return 1;\n}\nexport function caller(): number {\n  return helper();\n}\n",
	})
	relation, evidence := callEntry(t, snapshot, "CALLS", "Ledger.java", "caller", ":Ledger.helper")
	var from SymbolRecord
	for _, symbol := range snapshot.Symbols {
		if symbol.ID == relation.FromID {
			from = symbol
		}
	}
	if evidence.StartLine != from.StartLine || evidence.EndLine != from.EndLine {
		t.Fatalf("Java call entry lines %d-%d, want the caller's span %d-%d", evidence.StartLine, evidence.EndLine, from.StartLine, from.EndLine)
	}
}

// The capability names exactly the languages whose emit paths were made to
// say where the call is. Reason: a consumer trusts a line only for a listed
// language, so the list is the promise.
func TestCallSiteLinesCapability(t *testing.T) {
	report := Capabilities()
	if !report.OptionalLocalOnlyFeatures["call_site_lines"] {
		t.Fatal("capabilities omit call_site_lines")
	}
	if got := report.FeatureLanguages["call_site_lines"]; !reflect.DeepEqual(got, []string{"Go", "JavaScript", "Python", "TypeScript"}) {
		t.Fatalf("call_site_lines languages %v", got)
	}
}

// In Python and Go no scanner turns a name handed over as a value into a call
// relation, so an unmarked entry in those languages is certainly a call: a
// function passed by name, assigned, returned, held in a literal or taken as a
// method value gives no relation at all. Reason: capability call_site_lines
// promises it for them; this pins the shapes the promise rests on.
func TestCallSiteLinesNoReferenceRelationsInPythonAndGo(t *testing.T) {
	snapshot := buildCallLineFixture(t, map[string]string{
		"app.py": `def double(x):
    return x * 2


class Box:
    def run(self):
        return 1


def passes(xs, box):
    mapped = list(map(double, xs))
    handler = double
    table = {"d": double}
    method = box.run
    return mapped, handler, table, method, [double]


def caller():
    return double(1)
`,
		"main.go": `package main

type Box struct{}

func (b Box) Run() int { return 1 }

func double(x int) int { return x * 2 }

func apply(f func(int) int, x int) int { return f(x) }

func passes(b Box) []any {
	h := double
	m := b.Run
	return []any{apply(double, 1), h, m, map[string]any{"d": double}}
}

func main() {
	_ = double(1)
}
`,
	})
	symbols := map[string]SymbolRecord{}
	for _, symbol := range snapshot.Symbols {
		symbols[symbol.ID] = symbol
	}
	for _, relation := range snapshot.Relations {
		from := symbols[relation.FromID]
		if from.Name != "passes" || (relation.Type != "CALLS" && relation.Type != "CONSTRUCTS") {
			continue
		}
		if strings.HasSuffix(relation.ToID, ":double") || strings.HasSuffix(relation.ToID, ".Run") || strings.HasSuffix(relation.ToID, ".run") {
			t.Errorf("%s: a value handed over gave a call relation %s -> %s (%+v)", from.Language, relation.FromID, relation.ToID, relation.Evidence)
		}
	}
	// The real calls are there, unmarked.
	assertCallLine(t, snapshot, "CALLS", "app.py", "caller", ":double", "call_site", 19)
	assertCallLine(t, snapshot, "CALLS", "main.go", "main", ":double", "call_site", 18)
	assertCallLine(t, snapshot, "CALLS", "main.go", "passes", ":apply", "call_site", 14)
}

// A Python name whose `(` opens the next statement is not a call: the text
// scan still finds it (the relation is unchanged) but the entry gets no line.
// Reason: found on the Neuve head, a pytest fixture parameter followed by a
// parenthesised expression on a later line read as a call to the fixture.
func TestCallSiteLinesPythonLineBreakIsNotACall(t *testing.T) {
	src := `def fixture():
    return 1, 2


def test_case(fixture):
    first, second = fixture
    # a comment between
    (first + second).bit_length()
`
	snapshot := buildCallLineFixture(t, map[string]string{"test_app.py": src})
	_, evidence := callEntry(t, snapshot, "CALLS", "test_app.py", "test_case", ":fixture")
	if evidence.StartLine != 0 || evidence.EndLine != 0 || evidence.Form != "" {
		t.Fatalf("a name followed by a parenthesised next statement got %+v, want no line", evidence)
	}
}

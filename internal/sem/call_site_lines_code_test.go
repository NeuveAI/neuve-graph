package sem

import (
	"strings"
	"testing"
)

// assertCallNoLine checks that the relation exists and its entry has no line:
// every match of the spelling in the caller is text that is not code.
func assertCallNoLine(t *testing.T, snapshot ProviderSnapshot, relationType, fromFile, fromName, toSuffix string) {
	t.Helper()
	_, evidence := callEntry(t, snapshot, relationType, fromFile, fromName, toSuffix)
	if evidence.StartLine != 0 || evidence.EndLine != 0 || evidence.Form != "" {
		t.Errorf("%s %s:%s -> *%s: %+v, want no line (only text that is not code writes it)", relationType, fromFile, fromName, toSuffix, evidence)
	}
}

// assertNoCallRelation checks that no relation of relationType goes from the
// symbol fromName in fromFile (the file itself when fromName is "") to a
// target whose ID ends with toSuffix.
func assertNoCallRelation(t *testing.T, snapshot ProviderSnapshot, relationType, fromFile, fromName, toSuffix string) {
	t.Helper()
	symbols := map[string]SymbolRecord{}
	for _, symbol := range snapshot.Symbols {
		symbols[symbol.ID] = symbol
	}
	fileIDs := map[string]string{}
	for _, file := range snapshot.Files {
		fileIDs[file.Path] = file.ID
	}
	for _, relation := range snapshot.Relations {
		if relation.Type != relationType || !strings.HasSuffix(relation.ToID, toSuffix) {
			continue
		}
		from, ok := symbols[relation.FromID]
		if (fromName == "" && relation.FromID == fileIDs[fromFile]) || (ok && from.Name == fromName && from.FilePath == fromFile) {
			t.Errorf("%s %s:%s -> *%s: a relation %+v, want none", relationType, fromFile, fromName, toSuffix, relation.Evidence)
		}
	}
}

const callLinesCodePython = `import os


class Store:
    def save(self):
        return 1

    def refresh(self):
        return 1

    def after_comment(self):
        # self.refresh() and os.getcwd() and Store().save() are mentioned here
        x = 1
        self.refresh()  # receiver-after-comment
        Store().save()  # chain-after-comment
        return os.getcwd()  # imported-after-comment

    def after_docstring(self):
        """Explain the method.

        self.refresh() is called, then os.getcwd() as well.
        """
        x = "self.refresh() in a string"
        y = '''os.getcwd() in a
        multi-line string, self.refresh()'''
        self.refresh()  # receiver-after-docstring
        return os.getcwd()  # imported-after-docstring

    def only_text(self):
        """Mentions self.refresh() and never calls it.

        os.getcwd() neither.
        """
        # self.refresh()
        return 1

    def first_of_several(self):
        y = 1
        self.refresh()  # receiver-first
        self.refresh()
        return y


def helper():
    return 1


async def work():
    return 1


def bare_after_text():
    """helper() in a docstring.

    helper() again.
    """
    # helper() in a comment
    return helper()  # bare-after-text


async def awaits_after_text():
    """Docstring: await work() here.

    await work() again.
    """
    # await work()
    s = "await work()"
    await work()  # async-after-text
    await work()


async def async_only_text():
    """Nothing is awaited.

    await work() is only written here.
    """
    return 1


# helper() at file level, in a comment
"""helper() in a module string.

helper() again.
"""
if __name__ == "__main__":
    helper()  # top-level-after-text
`

// Python: a receiver call, a constructor chain, a call through an imported
// module, an awaited call, a bare call and a file-level call, each written
// first in a # comment, a one-line string, a docstring or a multi-line
// string, then really called lower: the entry is the lower line. Written only
// in such text, the relation keeps no line. A first of several real calls is
// the first. Reason: the receiver and awaited scans read the generic mask,
// which knows no # comment and gives up a string at a line break, and placed
// these entries on the comment or docstring (B1).
func TestCallSiteLinesPythonTextIsNotACall(t *testing.T) {
	snapshot := buildCallLineFixture(t, map[string]string{"app.py": callLinesCodePython})
	src := callLinesCodePython
	assertCallLine(t, snapshot, "CALLS", "app.py", "after_comment", ":Store.refresh", "call_site", markerLine(t, src, "receiver-after-comment"))
	assertCallLine(t, snapshot, "CALLS", "app.py", "after_comment", ":Store.save", "call_site", markerLine(t, src, "chain-after-comment"))
	assertCallLine(t, snapshot, "CALLS", "app.py", "after_comment", "os.getcwd", "imported_call_site", markerLine(t, src, "imported-after-comment"))
	assertCallLine(t, snapshot, "CALLS", "app.py", "after_docstring", ":Store.refresh", "call_site", markerLine(t, src, "receiver-after-docstring"))
	assertCallLine(t, snapshot, "CALLS", "app.py", "after_docstring", "os.getcwd", "imported_call_site", markerLine(t, src, "imported-after-docstring"))
	assertCallNoLine(t, snapshot, "CALLS", "app.py", "only_text", ":Store.refresh")
	assertCallNoLine(t, snapshot, "CALLS", "app.py", "only_text", "os.getcwd")
	assertCallLine(t, snapshot, "CALLS", "app.py", "first_of_several", ":Store.refresh", "call_site", markerLine(t, src, "receiver-first"))
	assertCallLine(t, snapshot, "CALLS", "app.py", "bare_after_text", ":helper", "call_site", markerLine(t, src, "bare-after-text"))
	assertCallLine(t, snapshot, "ASYNC_CALLS", "app.py", "awaits_after_text", ":work", "async_call_site", markerLine(t, src, "async-after-text"))
	assertCallNoLine(t, snapshot, "ASYNC_CALLS", "app.py", "async_only_text", ":work")
	assertCallLine(t, snapshot, "CALLS", "app.py", "", ":helper", "top_level_call_site", markerLine(t, src, "top-level-after-text"))
}

const callLinesCodeTypeScript = `import { readFileSync } from "fs";

export class Store {
  save(): number {
    return 1;
  }
}

export function helper(): number {
  return 1;
}

export async function work(): Promise<number> {
  return 1;
}

export function receiverAfterText(store: Store): unknown {
  const re = /store.save() new Store().save() readFileSync()/;
  // store.save() in a comment
  const s = "store.save()";
  const view = <p>store.save() and new Store().save() and helper() and readFileSync() as text</p>;
  store.save(); // receiver-after-text
  store.save();
  new Store().save();
  helper(); // bare-after-text
  readFileSync("x"); // imported-after-text
  return [re, s, view];
}

export function chainAfterText(): unknown {
  const re = /new Store().save()/;
  // new Store().save() in a comment
  const s = "new Store().save()";
  const view = <p>new Store().save() as text</p>;
  new Store().save(); // chain-after-text
  return [re, s, view];
}

export async function afterNestedTemplate(store: Store, names: string[]): Promise<string> {
  const text = ` + "`${names.length}${\n    names.length ? `, including ${names.join(\", \")}'s,` : \"\"\n  } and more.`" + `;
  const after = ` + "`then store.save() and await work() in text`" + `;
  await work(); // async-after-template
  store.save(); // receiver-after-template
  return text + after;
}

export async function awaitsAfterText(): Promise<unknown> {
  const re = /await work()/;
  const view = <p>await work()</p>;
  await work(); // async-after-text
  await work();
  return [re, view];
}

export function callInSubstitution(store: Store): string {
  const text = ` + "`saved ${store.save()} times`" + `; // receiver-in-substitution
  store.save();
  return text;
}

export async function onlyText(store: Store): Promise<unknown> {
  const re = /store.save()/;
  return <p>store.save() and await work() as text</p>;
}
`

const callLinesCodeTypeScriptTopLevel = `import { helper } from "./a";

// helper() in a comment
const pattern = /helper()/;
const text = ` + "`helper() in text`" + `;
helper(); // top-level-after-text
`

// A call with type arguments the grammar reads as two comparisons is placed
// from the text scan; written first in a regular expression and a string, the
// text scan over the code view still finds the real call.
const callLinesCodeTypeScriptGeneric = `export function pick<T>(value: T): T {
  return value;
}

export function genericAfterText(): number {
  const re = /pick<number>(1)/;
  const s = "pick<number>(2)";
  const b = 1;
  return pick<number>(b) + (re && s ? 0 : 1); // generic-after-text
}
`

// TypeScript and TSX: the receiver, constructor-chain, bare, imported and
// awaited calls, each written first in a regular expression, a comment, a
// string, JSX text or template text, then really called lower: the entry is
// the lower line. The template shape is the one at Neuve's cli.ts: a template
// holding a nested template with an apostrophe, after which the generic mask
// reads text as code. A call inside a template's ${} is code and is the first
// call. Written only in such text, the relation keeps no line. Reason: only
// the bare-name pass asked the syntax tree; the other scans placed these
// entries on the text (B2).
func TestCallSiteLinesTypeScriptTextIsNotACall(t *testing.T) {
	snapshot := buildCallLineFixture(t, map[string]string{"a.tsx": callLinesCodeTypeScript, "top.ts": callLinesCodeTypeScriptTopLevel, "generic.ts": callLinesCodeTypeScriptGeneric})
	src := callLinesCodeTypeScript
	assertCallLine(t, snapshot, "CALLS", "a.tsx", "receiverAfterText", ":Store.save", "call_site", markerLine(t, src, "receiver-after-text"))
	assertCallLine(t, snapshot, "CALLS", "a.tsx", "receiverAfterText", ":helper", "call_site", markerLine(t, src, "bare-after-text"))
	assertCallLine(t, snapshot, "CALLS", "a.tsx", "receiverAfterText", "readFileSync", "imported_call_site", markerLine(t, src, "imported-after-text"))
	assertCallLine(t, snapshot, "CALLS", "a.tsx", "afterNestedTemplate", ":Store.save", "call_site", markerLine(t, src, "receiver-after-template"))
	assertCallLine(t, snapshot, "ASYNC_CALLS", "a.tsx", "afterNestedTemplate", ":work", "async_call_site", markerLine(t, src, "async-after-template"))
	assertCallLine(t, snapshot, "ASYNC_CALLS", "a.tsx", "awaitsAfterText", ":work", "async_call_site", markerLine(t, src, "async-after-text"))
	assertCallLine(t, snapshot, "CALLS", "a.tsx", "callInSubstitution", ":Store.save", "call_site", markerLine(t, src, "receiver-in-substitution"))
	assertCallNoLine(t, snapshot, "CALLS", "a.tsx", "onlyText", ":Store.save")
	assertCallNoLine(t, snapshot, "ASYNC_CALLS", "a.tsx", "onlyText", ":work")
	assertCallLine(t, snapshot, "CALLS", "top.ts", "", ":helper", "top_level_call_site", markerLine(t, callLinesCodeTypeScriptTopLevel, "top-level-after-text"))
	assertCallLine(t, snapshot, "CALLS", "generic.ts", "genericAfterText", ":pick", "call_site", markerLine(t, callLinesCodeTypeScriptGeneric, "generic-after-text"))
	// The constructor chain: its relation is the class's method, reached by
	// `new Store().save()` in a caller that reaches it no other way (a caller
	// that also calls store.save() keeps that entry, one relation per caller
	// and target). callEntry fails when the relation is missing, and the
	// detail says it is the chain's.
	assertCallLine(t, snapshot, "CALLS", "a.tsx", "chainAfterText", ":Store.save", "call_site", markerLine(t, src, "chain-after-text"))
	if _, evidence := callEntry(t, snapshot, "CALLS", "a.tsx", "chainAfterText", ":Store.save"); evidence.Detail != "new Store().save" {
		t.Errorf("chainAfterText -> Store.save: detail %q, want the constructor chain's", evidence.Detail)
	}
}

const callLinesCodeJavaScript = `export class Ledger {
  add() {
    return 1;
  }
}

export async function work() {
  return 1;
}

export async function caller() {
  const ledger = new Ledger();
  const re = /ledger.add() await work()/;
  const view = <p>ledger.add() and await work()</p>;
  // ledger.add()
  ledger.add(); // receiver-after-text
  await work(); // async-after-text
  ledger.add();
  return [re, view];
}
`

// JavaScript (JSX) takes the TypeScript path: a regular expression, JSX text
// and a comment before the real calls. Reason: B2's shapes, in the other
// language the syntax tree reads.
func TestCallSiteLinesJavaScriptTextIsNotACall(t *testing.T) {
	snapshot := buildCallLineFixture(t, map[string]string{"a.jsx": callLinesCodeJavaScript})
	src := callLinesCodeJavaScript
	assertCallLine(t, snapshot, "CALLS", "a.jsx", "caller", ":Ledger.add", "call_site", markerLine(t, src, "receiver-after-text"))
	assertCallLine(t, snapshot, "ASYNC_CALLS", "a.jsx", "caller", ":work", "async_call_site", markerLine(t, src, "async-after-text"))
}

const callLinesCodeGo = "package main\n\n" +
	"import \"os\"\n\n" +
	"type Store struct{}\n\n" +
	"func (s *Store) Save() int { return 1 }\n\n" +
	"func helper() int { return 1 }\n\n" +
	"func worker() {}\n\n" +
	"// helper() at file level, in a comment\n" +
	"var initial = helper() // top-level-call\n\n" +
	"func caller(store *Store) string {\n" +
	"\t// store.Save() and os.Getwd() and go worker() in a comment\n" +
	"\ttext := \"store.Save() go worker()\"\n" +
	"\traw := `store.Save()\n" +
	"\tos.Getwd() go worker()`\n" +
	"\tstore.Save() // receiver-first\n" +
	"\tstore.Save()\n" +
	"\tgo worker() // async-first\n" +
	"\tgo worker()\n" +
	"\tdir, _ := os.Getwd() // imported-call\n" +
	"\treturn text + raw + dir\n" +
	"}\n"

// Go: comments, strings and raw strings across lines are not code, so a
// receiver, imported or goroutine call written first in any of them is placed
// on the real call, the first of several. A package-level
// var initialiser's call is the file's, on its own line. Reason: the brief
// asks for each scan in each language, and the Go file-level call was
// asserted nowhere.
func TestCallSiteLinesGoTextIsNotACall(t *testing.T) {
	snapshot := buildCallLineFixture(t, map[string]string{"go.mod": "module example.com/m\n\ngo 1.22\n", "main.go": callLinesCodeGo})
	src := callLinesCodeGo
	assertCallLine(t, snapshot, "CALLS", "main.go", "caller", ":Store.Save", "call_site", markerLine(t, src, "receiver-first"))
	assertCallLine(t, snapshot, "ASYNC_CALLS", "main.go", "caller", ":worker", "async_call_site", markerLine(t, src, "async-first"))
	assertCallLine(t, snapshot, "CALLS", "main.go", "caller", "os.Getwd", "imported_call_site", markerLine(t, src, "imported-call"))
	assertCallLine(t, snapshot, "CALLS", "main.go", "", ":helper", "top_level_call_site", markerLine(t, src, "top-level-call"))
}

const callLinesBoundLib = `export function target(): number {
  return 1;
}

export function apply(value: unknown): unknown {
  return value;
}
`

const callLinesBoundCallers = `import { apply } from "./lib";

export function byParameter(target: number): unknown {
  return apply(target);
}

export function byLocal(): unknown {
  const target = 1;
  return apply(target);
}

export function byDestructured(options: { target: number }): unknown {
  const { target } = options;
  return apply(target);
}

export function byLoop(values: number[]): unknown {
  for (const target of values) {
    apply(target);
  }
  return values;
}

export function byCatch(): unknown {
  try {
    return 1;
  } catch (target) {
    return apply(target);
  }
}

export function byInnerArrow(values: number[]): unknown {
  return values.map((target) => apply(target));
}

for (const target of [1, 2]) {
  apply(target);
}
`

const callLinesBoundTarget = `import { apply, target } from "./lib";

export function passes(): unknown {
  return apply(target); // passes-target
}

export function both(values: number[]): unknown {
  for (const target of values) {
    apply(target);
  }
  return apply(target); // passes-after-local
}

apply(target); // file-level-target
`

// A name passed as an argument that is bound where it is passed -- a
// parameter, a local, a destructured, loop or catch binding, an inner arrow's
// parameter, a file-level loop binding -- is not the same-named function
// elsewhere: no relation. The target itself passed is a marked reference, and
// a caller that passes both keeps the relation at the target's line. Reason:
// the addendum; on Neuve these were most of the marked references, each a
// local value that shares a function's name.
func TestCallSiteLinesBoundArgumentIsNotTheTarget(t *testing.T) {
	snapshot := buildCallLineFixture(t, map[string]string{"lib.ts": callLinesBoundLib, "callers.test.ts": callLinesBoundCallers, "target.test.ts": callLinesBoundTarget})
	for _, caller := range []string{"byParameter", "byLocal", "byDestructured", "byLoop", "byCatch", "byInnerArrow"} {
		assertNoCallRelation(t, snapshot, "CALLS", "callers.test.ts", caller, "lib.ts:function:target")
	}
	assertNoCallRelation(t, snapshot, "CALLS", "callers.test.ts", "", "lib.ts:function:target")
	// The same shape with the target itself gives a relation, so the ones
	// above are absent because of the binding, not because the shape gives none.
	assertCallReference(t, snapshot, "CALLS", "target.test.ts", "passes", "lib.ts:function:target", "call_site", markerLine(t, callLinesBoundTarget, "passes-target"))
	assertCallReference(t, snapshot, "CALLS", "target.test.ts", "both", "lib.ts:function:target", "call_site", markerLine(t, callLinesBoundTarget, "passes-after-local"))
	assertCallReference(t, snapshot, "CALLS", "target.test.ts", "", "lib.ts:function:target", "top_level_call_site", markerLine(t, callLinesBoundTarget, "file-level-target"))
}

const callLinesEscapedHelpers = `def compute():
    return 1
`

const callLinesEscapedPython = `import os
import helpers


def escaped_double():
    """doc with \""" inside helpers.compute() and os.getcwd() """
    q = 1
    helpers.compute()  # double-module-call
    return os.getcwd()  # double-imported-call


def escaped_single():
    '''doc with \''' inside helpers.compute() and os.getcwd() '''
    q = 1
    helpers.compute()  # single-module-call
    return os.getcwd()  # single-imported-call


def escaped_raw():
    r"""raw doc with \""" inside helpers.compute() and os.getcwd() """
    q = 1
    helpers.compute()  # raw-module-call
    return os.getcwd()  # raw-imported-call
`

// Python: a triple-quoted string holding a backslash before its own quote
// run (three double or three single quotes) does not end there; a raw one
// does not either, as the language reference says (a quote after a backslash is escaped even in a raw
// literal). The calls the docstring spells after it are text, and the module
// and imported calls lower are the lines. Reason: the review of the round
// (N1): the Python mask ended the string at \""", read its tail as code and
// put both entries on the docstring's line.
func TestCallSiteLinesPythonEscapedTripleQuoteIsText(t *testing.T) {
	snapshot := buildCallLineFixture(t, map[string]string{"helpers.py": callLinesEscapedHelpers, "app.py": callLinesEscapedPython})
	src := callLinesEscapedPython
	for _, shape := range []struct{ caller, prefix string }{{"escaped_double", "double"}, {"escaped_single", "single"}, {"escaped_raw", "raw"}} {
		assertCallLine(t, snapshot, "CALLS", "app.py", shape.caller, "helpers.py:function:compute", "call_site", markerLine(t, src, shape.prefix+"-module-call"))
		assertCallLine(t, snapshot, "CALLS", "app.py", shape.caller, "os.getcwd", "imported_call_site", markerLine(t, src, shape.prefix+"-imported-call"))
	}
}

const callLinesRawBackslashGo = "package main\n\n" +
	"type S struct{}\n\n" +
	"func (s *S) Save() int { return 1 }\n\n" +
	"func helper() int { return 1 }\n\n" +
	"var sep = `\\`\n" +
	"var doc = `helper() at file level`\n" +
	"var initial = helper() // top-level-call\n\n" +
	"func caller(s *S) string {\n" +
	"\tsep := `\\`\n" +
	"\tdoc := `helper() and s.Save() are here`\n" +
	"\tq := 1\n" +
	"\thelper() // bare-call\n" +
	"\ts.Save() // receiver-call\n" +
	"\treturn sep + doc + string(rune(q))\n" +
	"}\n"

// Go: a raw string takes no escapes, so one ending in a backslash ends at the
// backtick after it. The calls the next raw string spells are text, and the
// real bare and receiver calls lower are the lines, in a function and at file
// level. Reason: the review of the round (N2): Go's view was the generic
// mask, which reads that backtick as escaped and swapped code and text for
// the rest of the caller.
func TestCallSiteLinesGoRawStringEndingInBackslash(t *testing.T) {
	snapshot := buildCallLineFixture(t, map[string]string{"go.mod": "module example.com/m\n\ngo 1.22\n", "main.go": callLinesRawBackslashGo})
	src := callLinesRawBackslashGo
	assertCallLine(t, snapshot, "CALLS", "main.go", "caller", ":function:helper", "call_site", markerLine(t, src, "bare-call"))
	assertCallLine(t, snapshot, "CALLS", "main.go", "caller", ":S.Save", "call_site", markerLine(t, src, "receiver-call"))
	assertCallLine(t, snapshot, "CALLS", "main.go", "", ":function:helper", "top_level_call_site", markerLine(t, src, "top-level-call"))
}

// A TypeScript file whose scope walk stopped at the depth limit has no syntax
// tree to tell code from text, so its code view is blank: a call the tree did
// not record gets no line, even where the text scan finds the name in a
// regular expression first. Reason: the review of the round (note 3): a
// view made all code there instead put the entry on the regular expression,
// and no test failed.
func TestCallSiteLinesTypeScriptNoWholeTreeIsNotCode(t *testing.T) {
	src := "export function helper(): number {\n  return 1;\n}\n\n" +
		"export function afterText(): unknown {\n  const re = /helper()/;\n  helper();\n  return re;\n}\n\n" +
		nestedSource("export const nested = ", '(', ')', 6000, "1", ";\n")
	scan, err := newJSScanState("deep.ts", src)
	if err != nil {
		t.Fatal(err)
	}
	if !scan.depthTruncated {
		t.Fatal("the fixture no longer stops the scope walk at the depth limit")
	}
	snapshot := buildCallLineFixture(t, map[string]string{"deep.ts": src})
	assertCallNoLine(t, snapshot, "CALLS", "deep.ts", "afterText", ":function:helper")
}

const callLinesNestedFStringPython = "import os\nimport helpers\nfrom helpers import compute\n\n\n" +
	"def nested_same_quote():\n" +
	"    label = f\"{\"helpers.compute() and os.getcwd() and compute()\"}\"\n" +
	"    helpers.compute()\n" +
	"    compute()  # nested-bare-call\n" +
	"    return os.getcwd(), label  # nested-imported-call\n\n\n" +
	"def nested_single_only():\n" +
	"    return f'{'os.getcwd()'}'\n\n\n" +
	"def nested_prefixes():\n" +
	"    a = rf\"{\"helpers.compute()\"}\" + T'{'os.getcwd()'}'\n" +
	"    b = f\"{f\"{\"os.getcwd()\"}\"}\"\n" +
	"    return os.getcwd(), a, b  # prefixes-imported-call\n\n\n" +
	"class Store:\n" +
	"    def refresh(self):\n" +
	"        return 1\n\n" +
	"    def in_braces(self):\n" +
	"        text = f\"{self.refresh()} done\"  # braces-receiver\n" +
	"        self.refresh()\n" +
	"        return text\n"

// Python 3.12 and later (PEP 701): a string nested in an f-string's braces
// may use the outer quote, so the outer quote ends the f-string only outside
// every field. The nested string's text is not code: the real calls lower are
// the lines, and a caller that only spells a call there keeps its relation
// (found by the receiver scan's generic mask) with no line. The same holds
// for rf and T prefixes and an f-string nested in a field. A field itself is
// code, so a receiver call written in one is the first call. Reason: the
// review of round 10c (N3): the f-string ended at the nested string's opening
// quote, its text read as code, and two entries sat on lines with no call.
func TestCallSiteLinesPythonNestedFStringIsText(t *testing.T) {
	snapshot := buildCallLineFixture(t, map[string]string{"helpers.py": callLinesEscapedHelpers, "app.py": callLinesNestedFStringPython})
	src := callLinesNestedFStringPython
	assertCallLine(t, snapshot, "CALLS", "app.py", "nested_same_quote", "helpers.py:function:compute", "call_site", markerLine(t, src, "nested-bare-call"))
	assertCallLine(t, snapshot, "CALLS", "app.py", "nested_same_quote", "os.getcwd", "imported_call_site", markerLine(t, src, "nested-imported-call"))
	assertCallNoLine(t, snapshot, "CALLS", "app.py", "nested_single_only", "os.getcwd")
	assertCallLine(t, snapshot, "CALLS", "app.py", "nested_prefixes", "os.getcwd", "imported_call_site", markerLine(t, src, "prefixes-imported-call"))
	assertCallNoLine(t, snapshot, "CALLS", "app.py", "nested_prefixes", "helpers.py:function:compute")
	assertCallLine(t, snapshot, "CALLS", "app.py", "in_braces", ":Store.refresh", "call_site", markerLine(t, src, "braces-receiver"))
}

const callLinesRawBackslashOnlyTextGo = "package main\n\n" +
	"import \"os\"\n\n" +
	"type S struct{}\n\n" +
	"func (s *S) Save() int { return 1 }\n\n" +
	"func helper() int { return 1 }\n\n" +
	"func onlyText(s *S) string {\n" +
	"\tsep := `\\`\n" +
	"\tdoc := `helper() and s.Save() and os.Getenv(\"x\") are here`\n" +
	"\treturn sep + doc\n" +
	"}\n"

// Go: a caller whose only spellings of its calls are in a raw string after a
// raw string ending in a backslash keeps the relations the scanners find
// (they read the generic mask), each with no line. Reason: Neuve's review of
// round 10c: the raw-string test always had a real call, so nothing pinned
// the text-only case.
func TestCallSiteLinesGoRawStringOnlyTextHasNoLine(t *testing.T) {
	snapshot := buildCallLineFixture(t, map[string]string{"go.mod": "module example.com/m\n\ngo 1.22\n", "main.go": callLinesRawBackslashOnlyTextGo})
	assertCallNoLine(t, snapshot, "CALLS", "main.go", "onlyText", ":function:helper")
	assertCallNoLine(t, snapshot, "CALLS", "main.go", "onlyText", ":S.Save")
	assertCallNoLine(t, snapshot, "CALLS", "main.go", "onlyText", "os.Getenv")
}

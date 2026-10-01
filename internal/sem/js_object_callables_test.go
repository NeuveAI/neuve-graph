package sem

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

const namedObjectCallableFixture = `function target() { return true; }
function initialize() { return 1; }
function defer(fn) { return fn; }
export const A = {
  eager: initialize(),
  stopSession: () => target(),
  deferred: () => defer(function () { return target(); }),
  named: function internal(value) { return target(); },
  nested: { stopSession: () => target() },
};
const B = { stopSession: () => target() };
function factory() { return { stopSession: () => target() }; }
class Service extends define({
  effect: defer(function () {
    return { stopSession: () => defer(function () { return target(); }) };
  }),
}) {}
function bareDecoy() { return stopSession(); }
function receivers() { A.stopSession(); B.stopSession(); Service.stopSession(); }
`

func namedObjectSnapshot(t *testing.T, path, source string) ProviderSnapshot {
	t.Helper()
	repo := t.TempDir()
	writeFile(t, repo, path, source)
	snapshot, err := BuildProviderSnapshot(t.Context(), repo, "test-version")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Header.PartialFailures) != 0 {
		t.Fatalf("partial failures: %#v", snapshot.Header.PartialFailures)
	}
	return snapshot
}

func objectSnapshotEdges(snapshot ProviderSnapshot, relation string) []string {
	names := map[string]string{}
	for _, file := range snapshot.Files {
		names[file.ID] = "file:" + file.Path
	}
	for _, symbol := range snapshot.Symbols {
		names[symbol.ID] = symbol.QualifiedName
	}
	var edges []string
	for _, edge := range snapshot.Relations {
		if edge.Type == relation && names[edge.FromID] != "" && names[edge.ToID] != "" {
			edges = append(edges, names[edge.FromID]+" -> "+names[edge.ToID])
		}
	}
	sort.Strings(edges)
	return edges
}

func TestNamedObjectCallablesPreserveStaticOwnership(t *testing.T) {
	for _, path := range []string{"source.ts", "source.js"} {
		t.Run(path, func(t *testing.T) {
			snapshot := namedObjectSnapshot(t, path, namedObjectCallableFixture)
			var observed []string
			for _, symbol := range snapshot.Symbols {
				if strings.Contains(symbol.QualifiedName, "stopSession") || symbol.objectLiteral {
					observed = append(observed, symbol.Kind+":"+symbol.QualifiedName)
				}
			}
			sort.Strings(observed)
			want := []string{"method:A.nested.stopSession", "method:A.stopSession", "method:B.stopSession", "method:Service.stopSession", "method:factory.stopSession", "variable:A", "variable:A.nested", "variable:B"}
			if !reflect.DeepEqual(observed, want) {
				t.Fatalf("named objects and properties = %v; want %v", observed, want)
			}
			wantCalls := []string{
				"A -> initialize", "A.deferred -> defer", "A.deferred -> target", "A.named -> target",
				"A.nested.stopSession -> target", "A.stopSession -> target", "B.stopSession -> target",
				"Service.stopSession -> defer", "Service.stopSession -> target", "factory -> target",
				"factory.stopSession -> target", "receivers -> A.stopSession", "receivers -> B.stopSession",
			}
			sort.Strings(wantCalls)
			if actual := objectSnapshotEdges(snapshot, "CALLS"); !reflect.DeepEqual(actual, wantCalls) {
				t.Fatalf("CALLS = %v; want %v", actual, wantCalls)
			}
			wantContains := []string{"A -> A.deferred", "A -> A.named", "A -> A.nested", "A -> A.stopSession", "A.nested -> A.nested.stopSession", "B -> B.stopSession", "Service -> Service.stopSession", "factory -> factory.stopSession"}
			sort.Strings(wantContains)
			if actual := objectSnapshotEdges(snapshot, "CONTAINS"); !reflect.DeepEqual(actual, wantContains) {
				t.Fatalf("CONTAINS = %v; want %v", actual, wantContains)
			}
		})
	}
}

func TestObjectPropertyRefusalRetainsNamedExpression(t *testing.T) {
	snapshot := namedObjectSnapshot(t, "keys.ts", `function helper() { return 1; }
export const A = {
  data: 1,
  [dynamic]: function kept() { return helper(); },
  valid: function redundant() { return helper(); },
  "quoted": () => helper(),
};
`)
	var callables []string
	for _, symbol := range snapshot.Symbols {
		if symbol.Kind == "function" || symbol.Kind == "method" {
			callables = append(callables, symbol.QualifiedName)
		}
	}
	sort.Strings(callables)
	if want := []string{"A.quoted", "A.valid", "helper", "kept"}; !reflect.DeepEqual(callables, want) {
		t.Fatalf("callables = %v; want %v", callables, want)
	}
	if want := []string{"A.quoted -> helper", "A.valid -> helper", "kept -> helper"}; !reflect.DeepEqual(objectSnapshotEdges(snapshot, "CALLS"), want) {
		t.Fatalf("CALLS = %v; want %v", objectSnapshotEdges(snapshot, "CALLS"), want)
	}
}

func TestQuotedObjectKeysDoNotLoseLiteralQuoteCharacters(t *testing.T) {
	for _, path := range []string{"keys.ts", "keys.js"} {
		t.Run(path, func(t *testing.T) {
			snapshot := namedObjectSnapshot(t, path, `function helper() { return 1; }
const A = { "stop'": function quoted() { return helper(); }, stop: () => helper() };
`)
			var callables []string
			for _, symbol := range snapshot.Symbols {
				if callableTargetKind(symbol.Kind) {
					callables = append(callables, symbol.QualifiedName)
				}
			}
			sort.Strings(callables)
			if want := []string{"A.stop", "helper", "quoted"}; !reflect.DeepEqual(callables, want) {
				t.Fatalf("callables = %v; want %v", callables, want)
			}
			if want := []string{"A.stop -> helper", "quoted -> helper"}; !reflect.DeepEqual(objectSnapshotEdges(snapshot, "CALLS"), want) {
				t.Fatalf("CALLS = %v; want %v", objectSnapshotEdges(snapshot, "CALLS"), want)
			}
		})
	}
}

func TestObjectCallableIdentitySurvivesBodyEditsAndSiblingObjects(t *testing.T) {
	before, _, status := (TreeSitterParser{}).ParseWithStatus("identity.ts", namedObjectCallableFixture)
	if status.ParseError {
		t.Fatal(status)
	}
	afterSource := "const unrelated = { stopSession: () => 4 };\n" + strings.ReplaceAll(namedObjectCallableFixture, "return true", "return false")
	after, _, status := (TreeSitterParser{}).ParseWithStatus("identity.ts", afterSource)
	if status.ParseError {
		t.Fatal(status)
	}
	beforeSymbols := entitySymbols("local/fixture", "identity.ts", "TypeScript", before)
	afterSymbols := entitySymbols("local/fixture", "identity.ts", "TypeScript", after)
	ids := map[string]string{}
	for _, symbol := range afterSymbols {
		ids[symbol.QualifiedName] = symbol.ID
	}
	for _, symbol := range beforeSymbols {
		if actual := ids[symbol.QualifiedName]; actual != symbol.ID {
			t.Fatalf("%s changed ID %q -> %q", symbol.QualifiedName, symbol.ID, actual)
		}
	}
	for _, symbol := range beforeSymbols {
		if symbol.QualifiedName == "A.named" && symbol.Signature != "named: function internal(value)" {
			t.Fatalf("named expression signature = %q", symbol.Signature)
		}
	}
}

func TestNamedCallableExtractionRetainsOtherLanguageOwnership(t *testing.T) {
	for _, fixture := range []struct{ path, source, edge string }{
		{"source.py", "def helper():\n    return True\nclass Service:\n    def stopSession(self):\n        return helper()\n", "Service.stopSession -> helper"},
		{"source.rb", "def helper\n  true\nend\nclass Service\n  def stopSession\n    helper()\n  end\nend\n", "Service.stopSession -> helper"},
	} {
		t.Run(fixture.path, func(t *testing.T) {
			snapshot := namedObjectSnapshot(t, fixture.path, fixture.source)
			if want := []string{fixture.edge}; !reflect.DeepEqual(objectSnapshotEdges(snapshot, "CALLS"), want) {
				t.Fatalf("CALLS = %v; want %v", objectSnapshotEdges(snapshot, "CALLS"), want)
			}
		})
	}
}

func TestLocalObjectPathsDoNotGrantExternalReceiverAccess(t *testing.T) {
	snapshot := namedObjectSnapshot(t, "locals.ts", `function helper() { return true; }
function factory() { const A = { stop: () => helper() }; }
class C { static { const A = { stop: () => helper() }; } }
function outside() { factory.A.stop(); C.A.stop(); }
factory.A.stop(); C.A.stop();
`)
	if want := []string{"C.A.stop -> helper", "factory -> helper", "factory.A.stop -> helper"}; !reflect.DeepEqual(objectSnapshotEdges(snapshot, "CALLS"), want) {
		t.Fatalf("CALLS = %v; want %v", objectSnapshotEdges(snapshot, "CALLS"), want)
	}
}

func TestObjectInitializerDoesNotReacquireNestedNamespaceCalls(t *testing.T) {
	snapshot := namedObjectSnapshot(t, "namespace.ts", `namespace Helpers { export function eager() { return 1; } export function later() { return 2; } }
export const api = { value: Helpers.eager(), stop: () => Helpers.later() };
`)
	if want := []string{"api -> eager", "api.stop -> later"}; !reflect.DeepEqual(objectSnapshotEdges(snapshot, "CALLS"), want) {
		t.Fatalf("CALLS = %v; want %v", objectSnapshotEdges(snapshot, "CALLS"), want)
	}
}

func TestPropertyScopeKeepsNestedHelperIdentityDistinct(t *testing.T) {
	before := `const A = { stop: () => { function helper() { return 1; } return helper(); } };`
	addition := `
const B = { stop: () => { function helper() { return 2; } return helper(); } };`
	parse := func(source string) []SymbolRecord {
		entities, language, status := (TreeSitterParser{}).ParseWithStatus("scope.ts", source)
		if status.ParseError {
			t.Fatal(status)
		}
		return entitySymbols("local/fixture", "scope.ts", language, entities)
	}
	var beforeID string
	for _, symbol := range parse(before) {
		if symbol.QualifiedName == "A.stop.helper" {
			beforeID = symbol.ID
		}
	}
	if beforeID != "local/fixture:TypeScript:scope.ts:function:A.stop.helper" {
		t.Fatalf("nested helper ID = %q", beforeID)
	}
	var names []string
	for _, symbol := range parse(before + addition) {
		if symbol.Name == "helper" {
			names = append(names, symbol.QualifiedName)
			if symbol.QualifiedName == "A.stop.helper" && symbol.ID != beforeID {
				t.Fatalf("adding B rekeyed A's helper: %q", symbol.ID)
			}
		}
	}
	if want := []string{"A.stop.helper", "B.stop.helper"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("helpers = %v; want %v", names, want)
	}
	snapshot := namedObjectSnapshot(t, "scope.ts", before+addition)
	if want := []string{"A -> A.stop", "A.stop -> A.stop.helper", "B -> B.stop", "B.stop -> B.stop.helper"}; !reflect.DeepEqual(objectSnapshotEdges(snapshot, "CONTAINS"), want) {
		t.Fatalf("CONTAINS = %v; want %v", objectSnapshotEdges(snapshot, "CONTAINS"), want)
	}
}

func TestAnonymousObjectDuplicateKeysKeepSeparateDeclarations(t *testing.T) {
	snapshot := namedObjectSnapshot(t, "duplicates.ts", `function one() { return 1; }
function two() { return 2; }
function make(flag) { return flag ? { stop: () => one() } : { stop: () => two() }; }
function outside() { make.stop(); }
`)
	var methods []SymbolRecord
	for _, symbol := range snapshot.Symbols {
		if symbol.QualifiedName == "make.stop" {
			methods = append(methods, symbol)
		}
	}
	if len(methods) != 2 || methods[0].ID == methods[1].ID {
		t.Fatalf("duplicate property declarations = %#v", methods)
	}
	for index, method := range methods {
		wantTarget := []string{"one", "two"}[index]
		var targets []string
		for _, edge := range snapshot.Relations {
			if edge.Type == "CALLS" && edge.FromID == method.ID {
				for _, target := range snapshot.Symbols {
					if edge.ToID == target.ID {
						targets = append(targets, target.Name)
					}
				}
			}
		}
		if !reflect.DeepEqual(targets, []string{wantTarget}) {
			t.Fatalf("property %d targets = %v; want %s", index, targets, wantTarget)
		}
	}
	for _, edge := range objectSnapshotEdges(snapshot, "CALLS") {
		if strings.HasPrefix(edge, "outside ->") {
			t.Fatalf("anonymous object granted fabricated receiver: %s", edge)
		}
	}
}

func TestObjectCallableCacheRetainsOwnershipAndInitializerCalls(t *testing.T) {
	repo := t.TempDir()
	initRepo(t, repo)
	writeFile(t, repo, "source.ts", namedObjectCallableFixture)
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "callable fixture")
	options := ProviderSnapshotOptions{Profile: ProfileFull}
	cacheDir := t.TempDir()
	cold, hit, err := loadOrBuildSearchGraphSnapshot(t.Context(), repo, "test-version", options, cacheDir, false)
	if err != nil || hit {
		t.Fatalf("cold build hit=%v error=%v", hit, err)
	}
	warm, hit, err := loadOrBuildSearchGraphSnapshot(t.Context(), repo, "test-version", options, cacheDir, false)
	if err != nil || !hit {
		t.Fatalf("warm build hit=%v error=%v", hit, err)
	}
	selectiveOptions := options
	selectiveOptions.OnlyFiles = []string{"source.ts"}
	derived, hit, err := loadOrBuildSearchGraphSnapshot(t.Context(), repo, "test-version", selectiveOptions, cacheDir, false)
	if err != nil || !hit {
		t.Fatalf("derived build hit=%v error=%v", hit, err)
	}
	for _, observed := range []ProviderSnapshot{warm, derived} {
		if !reflect.DeepEqual(objectSnapshotEdges(observed, "CALLS"), objectSnapshotEdges(cold, "CALLS")) || !reflect.DeepEqual(objectSnapshotEdges(observed, "CONTAINS"), objectSnapshotEdges(cold, "CONTAINS")) {
			t.Fatalf("cached ownership changed CALLS or CONTAINS")
		}
	}
	if !reflect.DeepEqual(objectSnapshotEdges(cold, "CALLS"), []string{"A -> initialize", "A.deferred -> defer", "A.deferred -> target", "A.named -> target", "A.nested.stopSession -> target", "A.stopSession -> target", "B.stopSession -> target", "Service.stopSession -> defer", "Service.stopSession -> target", "factory -> target", "factory.stopSession -> target", "receivers -> A.stopSession", "receivers -> B.stopSession"}) {
		t.Fatalf("cold CALLS = %v", objectSnapshotEdges(cold, "CALLS"))
	}
}

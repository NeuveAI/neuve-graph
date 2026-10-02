package sem

import (
	"strings"
	"testing"
	"time"
)

// These are source-to-snapshot controls, not assertions about the scope helper.
func TestNamedDeclarationSelfCalls(t *testing.T) {
	for _, extension := range []string{"js", "ts"} {
		for _, tc := range []struct {
			name, body string
			wantSelf   int
		}{
			{"direct", "helper();\n  return self();", 1},
			{"repeated", "helper();\n  self();\n  return self();", 1},
			{"header only", "helper(); return 1;", 0},
			{"text only", "helper(); const text = 'self()'; const rx = /self\\(\\)/; const template = `self()`; /* self() */ return text;", 0},
			{"local binding", "const self = () => 1; helper(); return self();", 0},
			{"block binding", "{ const self = () => 1; self(); } helper();", 0},
			{"block shadow with real site", "{ const self = () => 1; self(); } helper();\n  return self();", 1},
			{"catch binding", "try { helper(); } catch (self) { self(); }", 0},
			{"destructuring binding", "const { self } = values; helper(); return self();", 0},
			{"nested same name", "helper();\n  function self() { return 1; }\n  return self();", 0},
			{"nested other callable", "helper();\n  function nested() { return self(); }\n  return 1;", 0},
			{"callback only", "helper(self); return 1;", 0},
			{"construction only", "helper(); return new self();", 0},
			{"assignment", "helper(); self = replacement; return self();", 0},
			{"parenthesized assignment", "helper(); (self) = replacement; return self();", 0},
			{"destructuring assignment", "helper(); ({ self } = replacement); return self();", 0},
			{"array assignment", "helper(); [self] = replacement; return self();", 0},
			{"augmented assignment", "helper(); self += replacement; return self();", 0},
			{"update", "helper(); self++; return self();", 0},
			{"prefix update", "helper(); --self; return self();", 0},
			{"dynamic scope only", "helper(); with ({ self: () => 1 }) { return self(); }", 0},
			{"dynamic then original binding", "helper(); with ({ self: () => 1 }) { self(); }\n  return self();", 1},
			{"nested dynamic callable", "helper(); function nested() { with ({ self: () => 1 }) { self(); } }", 0},
		} {
			t.Run(extension+"/"+tc.name, func(t *testing.T) {
				repo := t.TempDir()
				path := "focus." + extension
				writeFile(t, repo, path, "export function self() {\n  "+tc.body+"\n}\nfunction helper(value) { return value; }\n")
				writeFile(t, repo, "decoy."+extension, "export function self() { return 2; }\n")
				snapshot, err := BuildProviderSnapshotWithOptions(t.Context(), repo, "test", ProviderSnapshotOptions{Worktree: true, Profile: ProfileFull})
				if err != nil {
					t.Fatal(err)
				}
				var source SymbolRecord
				var helper SymbolRecord
				for _, symbol := range snapshot.Symbols {
					if symbol.FilePath == path && symbol.Name == "self" && symbol.StartLine == 1 {
						source = symbol
					}
					if symbol.FilePath == path && symbol.Name == "helper" {
						helper = symbol
					}
				}
				if source.Name != "self" || source.Kind != "function" || helper.Name != "helper" {
					t.Fatalf("source definitions missing: %#v", snapshot.Symbols)
				}
				selfCount, helperCount := 0, 0
				for _, relation := range snapshot.Relations {
					if relation.FromID != source.ID || relation.Type != "CALLS" {
						continue
					}
					if relation.ToID == helper.ID {
						helperCount++
					}
					if relation.ToID == source.ID {
						selfCount++
						if relation.Resolution != "exact" || relation.RelationScope != "file" || relation.Confidence != 0.92 || relation.TargetKind != "symbol" {
							t.Fatalf("self attribution: %#v", relation)
						}
						if len(relation.Evidence) != 1 {
							t.Fatalf("self relation witness count = %d, want 1", len(relation.Evidence))
						}
						for _, evidence := range relation.Evidence {
							if evidence.Kind != "call_site" || evidence.FilePath != path || evidence.StartLine != 3 || evidence.EndLine != 3 || evidence.Detail != "self" {
								t.Fatalf("self site includes declaration header: %#v", evidence)
							}
						}
					}
				}
				if helperCount != 1 {
					t.Errorf("independent helper CALLS = %d, want 1", helperCount)
				}
				if selfCount != tc.wantSelf {
					t.Errorf("self CALLS = %d, want %d", selfCount, tc.wantSelf)
				}
			})
		}
	}
}

func TestNamedDeclarationSelfCallProfiles(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, repo, "focus.js", "export function self() { return self(); }\n")
	for _, tc := range []struct {
		profile Profile
		want    int
	}{{ProfileFull, 1}, {ProfileFast, 1}, {ProfileSyntaxOnly, 0}} {
		snapshot, err := BuildProviderSnapshotWithOptions(t.Context(), repo, "test", ProviderSnapshotOptions{Worktree: true, Profile: tc.profile})
		if err != nil {
			t.Fatal(err)
		}
		var source SymbolRecord
		for _, symbol := range snapshot.Symbols {
			if symbol.Name == "self" {
				source = symbol
			}
		}
		if source.Name != "self" {
			t.Fatal("source definition missing")
		}
		count := 0
		for _, relation := range snapshot.Relations {
			if relation.Type == "CALLS" && relation.FromID == source.ID && relation.ToID == source.ID {
				count++
			}
		}
		if count != tc.want {
			t.Fatalf("profile %s self calls = %d, want %d", tc.profile, count, tc.want)
		}
	}
}

func TestNamedDeclarationSelfCallWrappedWrites(t *testing.T) {
	for _, target := range []string{"(self as any)", "(<any>self)", "self!", "((self as any)!)", "[self!]", "{ value: self! }"} {
		t.Run(target, func(t *testing.T) {
			repo := t.TempDir()
			writeFile(t, repo, "focus.ts", "export function self() { helper(); "+target+" = replacement; return self(); }\nfunction helper() {}\n")
			snapshot, err := BuildProviderSnapshotWithOptions(t.Context(), repo, "test", ProviderSnapshotOptions{Worktree: true, Profile: ProfileFull})
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Header.PartialFailures) != 0 {
				t.Fatalf("fixture must parse without recovery: %#v", snapshot.Header.PartialFailures)
			}
			var source, helper SymbolRecord
			for _, symbol := range snapshot.Symbols {
				if symbol.Name == "self" {
					source = symbol
				}
				if symbol.Name == "helper" {
					helper = symbol
				}
			}
			if source.Name != "self" || helper.Name != "helper" {
				t.Fatal("literal source definitions missing")
			}
			helperCount := 0
			for _, relation := range snapshot.Relations {
				if relation.Type != "CALLS" || relation.FromID != source.ID {
					continue
				}
				if relation.ToID == source.ID {
					t.Fatalf("wrapped explicit write granted exact self: %#v", relation)
				}
				if relation.ToID == helper.ID {
					helperCount++
				}
			}
			if helperCount != 1 {
				t.Fatalf("independent helper CALLS = %d, want 1", helperCount)
			}
		})
	}
}

func TestNamedDeclarationSelfCallWritesIgnoreTypesAndValues(t *testing.T) {
	for _, write := range []string{"(other as typeof self) = replacement", "(<typeof self>other) = replacement", "self!.value = replacement", "other = self"} {
		t.Run(write, func(t *testing.T) {
			repo := t.TempDir()
			writeFile(t, repo, "focus.ts", "export function self() {\n  "+write+";\n  return self();\n}\n")
			snapshot, err := BuildProviderSnapshotWithOptions(t.Context(), repo, "test", ProviderSnapshotOptions{Worktree: true, Profile: ProfileFull})
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Header.PartialFailures) != 0 {
				t.Fatalf("fixture must parse without recovery: %#v", snapshot.Header.PartialFailures)
			}
			var source SymbolRecord
			for _, symbol := range snapshot.Symbols {
				if symbol.Name == "self" {
					source = symbol
				}
			}
			if source.Name != "self" {
				t.Fatal("source definition missing")
			}
			count := 0
			for _, relation := range snapshot.Relations {
				if relation.Type == "CALLS" && relation.FromID == source.ID && relation.ToID == source.ID {
					count++
					if len(relation.Evidence) != 1 || relation.Evidence[0].StartLine != 3 {
						t.Fatalf("actual self witness = %#v", relation.Evidence)
					}
				}
			}
			if count != 1 {
				t.Fatalf("nonbinding write self edges = %d, want 1", count)
			}
		})
	}
}

func TestNamedDeclarationSelfCallIncompleteScope(t *testing.T) {
	for _, kind := range []string{"depth", "write depth", "timeout"} {
		t.Run(kind, func(t *testing.T) {
			source := "export function self() { return self(); }\n"
			code := "E_PARSE_DEPTH_EXCEEDED"
			if kind == "write depth" {
				source += nestedSource("", '(', ')', 6000, "self", " = replacement;\n")
			} else if kind == "depth" {
				source += nestedSource("const deep = ", '(', ')', 6000, "1", ";\n")
			} else {
				code = "E_PARSE_TIMEOUT"
				saved := jsScanParseTimeout
				jsScanParseTimeout = time.Nanosecond
				t.Cleanup(func() { jsScanParseTimeout = saved })
				source += strings.Repeat("const ordinary = 1;\n", 2000)
			}
			repo := t.TempDir()
			writeFile(t, repo, "focus.ts", source)
			snapshot, err := BuildProviderSnapshotWithOptions(t.Context(), repo, "test", ProviderSnapshotOptions{Worktree: true, Profile: ProfileFull})
			if err != nil {
				t.Fatal(err)
			}
			var symbol SymbolRecord
			for _, candidate := range snapshot.Symbols {
				if candidate.Name == "self" {
					symbol = candidate
				}
			}
			if symbol.Name != "self" {
				t.Fatal("shallow source definition missing")
			}
			for _, relation := range snapshot.Relations {
				if relation.Type == "CALLS" && relation.FromID == symbol.ID && relation.ToID == symbol.ID {
					t.Fatalf("incomplete scope granted exact self: %#v", relation)
				}
			}
			found := false
			for _, failure := range snapshot.Header.PartialFailures {
				if failure.Code == code && failure.FilePath == "focus.ts" && strings.Contains(failure.EffectOnCompleteness, "relation-phase") {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing relation-phase uncertainty %s: %#v", code, snapshot.Header.PartialFailures)
			}
		})
	}
}

func TestNamedDeclarationSelfCallUncertainty(t *testing.T) {
	for _, tc := range []struct{ name, source string }{
		{"parameter", "export function self(self) { helper(); return self(); }\n"},
		{"same scope rival", "export function self() { helper(); return self(); }\nfunction self() { return 1; }\n"},
		{"enclosing write", "export function self() { helper(); return self(); }\nself = replacement;\n"},
		{"enclosing destructuring write", "export function self() { helper(); return self(); }\n({ self } = replacement);\n"},
		{"recovery tree", "export function self() { helper(); return self(); }\nconst broken = ;\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			writeFile(t, repo, "focus.js", tc.source+"function helper() {}\n")
			snapshot, err := BuildProviderSnapshotWithOptions(t.Context(), repo, "test", ProviderSnapshotOptions{Worktree: true, Profile: ProfileFull})
			if err != nil {
				t.Fatal(err)
			}
			var source SymbolRecord
			for _, symbol := range snapshot.Symbols {
				if symbol.Name == "self" && symbol.StartLine == 1 {
					source = symbol
				}
			}
			if source.Name != "self" {
				t.Fatalf("source definition missing: %#v", snapshot.Symbols)
			}
			for _, relation := range snapshot.Relations {
				if relation.Type == "CALLS" && relation.FromID == source.ID && relation.ToID == source.ID {
					t.Fatalf("uncertain self target granted: %#v", relation)
				}
			}
			if tc.name == "recovery tree" {
				found := false
				for _, failure := range snapshot.Header.PartialFailures {
					if failure.Code == "E_PARSE_ERROR" && failure.FilePath == "focus.js" {
						found = true
					}
				}
				if !found {
					t.Fatalf("parse uncertainty hidden: %#v", snapshot.Header.PartialFailures)
				}
			}
		})
	}
}

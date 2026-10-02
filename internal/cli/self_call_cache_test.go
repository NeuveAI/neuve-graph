package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/entireio/entire-graph/internal/sem"
)

// An optional preserved pre-change executable seeds real historical omissions.
// Without it CI still proves source-backed cold/warm results in both families.
func TestNamedSelfCallCaches(t *testing.T) {
	repo, cacheDir := t.TempDir(), t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Entire Graph Test")
	git(t, repo, "config", "user.email", "graph@example.com")
	write(t, repo, "focus.js", "export function self() {\n  return self();\n}\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "self call cache fixture")
	beforeBinary := os.Getenv("GRAPH_SELF_CALL_BEFORE_BINARY")
	for _, family := range []string{"search", "records"} {
		t.Run(family, func(t *testing.T) {
			args := []string{"snapshot", "--repo", repo, "--cache-dir", cacheDir, "--format", "ndjson"}
			if family == "search" {
				args = []string{"neighbors", "--repo", repo, "--cache-dir", cacheDir, "--head", "--symbol", "self", "--direction", "out", "--selection", "exact", "--format", "json"}
			}
			var beforeID, beforeTree string
			if beforeBinary != "" {
				command := exec.CommandContext(t.Context(), beforeBinary, args...)
				var stderr bytes.Buffer
				command.Stderr = &stderr
				before, err := command.Output()
				if err != nil {
					t.Fatalf("before binary: %v\n%s", err, stderr.String())
				}
				beforeID, beforeTree = assertSelfCacheOutput(t, family, before, 0)
				entries, err := os.ReadDir(filepath.Join(cacheDir, family))
				if err != nil || len(entries) != 1 {
					t.Fatalf("before did not populate actual %s cache: %v %#v", family, err, entries)
				}
				t.Logf("before %s: source=%s tree=%s self_calls=0 populated_cache=%s", family, beforeID, beforeTree, entries[0].Name())
			}
			var cold, warm []byte
			for attempt := 0; attempt < 2; attempt++ {
				var out bytes.Buffer
				if err := Run(t.Context(), Options{Version: "dev", Stdout: &out}, args); err != nil {
					t.Fatal(err)
				}
				id, tree := assertSelfCacheOutput(t, family, out.Bytes(), 1)
				if beforeBinary != "" && (id != beforeID || tree != beforeTree) {
					t.Fatalf("cache correction changed source identity/tree: %s %s -> %s %s", beforeID, beforeTree, id, tree)
				}
				if family == "search" {
					var response neighborResponse
					if err := json.Unmarshal(out.Bytes(), &response); err != nil {
						t.Fatal(err)
					}
					if response.IndexCacheHit != (attempt == 1) {
						t.Fatalf("candidate search attempt %d cache hit=%t", attempt, response.IndexCacheHit)
					}
				}
				if attempt == 0 {
					cold = bytes.Clone(out.Bytes())
				} else {
					warm = bytes.Clone(out.Bytes())
				}
			}
			if family == "records" && !bytes.Equal(cold, warm) {
				t.Fatal("warm record cache changed the emitted source/relationship stream")
			}
			if beforeBinary != "" {
				entries, err := os.ReadDir(filepath.Join(cacheDir, family))
				if err != nil || len(entries) != 2 {
					t.Fatalf("candidate reused the before namespace: %v %#v", err, entries)
				}
				t.Logf("candidate %s: same source/tree, self_calls=1 cold and warm, preserved old and rebuilt namespaces", family)
			}
		})
	}
}

func assertSelfCacheOutput(t *testing.T, family string, output []byte, want int) (string, string) {
	t.Helper()
	if family == "search" {
		var response neighborResponse
		if err := json.Unmarshal(output, &response); err != nil {
			t.Fatal(err)
		}
		if len(response.Matches) != 1 || response.Matches[0].Symbol.Name != "self" {
			t.Fatalf("source missing from search: %s", output)
		}
		focus := response.Matches[0]
		if len(focus.Outgoing) != want {
			t.Fatalf("cached search self edges = %d, want %d: %s", len(focus.Outgoing), want, output)
		}
		for _, edge := range focus.Outgoing {
			if edge.Endpoint.ID != focus.Symbol.ID || edge.Resolution != "exact" {
				t.Fatalf("cached target identity wrong: %#v", edge)
			}
		}
		return focus.Symbol.ID, response.Tree
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	var id, tree string
	var relations []sem.RelationRecord
	for {
		var record json.RawMessage
		if err := decoder.Decode(&record); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		var discriminator struct {
			RecordType      string `json:"record_type"`
			Tree            string `json:"tree"`
			ProviderVersion string `json:"provider_version"`
		}
		if err := json.Unmarshal(record, &discriminator); err != nil {
			t.Fatal(err)
		}
		switch discriminator.RecordType {
		case "symbol":
			var symbol sem.SymbolRecord
			if err := json.Unmarshal(record, &symbol); err != nil {
				t.Fatal(err)
			}
			if symbol.Name == "self" {
				id = symbol.ID
			}
		case "relation":
			var relation sem.RelationRecord
			if err := json.Unmarshal(record, &relation); err != nil {
				t.Fatal(err)
			}
			relations = append(relations, relation)
		default:
			if discriminator.ProviderVersion != "" {
				if discriminator.ProviderVersion != "dev" {
					t.Fatalf("provider version changed: %s", record)
				}
				tree = discriminator.Tree
			}
		}
	}
	if id == "" || tree == "" {
		t.Fatalf("record source/tree missing: %s", output)
	}
	count := 0
	for _, relation := range relations {
		if relation.Type == "CALLS" && relation.FromID == id && relation.ToID == id {
			count++
		}
	}
	if count != want {
		t.Fatalf("cached records self edges = %d, want %d", count, want)
	}
	return id, tree
}

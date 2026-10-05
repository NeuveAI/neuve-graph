package sem

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func TestLSPExactPositionAndLocationQualification(t *testing.T) {
	text := "😀 name\n"
	if lspValidatePosition(text, LSPPosition{0, 1}) || !lspValidatePosition(text, LSPPosition{0, 2}) || lspValidatePosition(text, LSPPosition{-1, 0}) {
		t.Fatal("UTF-16 admission invalid")
	}
	root := t.TempDir()
	contents := map[string]string{"file.cpp": text}
	for _, uri := range []string{"https://example.invalid/x", "file://foreign/file.cpp", lspFileURI(filepath.Join(root, "../foreign.cpp"))} {
		raw, _ := json.Marshal(map[string]any{"uri": uri, "range": LSPRange{}})
		locations, err := lspLocations(raw, root, contents)
		if err != nil || len(locations) != 1 || locations[0].Availability != "unavailable" {
			t.Fatalf("foreign source falsely qualified %v %v", locations, err)
		}
	}
	raw, _ := json.Marshal(map[string]any{"uri": lspFileURI(filepath.Join(root, "file.cpp")), "range": LSPRange{LSPPosition{0, 2}, LSPPosition{0, 7}}})
	locations, err := lspLocations(raw, root, contents)
	if err != nil || locations[0].SHA256 != lspDigest([]byte(text)) {
		t.Fatalf("exact location %v %v", locations, err)
	}
}
func TestLSPDarwinNetworkAndChildConfinement(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS policy only")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(t.TempDir(), "foreign-source")
	cmd := exec.Command("/usr/bin/sandbox-exec", "-p", lspSeatbeltProfile(root, executable, filepath.Dir(foreign)), executable, "-test.run=^TestLSPPeerHelper$")
	if err = os.WriteFile(foreign, []byte("outside qualified input"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(), "NEUVE_LSP_TEST_FOREIGN_SOURCE="+foreign, "NEUVE_LSP_TEST_HELPER=policy", "NEUVE_LSP_TEST_ADDRESS="+listener.Addr().String())
	output, err := cmd.CombinedOutput()
	if err != nil || !bytes.Contains(output, []byte("network-and-child-denied")) {
		t.Fatalf("confinement did not prove denial: %s %v", output, err)
	}
}
func TestLSPInstalledClangdCommittedFixture(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS policy only")
	}
	executable := "/Library/Developer/CommandLineTools/usr/bin/clangd"
	server, err := os.ReadFile(executable)
	if err != nil {
		t.Skip("operator fixture server not installed")
	}
	repository := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repository
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fixture git: %s %v", output, err)
		}
		return strings.TrimSpace(string(output))
	}
	git("init", "-q")
	git("config", "user.email", "fixture@example.invalid")
	git("config", "user.name", "Fixture")
	git("config", "core.hooksPath", "/dev/null")
	source := "int add(int a, int b) { return a + b; }\nint main() { /* 😀 */ return add(1, 2); }\n"
	if err = os.WriteFile(filepath.Join(repository, "main.cpp"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("-c", "commit.gpgsign=false", "commit", "-qm", "A")
	commit := git("rev-parse", "HEAD")
	tree := git("rev-parse", "HEAD^{tree}")
	// A new HEAD and dirty source must not leak into the qualified historical view.
	os.WriteFile(filepath.Join(repository, "main.cpp"), []byte("int unrelated() { return 0; }\n"), 0600)
	git("add", ".")
	git("-c", "commit.gpgsign=false", "commit", "-qm", "B")
	os.WriteFile(filepath.Join(repository, "main.cpp"), []byte("dirty input"), 0600)
	position := LSPPosition{1, len(utf16.Encode([]rune("int main() { /* 😀 */ return ")))}
	binding := LSPProjectBinding{Repository: repository, Commit: commit, Tree: tree, PolicyID: "explicit-fixture-inputs", Inputs: []LSPProjectInput{{"main.cpp", lspDigest([]byte(source))}}, MaxProjectBytes: 1 << 20}
	config := LocalLSPConfig{Executable: executable, ExecutableSHA256: lspDigest(server), Policy: "darwin-seatbelt-clangd-v1", Limits: lspTestLimits()}
	config.Limits.RequestTimeout = 5e9
	config.Limits.ShutdownGrace = 1e9
	anchors := []LSPAnchor{{Path: "main.cpp", SHA256: lspDigest([]byte(source)), LanguageID: "cpp", Feature: "definition", Position: position}, {Path: "main.cpp", SHA256: lspDigest([]byte(source)), LanguageID: "cpp", Feature: "references", Position: LSPPosition{0, 4}}}
	artifact, err := PrepareLocalLSPEnrichment(context.Background(), binding, config, anchors)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Commit != commit || artifact.Tree != tree || artifact.PositionEncoding != "utf-16" || len(artifact.Observations) != 2 {
		t.Fatalf("qualification missing %+v", artifact)
	}
	definition := artifact.Observations[0]
	if definition.Availability != "observed" || len(definition.Locations) != 1 || definition.Locations[0].Range.Start != (LSPPosition{0, 4}) || definition.Locations[0].SHA256 != binding.Inputs[0].SHA256 {
		t.Fatalf("literal definition failed %+v", definition)
	}
	references := artifact.Observations[1]
	if len(references.Locations) != 1 || references.Locations[0].Range.Start != position {
		t.Fatalf("literal reference failed %+v", references)
	}
	for _, drift := range []func(*LSPProjectBinding, *LocalLSPConfig){func(b *LSPProjectBinding, c *LocalLSPConfig) { b.Tree = strings.Repeat("0", 40) }, func(b *LSPProjectBinding, c *LocalLSPConfig) {
		b.Inputs = []LSPProjectInput{{"main.cpp", strings.Repeat("0", 64)}}
	}, func(b *LSPProjectBinding, c *LocalLSPConfig) { c.Policy = "unknown" }, func(b *LSPProjectBinding, c *LocalLSPConfig) { c.ExecutableSHA256 = strings.Repeat("0", 64) }, func(b *LSPProjectBinding, c *LocalLSPConfig) {
		b.Inputs = []LSPProjectInput{{".clangd", strings.Repeat("0", 64)}}
	}} {
		b, c := binding, config
		drift(&b, &c)
		if _, err := PrepareLocalLSPEnrichment(context.Background(), b, c, anchors); err == nil {
			t.Fatal("unqualified configuration/source admitted")
		}
	}
	t.Logf("installed clangd literal observations: commit=%s server=%s selection=%s definitions=%d references=%d; finite fixture only", commit, config.ExecutableSHA256, artifact.SelectionID, len(definition.Locations), len(references.Locations))
}

func TestLSPMalformedLocationsAndUnavailableQualification(t *testing.T) {
	root := t.TempDir()
	contents := map[string]string{"file.cpp": "text"}
	for _, raw := range []string{`{"uri":"` + lspFileURI(filepath.Join(root, "file.cpp")) + `"}`, `{"uri":"` + lspFileURI(filepath.Join(root, "file.cpp")) + `","range":{}}`} {
		if _, err := lspLocations(json.RawMessage(raw), root, contents); err == nil {
			t.Fatal("missing location fields qualified")
		}
	}
	binding := LSPProjectBinding{Repository: "/requested", Commit: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40), PolicyID: "requested-policy", Inputs: []LSPProjectInput{{Path: "file.cpp", SHA256: strings.Repeat("c", 64)}}, MaxProjectBytes: 1024}
	config := LocalLSPConfig{Executable: "/operator/clangd", ExecutableSHA256: strings.Repeat("d", 64), Policy: "unavailable-policy", Limits: lspTestLimits()}
	_, err := PrepareLocalLSPEnrichment(context.Background(), binding, config, []LSPAnchor{{Path: "file.cpp"}})
	refusal, ok := err.(*LSPEnrichmentError)
	if !ok || refusal.Reason != "unavailable-policy" || refusal.Requested == nil || refusal.Requested.Source.Commit != binding.Commit {
		t.Fatalf("refusal loses requested qualification %v", err)
	}
}

func TestLSPExecutableAdmissionNonblockingHandleAndSymlinks(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS native FIFO control")
	}
	root := t.TempDir()
	regular := filepath.Join(root, "real-clangd")
	if err := os.WriteFile(regular, []byte("literal executable bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "clangd")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	body, err := lspReadExecutable(link, 1024)
	if err != nil || string(body) != "literal executable bytes" {
		t.Fatalf("ordinary executable symlink refused: %s %v", body, err)
	}
	fifo := filepath.Join(root, "writerless")
	if output, err := exec.Command("/usr/bin/mkfifo", fifo).CombinedOutput(); err != nil {
		t.Fatalf("native FIFO fixture unavailable: %s %v", output, err)
	}
	fifoLink := filepath.Join(root, "fifo-link")
	if err := os.Symlink(fifo, fifoLink); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fifo, fifoLink} {
		done := make(chan error, 1)
		go func() { _, err := lspReadExecutable(path, 1024); done <- err }()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("FIFO handle admitted")
			}
		case <-time.After(250 * time.Millisecond):
			t.Fatal("FIFO admission blocked without writer")
		}
	}
}
func TestLSPLocationLinkFullRangeAndDiagnosticOrder(t *testing.T) {
	root := t.TempDir()
	uri := lspFileURI(filepath.Join(root, "file.cpp"))
	contents := map[string]string{"file.cpp": "text\nnext"}
	selection := LSPRange{LSPPosition{0, 0}, LSPPosition{0, 1}}
	for _, target := range []LSPRange{{LSPPosition{99, 0}, LSPPosition{100, 0}}, {LSPPosition{0, 3}, LSPPosition{0, 2}}, {LSPPosition{0, 2}, LSPPosition{0, 4}}} {
		raw, _ := json.Marshal(map[string]any{"targetUri": uri, "targetRange": target, "targetSelectionRange": selection})
		if _, err := lspLocations(raw, root, contents); err == nil {
			t.Fatalf("invalid full target range or disjoint selection admitted: %+v", target)
		}
	}
	target := LSPRange{LSPPosition{0, 0}, LSPPosition{1, 4}}
	raw, _ := json.Marshal(map[string]any{"targetUri": uri, "targetRange": target, "targetSelectionRange": selection})
	locations, err := lspLocations(raw, root, contents)
	if err != nil || locations[0].Availability != "qualified" {
		t.Fatalf("valid link refused %+v %v", locations, err)
	}
	if lspValidSourceRange("text", LSPRange{LSPPosition{0, 3}, LSPPosition{0, 1}}) {
		t.Fatal("reversed diagnostic range qualified by shared invariant")
	}
}

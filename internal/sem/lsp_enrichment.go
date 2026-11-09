package sem

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/entireio/entire-graph/internal/gitutil"
)

// LocalLSPConfig admits one existing clangd executable, not arbitrary repository commands.
// The first policy is intentionally macOS-only; unsupported confinement refuses execution.
type LocalLSPConfig struct {
	Executable, ExecutableSHA256, Policy string
	Limits                               LSPLimits
}

// Requested selection is retained on refusal; it is not validated source attribution.
type LSPRequestedSelection struct {
	Limits                            LSPLimits
	Source                            LSPProjectBinding
	ExecutableSHA256, ExecutionPolicy string
	Anchors                           []LSPAnchor
}
type LSPProjectInput struct{ Path, SHA256 string }
type LSPProjectBinding struct {
	Repository, Commit, Tree, PolicyID string
	Inputs                             []LSPProjectInput
	MaxProjectBytes                    int
}
type LSPPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}
type LSPRange struct {
	Start LSPPosition `json:"start"`
	End   LSPPosition `json:"end"`
}

func (p *LSPPosition) UnmarshalJSON(raw []byte) error {
	var value struct {
		Line      *int `json:"line"`
		Character *int `json:"character"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	if value.Line == nil || value.Character == nil || *value.Line < 0 || *value.Character < 0 {
		return fmt.Errorf("missing or negative position")
	}
	p.Line = *value.Line
	p.Character = *value.Character
	return nil
}
func (r *LSPRange) UnmarshalJSON(raw []byte) error {
	var value struct {
		Start *LSPPosition `json:"start"`
		End   *LSPPosition `json:"end"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	if value.Start == nil || value.End == nil {
		return fmt.Errorf("missing native range endpoints")
	}
	r.Start = *value.Start
	r.End = *value.End
	return nil
}

type LSPAnchor struct {
	Path, SHA256, LanguageID, Feature string
	Position                          LSPPosition
	IncludeDeclaration                bool
}
type LSPQualifiedLocation struct {
	OriginalURI          string `json:"original_uri"`
	Path, SHA256         string
	Range                LSPRange `json:"range"`
	Availability, Detail string
}
type LSPObservation struct {
	Anchor               LSPAnchor
	Availability, Detail string
	Raw                  json.RawMessage
	Locations            []LSPQualifiedLocation
}
type LSPDiagnosticObservation struct {
	Path         string
	URI          string
	Version      *int
	Availability string
	Raw          json.RawMessage
}
type LSPEnrichmentArtifact struct {
	Limits                                                                                    LSPLimits
	InitializeResult                                                                          json.RawMessage
	CompilerArguments                                                                         []string
	Format, SelectionID, Repository, Commit, Tree, GitCommonDir, ScanScope, PolicyID          string
	ProjectInputs                                                                             []LSPProjectInput
	ServerSHA256, ConfinementSHA256, ExecutionPolicy, ExecutionPolicySHA256, PositionEncoding string
	ServerInfo                                                                                json.RawMessage
	Observations                                                                              []LSPObservation
	Diagnostics                                                                               []LSPDiagnosticObservation
	Limitations                                                                               []string
}

func lspDigest(body []byte) string { sum := sha256.Sum256(body); return hex.EncodeToString(sum[:]) }
func lspCanonicalPath(path string) bool {
	return path != "" && !strings.ContainsAny(path, "\x00\\") && !filepath.IsAbs(path) && filepath.ToSlash(filepath.Clean(path)) == path && path != "." && path != ".." && !strings.HasPrefix(path, "../")
}
func lspValidatePosition(text string, position LSPPosition) bool {
	if position.Line < 0 || position.Character < 0 || !utf8.ValidString(text) {
		return false
	}
	lines := strings.Split(text, "\n")
	if position.Line >= len(lines) {
		return false
	}
	units := 0
	for _, r := range strings.TrimSuffix(lines[position.Line], "\r") {
		if units == position.Character {
			return true
		}
		if r > 0xffff {
			units += 2
		} else {
			units++
		}
	}
	return units == position.Character
}

// prepare materializes only explicitly admitted, exact committed inputs. It neither
// discovers project configuration nor claims that this finite manifest is a whole project.
func PrepareLocalLSPEnrichment(ctx context.Context, binding LSPProjectBinding, config LocalLSPConfig, anchors []LSPAnchor) (result LSPEnrichmentArtifact, err error) {
	unavailable := func(reason LSPErrorReason, detail string, cause error) (LSPEnrichmentArtifact, error) {
		selection := binding
		selection.Inputs = append([]LSPProjectInput(nil), binding.Inputs...)
		return LSPEnrichmentArtifact{}, &LSPEnrichmentError{Reason: reason, Detail: detail, Cause: cause, Requested: &LSPRequestedSelection{Limits: config.Limits, Source: selection, ExecutableSHA256: config.ExecutableSHA256, ExecutionPolicy: config.Policy, Anchors: append([]LSPAnchor(nil), anchors...)}}
	}
	if !config.Limits.valid() || binding.MaxProjectBytes <= 0 || binding.MaxProjectBytes > 32<<20 || len(binding.Inputs) == 0 || len(binding.Inputs) > 512 || len(anchors) == 0 || len(anchors)+2 > config.Limits.Requests || binding.PolicyID == "" {
		return unavailable("configuration", "invalid finite project/request bounds", nil)
	}
	if runtime.GOOS != "darwin" || config.Policy != "darwin-seatbelt-clangd-v1" {
		return unavailable("unavailable-policy", "no admitted no-egress execution policy on this platform", nil)
	}
	if !filepath.IsAbs(config.Executable) || filepath.Base(config.Executable) != "clangd" {
		return unavailable("configuration", "executable must be operator-selected absolute path", nil)
	}
	executable, err := filepath.EvalSymlinks(config.Executable)
	if err != nil {
		return unavailable("configuration", "executable unavailable", err)
	}
	executableBytes, err := lspReadExecutable(executable, 128<<20)
	if err != nil || len(executableBytes) > 128<<20 || lspDigest(executableBytes) != config.ExecutableSHA256 {
		return unavailable("server-drift", "selected executable digest changed", err)
	}
	sandboxBytes, err := lspReadExecutable("/usr/bin/sandbox-exec", 8<<20)
	if err != nil {
		return unavailable("unavailable-policy", "platform confinement unavailable", err)
	}
	repo, err := filepath.Abs(binding.Repository)
	if err != nil {
		return unavailable("source", "repository unavailable", err)
	}
	repo, err = filepath.EvalSymlinks(repo)
	if err != nil {
		return unavailable("source", "repository unavailable", err)
	}
	sourceCtx, safe := newGitMetadataValidation(ctx, repo)
	if !safe {
		return unavailable("source", "repository metadata refused", gitMetadataRefusalError(repo))
	}
	owner, err := resolveCacheSource(sourceCtx, repo)
	if err != nil {
		return unavailable("source", "repository ownership unavailable", err)
	}
	commit, tree, err := resolvePreparedTarget(sourceCtx, repo, binding.Commit)
	if err != nil || tree != binding.Tree {
		return unavailable("source-drift", "exact committed source differs", err)
	}
	inputs := append([]LSPProjectInput(nil), binding.Inputs...)
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].Path < inputs[j].Path })
	contents := make(map[string]string, len(inputs))
	bytes := 0
	for _, input := range inputs {
		if !lspCanonicalPath(input.Path) || contents[input.Path] != "" {
			return unavailable("source", "noncanonical or duplicate input path", nil)
		}
		if _, exists := contents[input.Path]; exists {
			return unavailable("source", "duplicate input path", nil)
		}
		// Configuration and compilation database execution are never admitted from source.
		name := filepath.Base(input.Path)
		if name == ".clangd" || name == "compile_commands.json" || name == "compile_flags.txt" {
			return unavailable("unsupported-configuration", "repository-selected clangd/driver configuration is unsupported", nil)
		}
		file, readErr := gitutil.ReadFileLimited(sourceCtx, repo, commit, input.Path, int64(binding.MaxProjectBytes-bytes))
		if readErr != nil || file.Status != gitutil.LimitedFileContent || !utf8.ValidString(file.Content) || lspDigest([]byte(file.Content)) != input.SHA256 {
			return unavailable("source-drift", "input bytes unavailable or digest differs", readErr)
		}
		bytes += len(file.Content)
		if bytes > binding.MaxProjectBytes {
			return unavailable("bounds", "project bytes exceeded bound", nil)
		}
		contents[input.Path] = file.Content
	}
	for _, anchor := range anchors {
		text, exists := contents[anchor.Path]
		if !exists || lspDigest([]byte(text)) != anchor.SHA256 || !lspValidatePosition(text, anchor.Position) {
			return unavailable("position", "anchor is outside exact admitted UTF-16 source", nil)
		}
		if (anchor.Feature != "definition" && anchor.Feature != "references") || anchor.LanguageID != "cpp" {
			return unavailable("unsupported-feature", "finite clangd adapter does not admit this requested feature/document capability", nil)
		}
	}
	temporary, err := os.MkdirTemp("", "neuve-lsp-qualified-")
	if err != nil {
		return unavailable("source", "temporary source view unavailable", err)
	}
	defer func() { err = joinLSPCleanup(err, os.RemoveAll(temporary)) }()
	temporary, err = filepath.EvalSymlinks(temporary)
	if err != nil {
		return unavailable("source", "temporary source view canonicalization failed", err)
	}
	project := filepath.Join(temporary, "project")
	home := filepath.Join(temporary, "home")
	if err = os.MkdirAll(home, 0700); err != nil {
		return unavailable("source", "temporary home unavailable", err)
	}
	if err = os.MkdirAll(project, 0700); err != nil {
		return unavailable("source", "temporary project unavailable", err)
	}
	for path, text := range contents {
		target := filepath.Join(project, filepath.FromSlash(path))
		if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return unavailable("source", "materialization failed", err)
		}
		if err = os.WriteFile(target, []byte(text), 0600); err != nil {
			return unavailable("source", "materialization failed", err)
		}
	}
	// A fixed compiler command disallows project drivers, external header discovery,
	// response files and build scripts. Missing toolchain/header context is explicit.
	commands := make([]map[string]any, 0, len(inputs))
	for _, input := range inputs {
		commands = append(commands, map[string]any{"directory": project, "file": filepath.Join(project, input.Path), "arguments": []string{"clang", "-xc++", "-std=c++17", "-nostdinc", "-nostdinc++", filepath.Join(project, input.Path)}})
	}
	commandBytes, _ := json.Marshal(commands)
	if err = os.WriteFile(filepath.Join(project, "compile_commands.json"), commandBytes, 0600); err != nil {
		return unavailable("source", "fixed compiler configuration unavailable", err)
	}
	profile := lspSeatbeltProfile(temporary, executable, repo)
	cmd := exec.Command("/usr/bin/sandbox-exec", "-p", profile, executable, "--background-index=false", "--clang-tidy=false", "--enable-config=false", "--pch-storage=memory", "--log=error", "--compile-commands-dir="+project)
	cmd.Dir = project
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, "TMPDIR=" + temporary, "XDG_CONFIG_HOME=" + home}
	peer, err := startLSPPeer(cmd, config.Limits)
	if err != nil {
		return unavailable("unavailable-policy", "confined peer failed to start", err)
	}
	defer func() {
		err = joinLSPCleanup(err, peer.close())
		if err != nil {
			result = LSPEnrichmentArtifact{}
		}
	}()
	initialized, err := peer.request(ctx, "initialize", map[string]any{"processId": nil, "rootUri": lspFileURI(project), "capabilities": map[string]any{"general": map[string]any{"positionEncodings": []string{"utf-16"}}}, "workspaceFolders": []map[string]string{{"uri": lspFileURI(project), "name": "qualified-project"}}})
	if err != nil {
		return unavailable("transport", "initialization failed", err)
	}
	var handshake struct {
		Capabilities struct {
			PositionEncoding   string          `json:"positionEncoding"`
			DefinitionProvider json.RawMessage `json:"definitionProvider"`
			ReferencesProvider json.RawMessage `json:"referencesProvider"`
		} `json:"capabilities"`
		ServerInfo json.RawMessage `json:"serverInfo"`
	}
	if json.Unmarshal(initialized, &handshake) != nil {
		return unavailable("protocol", "invalid initialize response", nil)
	}
	encoding := handshake.Capabilities.PositionEncoding
	if encoding == "" {
		encoding = "utf-16"
	}
	if encoding != "utf-16" {
		return unavailable("position-encoding", "peer did not negotiate UTF-16", nil)
	}
	if err = peer.notify(ctx, "initialized", map[string]any{}); err != nil {
		return unavailable("transport", "initialized notification failed", err)
	}
	opened := map[string]bool{}
	artifact := LSPEnrichmentArtifact{Format: "local-lsp-enrichment-v1", Limits: config.Limits, InitializeResult: append(json.RawMessage(nil), initialized...), CompilerArguments: []string{"clang", "-xc++", "-std=c++17", "-nostdinc", "-nostdinc++", "<source>"}, Repository: repo, Commit: commit, Tree: tree, GitCommonDir: owner.CommonDir, ScanScope: owner.Scope, PolicyID: binding.PolicyID, ProjectInputs: inputs, ServerSHA256: config.ExecutableSHA256, ConfinementSHA256: lspDigest(sandboxBytes), ExecutionPolicy: config.Policy, ExecutionPolicySHA256: lspDigest([]byte(lspSeatbeltProfile("/qualified-project-view", executable, repo) + ";clang;-xc++;-std=c++17;-nostdinc;-nostdinc++;--background-index=false;--clang-tidy=false;--enable-config=false;--pch-storage=memory;--log=error;HOME=<isolated>;TMPDIR=<isolated>;PATH=/usr/bin:/bin")), PositionEncoding: encoding, ServerInfo: handshake.ServerInfo, Limitations: []string{"Finite admitted input/request set is not whole-project coverage; recompute entire selection on any input/server/config/policy change", "Materialized view retains exact admitted blob bytes, not symlink/submodule/build topology or omitted project inputs", "Fixed C++17 fallback configuration excludes system headers and build-equivalent toolchain context; observations do not establish build validity", "References are contextual observations, never CALLS or proof of exhaustive uses", "Missing or unversioned diagnostic notifications do not establish a clean document", "System runtime/toolchain libraries are not source-qualified inputs: this selection is not a reusable build-equivalent artifact across toolchain/host changes", "Executable digest is checked through a nonblocking regular-file handle before/after execution; protected immutable operator executable placement is required because pathname image swap-and-restoration is not attested"}}
	for _, anchor := range anchors {
		capability := handshake.Capabilities.DefinitionProvider
		if anchor.Feature == "references" {
			capability = handshake.Capabilities.ReferencesProvider
		}
		observation := LSPObservation{Anchor: anchor, Availability: "unavailable"}
		if len(capability) > 0 && string(capability) != "true" && string(capability) != "false" && string(capability) != "null" {
			var options map[string]any
			if json.Unmarshal(capability, &options) != nil || options == nil {
				return unavailable("protocol", "invalid negotiated feature capability", nil)
			}
		}
		if string(capability) == "false" || len(capability) == 0 || string(capability) == "null" {
			observation.Detail = "Peer lacks negotiated feature"
			artifact.Observations = append(artifact.Observations, observation)
			continue
		}
		if !opened[anchor.Path] {
			err = peer.notify(ctx, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": lspFileURI(filepath.Join(project, anchor.Path)), "languageId": anchor.LanguageID, "version": 1, "text": contents[anchor.Path]}})
			if err != nil {
				return unavailable("transport", "document open failed", err)
			}
			opened[anchor.Path] = true
		}
		params := map[string]any{"textDocument": map[string]string{"uri": lspFileURI(filepath.Join(project, anchor.Path))}, "position": anchor.Position}
		if anchor.Feature == "references" {
			params["context"] = map[string]bool{"includeDeclaration": anchor.IncludeDeclaration}
		}
		raw, requestErr := peer.request(ctx, "textDocument/"+anchor.Feature, params)
		if requestErr != nil {
			var featureErr *LSPEnrichmentError
			if errors.As(requestErr, &featureErr) && featureErr.Reason == "feature-error" {
				observation.Raw = featureErr.Raw
				observation.Detail = featureErr.Detail
				artifact.Observations = append(artifact.Observations, observation)
				continue
			}
			return unavailable("transport", "finite feature request incomplete", requestErr)
		}
		observation.Raw = append(json.RawMessage(nil), raw...)
		observation.Availability = "observed"
		locations, locationErr := lspLocations(raw, project, contents)
		if locationErr != nil {
			return unavailable("protocol", "invalid source location result", locationErr)
		}
		observation.Locations = locations
		for _, location := range locations {
			if location.Availability != "qualified" {
				observation.Availability = "partial"
			}
		}
		artifact.Observations = append(artifact.Observations, observation)
	}
	latestDiagnostic := map[string]int{}
	for _, notification := range peer.notifications {
		if notification.Method == "textDocument/publishDiagnostics" {
			var diagnostic struct {
				URI         string          `json:"uri"`
				Version     *int            `json:"version"`
				Diagnostics json.RawMessage `json:"diagnostics"`
			}
			if json.Unmarshal(notification.Params, &diagnostic) != nil || len(diagnostic.Diagnostics) == 0 {
				return unavailable("protocol", "invalid diagnostic notification", nil)
			}
			var entries []struct {
				Range   *LSPRange `json:"range"`
				Message *string   `json:"message"`
			}
			if string(diagnostic.Diagnostics) == "null" || json.Unmarshal(diagnostic.Diagnostics, &entries) != nil {
				return unavailable("protocol", "invalid diagnostic array", nil)
			}
			for _, entry := range entries {
				if entry.Range == nil || entry.Message == nil || !lspPositionAtOrBefore(entry.Range.Start, entry.Range.End) {
					return unavailable("protocol", "invalid diagnostic entry", nil)
				}
			}
			path := lspOriginalPath(diagnostic.URI, project)
			for _, entry := range entries {
				if text, ok := contents[path]; ok && (!lspValidSourceRange(text, *entry.Range)) {
					return unavailable("protocol", "diagnostic range exceeds exact source", nil)
				}
			}
			availability := "unavailable"
			if _, ok := contents[path]; ok && diagnostic.Version != nil && *diagnostic.Version == 1 {
				availability = "version-qualified"
			}
			retained := LSPDiagnosticObservation{Path: path, URI: diagnostic.URI, Version: diagnostic.Version, Availability: availability, Raw: append(json.RawMessage(nil), notification.Params...)}
			if index, seen := latestDiagnostic[diagnostic.URI]; seen {
				artifact.Diagnostics[index] = retained
			} else {
				latestDiagnostic[diagnostic.URI] = len(artifact.Diagnostics)
				artifact.Diagnostics = append(artifact.Diagnostics, retained)
			}
		}
	}
	currentOwner, ownerErr := resolveCacheSource(sourceCtx, repo)
	_, currentTree, sourceErr := resolvePreparedTarget(sourceCtx, repo, commit)
	if ownerErr != nil || sourceErr != nil || currentOwner != owner || currentTree != tree {
		return unavailable("source-drift", "source owner changed during observation", ownerErr)
	}
	currentServer, serverErr := lspReadExecutable(executable, 128<<20)
	currentSandbox, sandboxErr := lspReadExecutable("/usr/bin/sandbox-exec", 8<<20)
	if serverErr != nil || sandboxErr != nil || lspDigest(currentServer) != config.ExecutableSHA256 || lspDigest(currentSandbox) != artifact.ConfinementSHA256 {
		return unavailable("server-drift", "executable/confinement changed during observation", serverErr)
	}
	qualification, _ := json.Marshal(struct {
		Binding         LSPProjectBinding
		Config          LocalLSPConfig
		Anchors         []LSPAnchor
		Owner           cacheSource
		Confinement     string
		ExecutionPolicy string
	}{binding, config, anchors, owner, artifact.ConfinementSHA256, artifact.ExecutionPolicySHA256})
	artifact.SelectionID = lspDigest(qualification)
	return artifact, nil
}
func joinLSPCleanup(original, cleanup error) error {
	if cleanup == nil {
		return original
	}
	if original != nil {
		return fmt.Errorf("%w; cleanup: %v", original, cleanup)
	}
	return lspError("cleanup", "peer/source view finalization failed", cleanup)
}
func lspFileURI(path string) string {
	return localFileURI(path)
}
func lspOriginalPath(uri, root string) string {
	parsed, err := url.Parse(uri)
	rootURI, rootErr := url.Parse(localFileURI(root))
	if err != nil || rootErr != nil || parsed.Scheme != "file" || parsed.User != nil || parsed.Opaque != "" ||
		!strings.EqualFold(parsed.Host, rootURI.Host) || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return ""
	}
	// Refuse traversal before path cleaning or filesystem normalization.
	for _, component := range strings.Split(strings.ReplaceAll(parsed.Path, "\\", "/"), "/") {
		if component == ".." {
			return ""
		}
	}
	target, err := lspNormalizeURIPath(parsed)
	if err != nil {
		return ""
	}
	normalRoot, err := lspNormalizeURIPath(rootURI)
	if err != nil {
		return ""
	}
	path, err := filepath.Rel(normalRoot, target)
	if err != nil || !lspCanonicalPath(filepath.ToSlash(path)) {
		return ""
	}
	return filepath.ToSlash(path)
}

func lspLocations(raw json.RawMessage, root string, contents map[string]string) ([]LSPQualifiedLocation, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	var list []json.RawMessage
	if len(raw) > 0 && raw[0] == '[' {
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, err
		}
	} else {
		list = []json.RawMessage{raw}
	}
	out := make([]LSPQualifiedLocation, 0, len(list))
	for _, item := range list {
		var location struct {
			URI                  string    `json:"uri"`
			Range                *LSPRange `json:"range"`
			TargetURI            string    `json:"targetUri"`
			TargetRange          *LSPRange `json:"targetRange"`
			TargetSelectionRange *LSPRange `json:"targetSelectionRange"`
		}
		if err := json.Unmarshal(item, &location); err != nil {
			return nil, err
		}
		uri := location.URI
		r := location.Range
		if uri == "" {
			uri = location.TargetURI
			r = location.TargetSelectionRange
		}
		if uri == "" || r == nil || (location.URI == "" && location.TargetRange == nil) {
			return nil, fmt.Errorf("missing location URI/range")
		}
		if !lspPositionAtOrBefore(r.Start, r.End) || (location.URI == "" && (!lspPositionAtOrBefore(location.TargetRange.Start, location.TargetRange.End) || !lspRangeContains(*location.TargetRange, *r))) {
			return nil, fmt.Errorf("reversed range or disjoint target selection")
		}
		path := lspOriginalPath(uri, root)
		qualified := LSPQualifiedLocation{OriginalURI: uri, Path: path, Range: *r, Availability: "unavailable", Detail: "Target is outside admitted exact source"}
		if text, ok := contents[path]; ok {
			if !lspValidSourceRange(text, *r) {
				return nil, fmt.Errorf("invalid exact-source range")
			}
			if location.URI == "" && (!lspValidSourceRange(text, *location.TargetRange) || !lspRangeContains(*location.TargetRange, *r)) {
				return nil, fmt.Errorf("invalid exact-source target range or selection containment")
			}
			qualified.SHA256 = lspDigest([]byte(text))
			qualified.Availability = "qualified"
			qualified.Detail = ""
		}
		out = append(out, qualified)
	}
	return out, nil
}
func lspSeatbeltProfile(root, executable, originalRepository string) string {
	quote := func(s string) string { return fmt.Sprintf("%q", s) }
	// No general sandbox service: this fixed local clangd policy denies sockets,
	// child execution, source reads outside the admitted view/system runtime and writes outside scratch.
	allowed := []string{root, "/usr/lib", "/usr/share", "/System", "/private/preboot", "/private/var/db", "/dev", "/Library/Developer", "/Applications/Xcode.app"}
	exclusions := []string{}
	for _, path := range allowed {
		exclusions = append(exclusions, "(require-not (literal \"/\"))", "(require-not (subpath "+quote(path)+"))")
	}
	exclusions = append(exclusions, "(require-not (literal \"/\"))", "(require-not (literal "+quote(executable)+"))", "(require-not (literal \"/dev/null\"))", "(require-not (literal \"/dev/urandom\"))")
	return "(version 1)(allow default)(deny file-read-data (subpath " + quote(originalRepository) + "))(deny network*)(deny process-fork)(deny process-exec (require-not (literal " + quote(executable) + ")))(deny file-read-data (require-all " + strings.Join(exclusions, " ") + "))(deny file-write* (subpath " + quote(filepath.Join(root, "project")) + "))(deny file-write* (require-not (subpath " + quote(root) + ")))"
}

func lspReadExecutable(path string, limit int64) ([]byte, error) {
	file, err := openBoundedRegularFile(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	regular, regularErr := openedFileIsRegular(file, info)
	if regularErr != nil {
		return nil, regularErr
	}
	if !regular || info.Size() > limit {
		return nil, fmt.Errorf("executable is not bounded regular file")
	}
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("executable exceeded byte bound")
	}
	return body, err
}

func lspPositionAtOrBefore(a, b LSPPosition) bool {
	return a.Line < b.Line || (a.Line == b.Line && a.Character <= b.Character)
}
func lspRangeContains(outer, inner LSPRange) bool {
	return lspPositionAtOrBefore(outer.Start, inner.Start) && lspPositionAtOrBefore(inner.End, outer.End)
}
func lspValidSourceRange(text string, r LSPRange) bool {
	return lspValidatePosition(text, r.Start) && lspValidatePosition(text, r.End) && lspPositionAtOrBefore(r.Start, r.End)
}

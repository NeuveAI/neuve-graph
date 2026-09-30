package sem

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func projectionCandidate(path, symbol string, start int, score float64) searchCandidate {
	return searchCandidate{
		score: score,
		result: SearchResult{
			FilePath: path, StartLine: start, EndLine: start,
			FocusLine: start, SnippetStartLine: start, SnippetEndLine: start,
			Kind: "function", SymbolID: path + ":" + symbol,
			SymbolName: symbol, QualifiedName: symbol, Snippet: symbol,
		},
	}
}

func candidateIdentityPrefix(candidates []searchCandidate) []string {
	out := make([]string, len(candidates))
	for index, candidate := range candidates {
		out[index] = searchCandidateLocationKey(candidate)
	}
	return out
}

func TestOrdinarySelectionProjectsOneStableRanking(t *testing.T) {
	var candidates []searchCandidate
	for index := 0; index < 80; index++ {
		path := fmt.Sprintf("src/unit%02d.go", index%20)
		candidates = append(candidates, projectionCandidate(path, fmt.Sprintf("Unit%02d", index), index+1, float64(200-index)))
	}
	at64 := selectDiverseCandidates(candidates, 64, 3)
	for _, topK := range []int{1, 2, 8, 16} {
		got := selectDiverseCandidates(candidates, topK, 3)
		if !reflect.DeepEqual(candidateIdentityPrefix(got), candidateIdentityPrefix(at64[:topK])) {
			t.Fatalf("ordinary top-k %d changed ranking prefix", topK)
		}
	}
}

func TestExpansionSeedHorizonKeepsRelevantNeighborsAndBridgesBeyondOldFloors(t *testing.T) {
	q := buildSearchQuery("bridge transition")
	readers := map[string]string{}
	symbolsByID := map[string]SymbolRecord{}
	symbolsByFile := map[string][]SymbolRecord{}
	var seeds []searchCandidate
	for index := 0; index < 381; index++ {
		path := fmt.Sprintf("src/seed%02d.go", index)
		symbol := SymbolRecord{ID: fmt.Sprintf("seed:%02d", index), Kind: "function", Name: fmt.Sprintf("Seed%02d", index), QualifiedName: fmt.Sprintf("Seed%02d", index), FilePath: path, StartLine: 1, EndLine: 1, ContainerID: "root"}
		seeds = append(seeds, projectionCandidate(path, symbol.Name, 1, 100))
		seeds[len(seeds)-1].result.SymbolID = symbol.ID
		symbolsByID[symbol.ID] = symbol
		symbolsByFile[path] = []SymbolRecord{symbol}
		readers[path] = "func " + symbol.Name + "() {}\n"
	}

	neighborSeed := SymbolRecord{ID: "late:seed", Kind: "function", Name: "LateSeed", QualifiedName: "LateSeed", FilePath: "src/late_neighbor.go", StartLine: 1, EndLine: 1, ContainerID: "late"}
	neighbor := SymbolRecord{ID: "late:neighbor", Kind: "function", Name: "LateNeighbor", QualifiedName: "LateNeighbor", FilePath: neighborSeed.FilePath, StartLine: 3, EndLine: 3, ContainerID: "late"}
	lateSeed := projectionCandidate(neighborSeed.FilePath, neighborSeed.Name, 1, 100)
	lateSeed.result.SymbolID = neighborSeed.ID
	seeds = append(seeds, lateSeed)
	symbolsByID[neighborSeed.ID] = neighborSeed
	symbolsByFile[neighborSeed.FilePath] = []SymbolRecord{neighborSeed, neighbor}
	readers[neighborSeed.FilePath] = "func LateSeed() {}\n\nfunc LateNeighbor() {}\n"

	bridgeLeft := SymbolRecord{ID: "bridge:left", Kind: "function", Name: "BridgeLeft", QualifiedName: "BridgeLeft", FilePath: "src/late_bridge.go", StartLine: 1, EndLine: 1, ContainerID: "bridge"}
	bridgeRight := SymbolRecord{ID: "bridge:right", Kind: "function", Name: "BridgeRight", QualifiedName: "BridgeRight", FilePath: bridgeLeft.FilePath, StartLine: 4, EndLine: 4, ContainerID: "bridge"}
	for _, symbol := range []SymbolRecord{bridgeLeft, bridgeRight} {
		seed := projectionCandidate(symbol.FilePath, symbol.Name, symbol.StartLine, 100)
		seed.result.SymbolID = symbol.ID
		seeds = append(seeds, seed)
		symbolsByID[symbol.ID] = symbol
	}
	symbolsByFile[bridgeLeft.FilePath] = []SymbolRecord{bridgeLeft, bridgeRight}
	readers[bridgeLeft.FilePath] = "func BridgeLeft() {}\n// bridge transition\n\nfunc BridgeRight() {}\n"

	read := func(path string) (string, bool) { value, ok := readers[path]; return value, ok }
	options := SearchOptions{ContextLines: 1, MaxSnippetLines: 6, MaxRegionLines: 80}
	neighbors := expandSameContainerNeighborCandidates(t.Context(), seeds, q, symbolsByID, symbolsByFile, read, nil, options)
	foundNeighbor := false
	for _, candidate := range neighbors {
		foundNeighbor = foundNeighbor || candidate.result.SymbolName == "LateNeighbor"
	}
	if !foundNeighbor {
		t.Fatalf("neighbor beyond the old 30-seed floor was lost: %#v", neighbors)
	}
	bridges := expandSameFileBridgeCandidates(t.Context(), seeds, q, symbolsByFile, read, nil, options)
	foundBridge := false
	for _, candidate := range bridges {
		foundBridge = foundBridge || (candidate.result.FilePath == bridgeLeft.FilePath && containsString(candidate.result.Signals, "same-file-bridge"))
	}
	if !foundBridge {
		t.Fatalf("bridge beyond the old 40-seed floor was lost: %#v", bridges)
	}
}

func denseNeighborCandidates(count int) ([]searchCandidate, map[string]SymbolRecord, map[string][]SymbolRecord, contentReader) {
	const path = "src/dense_neighbor.go"
	candidates := make([]searchCandidate, 0, count)
	symbolsByID := make(map[string]SymbolRecord, count*2)
	symbols := make([]SymbolRecord, 0, count*2)
	var content strings.Builder
	for index := 0; index < count; index++ {
		seedLine := index*4 + 1
		seed := SymbolRecord{
			ID: fmt.Sprintf("neighbor-seed:%03d", index), Kind: "function", Name: fmt.Sprintf("Seed%03d", index),
			QualifiedName: fmt.Sprintf("Seed%03d", index), FilePath: path,
			StartLine: seedLine, EndLine: seedLine, ContainerID: "dense",
		}
		neighbor := SymbolRecord{
			ID: fmt.Sprintf("neighbor-stage:%03d", index), Kind: "function", Name: fmt.Sprintf("Stage%03d", index),
			QualifiedName: fmt.Sprintf("Stage%03d", index), FilePath: path,
			StartLine: seedLine + 2, EndLine: seedLine + 2, ContainerID: "dense",
		}
		candidate := projectionCandidate(path, seed.Name, seed.StartLine, 100)
		candidate.result.SymbolID = seed.ID
		candidates = append(candidates, candidate)
		for _, symbol := range []SymbolRecord{seed, neighbor} {
			symbolsByID[symbol.ID] = symbol
			symbols = append(symbols, symbol)
		}
		fmt.Fprintf(&content, "func Seed%03d() {}\n// transition\nfunc Stage%03d() {}\n\n", index, index)
	}
	read := func(filePath string) (string, bool) { return content.String(), filePath == path }
	return candidates, symbolsByID, map[string][]SymbolRecord{path: symbols}, read
}

func TestSameContainerNeighborConstructionBoundsDenseSeedTopology(t *testing.T) {
	seeds, symbolsByID, symbolsByFile, read := denseNeighborCandidates(searchExpansionMaxSeedCandidates)
	neighbors, materialized := expandSameContainerNeighborCandidatesBounded(
		t.Context(), seeds, buildSearchQuery("transition"), symbolsByID, symbolsByFile, read, nil,
		SearchOptions{ContextLines: 1, MaxSnippetLines: defaultSearchMaxSnippetLines},
	)
	if materialized > searchNeighborEvidenceHorizon {
		t.Fatalf("materialized neighbor descriptors = %d, fixed evidence horizon is %d", materialized, searchNeighborEvidenceHorizon)
	}
	if len(neighbors) != searchNeighborOutputLimit {
		t.Fatalf("dense neighbor output = %d, want bounded full output %d", len(neighbors), searchNeighborOutputLimit)
	}
}

func TestSameContainerNeighborEvidenceHorizonCountsDistinctPublicIdentities(t *testing.T) {
	const path = "src/duplicate_neighbor.go"
	highSeed := SymbolRecord{ID: "high-seed", Kind: "function", Name: "HighSeed", QualifiedName: "HighSeed", FilePath: path, StartLine: 1, EndLine: 1, ContainerID: "pipeline"}
	highNeighbor := SymbolRecord{ID: "high-neighbor", Kind: "function", Name: "HighNeighbor", QualifiedName: "HighNeighbor", FilePath: path, StartLine: 3, EndLine: 3, ContainerID: "pipeline"}
	laterSeed := SymbolRecord{ID: "later-seed", Kind: "function", Name: "LaterSeed", QualifiedName: "LaterSeed", FilePath: path, StartLine: 9, EndLine: 9, ContainerID: "pipeline"}
	laterNeighbor := SymbolRecord{ID: "later-neighbor", Kind: "function", Name: "LaterNeighbor", QualifiedName: "LaterNeighbor", FilePath: path, StartLine: 11, EndLine: 11, ContainerID: "pipeline"}
	symbols := []SymbolRecord{highSeed, highNeighbor, laterSeed, laterNeighbor}
	symbolsByID := map[string]SymbolRecord{}
	for _, symbol := range symbols {
		symbolsByID[symbol.ID] = symbol
	}
	var seeds []searchCandidate
	for index := 0; index < searchNeighborEvidenceHorizon+8; index++ {
		seed := projectionCandidate(path, highSeed.Name, highSeed.StartLine, 100)
		seed.result.SymbolID = highSeed.ID
		seeds = append(seeds, seed)
	}
	later := projectionCandidate(path, laterSeed.Name, laterSeed.StartLine, 40)
	later.result.SymbolID = laterSeed.ID
	seeds = append(seeds, later)
	content := "func HighSeed() {}\n// transition\nfunc HighNeighbor() {}\n\n\n\n\n\nfunc LaterSeed() {}\n// transition\nfunc LaterNeighbor() {}\n"
	read := func(filePath string) (string, bool) { return content, filePath == path }

	neighbors, _ := expandSameContainerNeighborCandidatesBounded(
		t.Context(), seeds, buildSearchQuery("transition"), symbolsByID,
		map[string][]SymbolRecord{path: symbols}, read, nil,
		SearchOptions{ContextLines: 1, MaxSnippetLines: defaultSearchMaxSnippetLines},
	)
	foundLater := false
	for _, neighbor := range neighbors {
		foundLater = foundLater || neighbor.result.SymbolName == laterNeighbor.Name
	}
	if !foundLater {
		t.Fatalf("duplicate descriptors exhausted the distinct neighbor horizon: %#v", candidateIdentityPrefix(neighbors))
	}
}

func BenchmarkExpansionAtMaximumSeedHorizon(b *testing.B) {
	q := buildSearchQuery("bridge transition")
	readers := map[string]string{}
	symbolsByID := map[string]SymbolRecord{}
	symbolsByFile := map[string][]SymbolRecord{}
	seeds := make([]searchCandidate, 0, searchExpansionMaxSeedCandidates)
	for file := 0; file < searchExpansionMaxSeedCandidates/2; file++ {
		path := fmt.Sprintf("src/unit%03d.go", file)
		left := SymbolRecord{ID: fmt.Sprintf("unit:%03d:left", file), Kind: "function", Name: fmt.Sprintf("Left%03d", file), QualifiedName: fmt.Sprintf("Left%03d", file), FilePath: path, StartLine: 1, EndLine: 1, ContainerID: path}
		right := SymbolRecord{ID: fmt.Sprintf("unit:%03d:right", file), Kind: "function", Name: fmt.Sprintf("Right%03d", file), QualifiedName: fmt.Sprintf("Right%03d", file), FilePath: path, StartLine: 3, EndLine: 3, ContainerID: path}
		for _, symbol := range []SymbolRecord{left, right} {
			candidate := projectionCandidate(path, symbol.Name, symbol.StartLine, 100)
			candidate.result.SymbolID = symbol.ID
			seeds = append(seeds, candidate)
			symbolsByID[symbol.ID] = symbol
		}
		symbolsByFile[path] = []SymbolRecord{left, right}
		readers[path] = "func left() {}\n// bridge transition\nfunc right() {}\n"
	}
	read := func(path string) (string, bool) { value, ok := readers[path]; return value, ok }
	options := SearchOptions{ContextLines: 1, MaxSnippetLines: 6, MaxRegionLines: 80}
	b.Run("distributed_192_files/same_container", func(b *testing.B) {
		b.ReportAllocs()
		for iteration := 0; iteration < b.N; iteration++ {
			_ = expandSameContainerNeighborCandidates(context.Background(), seeds, q, symbolsByID, symbolsByFile, read, nil, options)
		}
	})
	b.Run("distributed_192_files/same_file_bridge", func(b *testing.B) {
		b.ReportAllocs()
		for iteration := 0; iteration < b.N; iteration++ {
			_ = expandSameFileBridgeCandidates(context.Background(), seeds, q, symbolsByFile, read, nil, options)
		}
	})

	const densePath = "src/dense.go"
	denseIDs := map[string]SymbolRecord{}
	denseByFile := map[string][]SymbolRecord{densePath: {}}
	denseSeeds := make([]searchCandidate, 0, searchExpansionMaxSeedCandidates)
	var denseContent strings.Builder
	for index := 0; index < searchExpansionMaxSeedCandidates; index++ {
		line := index + 1
		symbol := SymbolRecord{
			ID: fmt.Sprintf("dense:%03d", index), Kind: "function", Name: fmt.Sprintf("Dense%03d", index),
			QualifiedName: fmt.Sprintf("Dense%03d", index), FilePath: densePath,
			StartLine: line, EndLine: line, ContainerID: "dense-container",
		}
		candidate := projectionCandidate(densePath, symbol.Name, line, 100)
		candidate.result.SymbolID = symbol.ID
		denseSeeds = append(denseSeeds, candidate)
		denseIDs[symbol.ID] = symbol
		denseByFile[densePath] = append(denseByFile[densePath], symbol)
		fmt.Fprintf(&denseContent, "func Dense%03d() {} // bridge transition\n", index)
	}
	denseRead := func(path string) (string, bool) {
		if path != densePath {
			return "", false
		}
		return denseContent.String(), true
	}
	denseOptions := SearchOptions{ContextLines: 1, MaxSnippetLines: searchExpansionMaxSeedCandidates, MaxRegionLines: searchExpansionMaxSeedCandidates}
	b.Run("dense_one_file_one_container/same_container", func(b *testing.B) {
		b.ReportAllocs()
		for iteration := 0; iteration < b.N; iteration++ {
			_ = expandSameContainerNeighborCandidates(context.Background(), denseSeeds, q, denseIDs, denseByFile, denseRead, nil, denseOptions)
		}
	})
	b.Run("dense_one_file_one_container/same_file_bridge", func(b *testing.B) {
		b.ReportAllocs()
		for iteration := 0; iteration < b.N; iteration++ {
			_ = expandSameFileBridgeCandidates(context.Background(), denseSeeds, q, denseByFile, denseRead, nil, denseOptions)
		}
	})
}

func denseBridgeCandidates(count int) ([]searchCandidate, map[string][]SymbolRecord, contentReader) {
	const path = "src/dense_bridge.go"
	candidates := make([]searchCandidate, 0, count)
	symbols := make([]SymbolRecord, 0, count)
	var content strings.Builder
	for index := 0; index < count; index++ {
		line := index*2 + 1
		symbol := SymbolRecord{
			ID: fmt.Sprintf("bridge:%03d", index), Kind: "function", Name: fmt.Sprintf("Bridge%03d", index),
			QualifiedName: fmt.Sprintf("Bridge%03d", index), FilePath: path,
			StartLine: line, EndLine: line, ContainerID: "dense",
		}
		candidate := projectionCandidate(path, symbol.Name, line, 100)
		candidate.result.SymbolID = symbol.ID
		candidates = append(candidates, candidate)
		symbols = append(symbols, symbol)
		fmt.Fprintf(&content, "func Bridge%03d() {}\n// bridge transition\n", index)
	}
	read := func(filePath string) (string, bool) {
		return content.String(), filePath == path
	}
	return candidates, map[string][]SymbolRecord{path: symbols}, read
}

func TestSameFileBridgeConstructionBoundsDenseSeedTopology(t *testing.T) {
	seeds, symbolsByFile, read := denseBridgeCandidates(searchExpansionMaxSeedCandidates)
	bridges, materialized := expandSameFileBridgeCandidatesBounded(
		t.Context(), seeds, buildSearchQuery("bridge transition"), symbolsByFile, read, nil,
		SearchOptions{MaxSnippetLines: defaultSearchMaxSnippetLines},
	)
	if materialized > searchBridgePairEvidenceHorizon {
		t.Fatalf("materialized bridge pairs = %d, fixed evidence horizon is %d", materialized, searchBridgePairEvidenceHorizon)
	}
	if len(bridges) != searchBridgeOutputLimit {
		t.Fatalf("dense bridge output = %d, want bounded full output %d", len(bridges), searchBridgeOutputLimit)
	}
}

func TestSameFileBridgeEvidenceHorizonCountsDistinctPublicIdentities(t *testing.T) {
	const path = "src/duplicate_bridge.go"
	var candidates []searchCandidate
	var symbols []SymbolRecord
	addRepresentations := func(prefix string, startLine, endLine, count int, score float64) {
		for index := 0; index < count; index++ {
			symbol := SymbolRecord{
				ID:            fmt.Sprintf("%s:%02d", prefix, index),
				Kind:          "function",
				Name:          fmt.Sprintf("%s%02d", prefix, index),
				QualifiedName: fmt.Sprintf("%s%02d", prefix, index),
				FilePath:      path,
				StartLine:     startLine,
				EndLine:       endLine,
				ContainerID:   "pipeline",
			}
			candidate := projectionCandidate(path, symbol.Name, startLine, score)
			candidate.result.EndLine = endLine
			candidate.result.SymbolID = symbol.ID
			candidates = append(candidates, candidate)
			symbols = append(symbols, symbol)
		}
	}

	// Twelve equivalent representations on each side create 144 higher-scored
	// descriptors for one public bridge identity. They must not consume the
	// evidence horizon before the later distinct bridge is considered.
	addRepresentations("FirstLeft", 1, 1, 12, 100)
	addRepresentations("FirstRight", 3, 3, 12, 100)
	addRepresentations("LaterLeft", 9, 9, 1, 40)
	addRepresentations("LaterRight", 11, 11, 1, 40)
	sortSearchCandidates(candidates)
	content := strings.Join([]string{
		"func FirstLeft() {}",
		"// bridge transition",
		"func FirstRight() {}",
		"", "", "", "", "",
		"func LaterLeft() {}",
		"// bridge transition",
		"func LaterRight() {}",
	}, "\n") + "\n"
	read := func(filePath string) (string, bool) { return content, filePath == path }

	bridges, _ := expandSameFileBridgeCandidatesBounded(
		t.Context(), candidates, buildSearchQuery("bridge transition"),
		map[string][]SymbolRecord{path: symbols}, read, nil,
		SearchOptions{MaxSnippetLines: defaultSearchMaxSnippetLines},
	)
	foundLater := false
	for _, bridge := range bridges {
		foundLater = foundLater || (bridge.result.FilePath == path && bridge.result.StartLine == 10 && bridge.result.EndLine == 11)
	}
	if !foundLater {
		t.Fatalf("duplicate descriptors exhausted the distinct bridge horizon: %#v", candidateIdentityPrefix(bridges))
	}
}

func TestSameFileBridgeConstructionRanksLateStrongEvidenceBeforeEarlyPairs(t *testing.T) {
	const path = "src/merit_bridge.go"
	var candidates []searchCandidate
	var symbols []SymbolRecord
	var content strings.Builder
	linesWritten := 0
	for group := 0; group < searchBridgeOutputLimit+2; group++ {
		start := group*10 + 1
		score := 10.0
		if group == searchBridgeOutputLimit+1 {
			score = 20
		}
		for offset, name := range []string{"Left", "Right"} {
			line := start + offset*2
			symbol := SymbolRecord{
				ID: fmt.Sprintf("group:%02d:%s", group, name), Kind: "function",
				Name: fmt.Sprintf("%s%02d", name, group), QualifiedName: fmt.Sprintf("%s%02d", name, group),
				FilePath: path, StartLine: line, EndLine: line, ContainerID: fmt.Sprintf("group:%02d", group),
			}
			candidate := projectionCandidate(path, symbol.Name, line, score)
			candidate.result.SymbolID = symbol.ID
			candidates = append(candidates, candidate)
			symbols = append(symbols, symbol)
		}
		for linesWritten < start-1 {
			content.WriteString("\n")
			linesWritten++
		}
		fmt.Fprintf(&content, "func Left%02d() {}\n", group)
		if group == 0 {
			content.WriteString("// lexical decoy\n")
		} else {
			content.WriteString("// bridge transition\n")
		}
		fmt.Fprintf(&content, "func Right%02d() {}\n", group)
		linesWritten += 3
	}
	sortSearchCandidates(candidates)
	read := func(filePath string) (string, bool) { return content.String(), filePath == path }
	bridges := expandSameFileBridgeCandidates(
		t.Context(), candidates, buildSearchQuery("bridge transition"),
		map[string][]SymbolRecord{path: symbols}, read, nil,
		SearchOptions{MaxSnippetLines: defaultSearchMaxSnippetLines},
	)
	lateLine := (searchBridgeOutputLimit+1)*10 + 2
	foundLate := false
	for _, bridge := range bridges {
		foundLate = foundLate || bridge.result.FocusLine == lateLine
	}
	if !foundLate {
		t.Fatalf("late strong bridge was displaced by early source order: %#v", candidateIdentityPrefix(bridges))
	}
}

func TestSameFileBridgeIdentityIgnoresLargerPresentationWidth(t *testing.T) {
	seeds, symbolsByFile, read := denseBridgeCandidates(32)
	q := buildSearchQuery("bridge transition")
	base := expandSameFileBridgeCandidates(
		t.Context(), seeds, q, symbolsByFile, read, nil,
		SearchOptions{MaxSnippetLines: defaultSearchMaxSnippetLines},
	)
	wide := expandSameFileBridgeCandidates(
		t.Context(), seeds, q, symbolsByFile, read, nil,
		SearchOptions{MaxSnippetLines: searchExpansionMaxSeedCandidates},
	)
	if !reflect.DeepEqual(candidateIdentityPrefix(base), candidateIdentityPrefix(wide)) {
		t.Fatalf("larger presentation width changed bridge identities: %#v -> %#v",
			candidateIdentityPrefix(base), candidateIdentityPrefix(wide))
	}
}

func TestExpansionSeedHorizonBoundsAnEqualScoreAdversarialTail(t *testing.T) {
	candidates := make([]searchCandidate, 10_000)
	for index := range candidates {
		candidates[index] = projectionCandidate(
			fmt.Sprintf("src/adversarial%05d.go", index), fmt.Sprintf("Candidate%05d", index), 1, 100,
		)
	}
	if got := searchExpansionSeedHorizon(candidates, 0.35); got != searchExpansionMaxSeedCandidates {
		t.Fatalf("equal-score seed horizon = %d, want work ceiling %d", got, searchExpansionMaxSeedCandidates)
	}
}

func TestDeepSelectionProjectsOneWeightedRanking(t *testing.T) {
	semantic := make([]searchCandidate, 70)
	sparse := make([]searchCandidate, 70)
	for index := range semantic {
		semantic[index] = projectionCandidate(fmt.Sprintf("semantic/%02d.go", index), fmt.Sprintf("Semantic%02d", index), 1, float64(200-index))
		sparse[index] = projectionCandidate(fmt.Sprintf("sparse/%02d.go", index), fmt.Sprintf("Sparse%02d", index), 1, float64(100-index)/10)
	}
	at64 := selectHybridCandidates(semantic, sparse, 64)
	for _, topK := range []int{8, 16} {
		got := selectHybridCandidates(semantic, sparse, topK)
		if !reflect.DeepEqual(candidateIdentityPrefix(got), candidateIdentityPrefix(at64[:topK])) {
			t.Fatalf("deep top-k %d changed weighted prefix", topK)
		}
	}
	wantDeepSparse := searchCandidateLocationKey(sparse[39])
	if !containsString(candidateIdentityPrefix(at64), wantDeepSparse) {
		t.Fatalf("deep sparse candidate formerly outside the shallow input was lost: %s", wantDeepSparse)
	}
}

func TestDeepSelectionKeepsFiveEighthsSparseAtBoundaries(t *testing.T) {
	semantic := make([]searchCandidate, 40)
	sparse := make([]searchCandidate, 80)
	for index := range semantic {
		semantic[index] = projectionCandidate(fmt.Sprintf("semantic/%02d.go", index), fmt.Sprintf("Semantic%02d", index), 1, float64(200-index))
	}
	for index := range sparse {
		sparse[index] = projectionCandidate(fmt.Sprintf("sparse/%02d.go", index), fmt.Sprintf("Sparse%02d", index), 1, float64(100-index))
	}
	for _, topK := range []int{8, 16, 24, 32} {
		selected := selectHybridCandidates(semantic, sparse, topK)
		sparseCount := 0
		for _, candidate := range selected {
			if strings.HasPrefix(candidate.result.FilePath, "sparse/") {
				sparseCount++
			}
		}
		want := topK * deepSearchSparseNumerator / deepSearchSparseDenominator
		if sparseCount != want {
			t.Fatalf("top-k %d sparse share = %d, want %d", topK, sparseCount, want)
		}
	}
}

func TestDeepSelectionKeepsStrongRepeatedFileAheadOfWeakUnseenFile(t *testing.T) {
	semantic := []searchCandidate{
		projectionCandidate("shared/service.go", "Anchor", 1, 100),
		projectionCandidate("semantic/other.go", "Other", 1, 90),
	}
	sparse := []searchCandidate{
		projectionCandidate("shared/service.go", "StrongBody", 50, 100),
		projectionCandidate("weak/unseen.go", "WeakBody", 1, 1),
	}
	selected := selectHybridCandidates(semantic, sparse, 3)
	if len(selected) < 2 || selected[1].result.SymbolName != "StrongBody" {
		t.Fatalf("weak unseen-file diversity displaced strong repeated-file evidence: %#v", candidateIdentityPrefix(selected))
	}
}

func TestSearchCandidateCoverageQueuePrefersUnseenFilesWithinStableEvidenceBands(t *testing.T) {
	candidates := []searchCandidate{
		projectionCandidate("shared/service.go", "StrongRepeat", 1, 100),
		projectionCandidate("shared/service.go", "BandRepeat", 20, 20),
		projectionCandidate("unseen/service.go", "BandUnseen", 1, 19),
		projectionCandidate("shared/other.go", "BandTail", 1, 18),
	}
	queue := newSearchCandidateCoverageQueue(candidates)
	selectedFiles := map[string]bool{"shared/service.go": true}
	selectedLocations := map[string]bool{}

	first, ok := queue.pop(selectedFiles, selectedLocations)
	if !ok || first.result.SymbolName != "StrongRepeat" {
		t.Fatalf("unseen evidence across the score cliff displaced the strong repeat: %#v", first)
	}
	selectedLocations[searchCandidateLocationKey(first)] = true
	second, ok := queue.pop(selectedFiles, selectedLocations)
	if !ok || second.result.SymbolName != "BandUnseen" {
		t.Fatalf("same-band unseen evidence did not outrank the repeated file: %#v", second)
	}
}

func TestDeepSelectionBoundsVeryLargeSparsePopulation(t *testing.T) {
	semantic := []searchCandidate{projectionCandidate("semantic/anchor.go", "Anchor", 1, 100)}
	sparse := make([]searchCandidate, 100_000)
	for index := range sparse {
		sparse[index] = projectionCandidate(fmt.Sprintf("sparse/%06d.go", index), fmt.Sprintf("Sparse%06d", index), 1, float64(100_000-index))
	}
	selected := selectHybridCandidates(semantic, sparse, 64)
	if len(selected) != 64 {
		t.Fatalf("large sparse population selected %d rows, want 64", len(selected))
	}
	for _, candidate := range selected {
		if candidate.result.SymbolName == fmt.Sprintf("Sparse%06d", deepSearchSparseEvidenceHorizon) {
			t.Fatalf("candidate beyond sparse evidence horizon entered selection: %s", candidate.result.SymbolName)
		}
	}
}

func TestProseSelectionKeepsSourceRegionsBoundedAndDeduplicated(t *testing.T) {
	q := buildSearchQuery("stable retrieval")
	var candidates []searchCandidate
	for index := 0; index < 12; index++ {
		candidate := projectionCandidate("src/service.go", fmt.Sprintf("Operation%02d", index), index+1, float64(100-index))
		candidates = append(candidates, candidate)
	}
	candidates = append(candidates, candidates[1])
	for index := 0; index < 50; index++ {
		candidate := projectionCandidate(fmt.Sprintf("docs/note%02d.md", index), fmt.Sprintf("Note%02d", index), 1, float64(50-index))
		candidate.result.Kind = "section"
		candidates = append(candidates, candidate)
	}
	selected := selectSearchCandidates(candidates, q, 16, 3, true)
	seen := map[string]bool{}
	sourceCount := 0
	for _, candidate := range selected {
		key := searchCandidateLocationKey(candidate)
		if seen[key] {
			t.Fatalf("same source location selected twice: %s", key)
		}
		seen[key] = true
		if candidate.result.FilePath == "src/service.go" {
			sourceCount++
		}
	}
	if sourceCount != 3 {
		t.Fatalf("source regions selected = %d, want MaxRegionsPerFile 3", sourceCount)
	}

	codeOnly := candidates[:12]
	want := selectDiverseCandidates(codeOnly, 8, 3)
	got := selectSearchCandidates(codeOnly, q, 8, 3, true)
	if !reflect.DeepEqual(candidateIdentityPrefix(got), candidateIdentityPrefix(want)) {
		t.Fatalf("code-only ranking changed: %#v -> %#v", candidateIdentityPrefix(want), candidateIdentityPrefix(got))
	}
}

func TestLargeProseCorpusUsesBoundedStableTermFrontier(t *testing.T) {
	q := buildSearchQuery("stable retrieval")
	candidates := make([]searchCandidate, 1_000)
	for index := range candidates {
		candidate := projectionCandidate(fmt.Sprintf("docs/note%04d.md", index), fmt.Sprintf("Note%04d", index), 1, float64(1_000-index))
		candidate.result.Kind = "section"
		candidate.termCounts = map[string]int{"stable": 1, "retrieval": 1}
		candidates[index] = candidate
	}
	parents := proseParents(candidates, true)
	lists := proseParentTermLists(candidates, parents, proseParentQueryTerms(q), proseParentTermFrontier, true)
	for index, list := range lists {
		if len(list) > proseParentTermFrontier {
			t.Fatalf("term list %d has %d entries, want at most %d", index, len(list), proseParentTermFrontier)
		}
	}
	selected := selectSearchCandidates(candidates, q, 64, 3, true)
	if len(selected) != 64 {
		t.Fatalf("large prose selection returned %d rows, want 64", len(selected))
	}
}

func TestMultiResolutionProjectsOneCompletePromotionOrder(t *testing.T) {
	parents := []SearchResult{
		{Rank: 1, Score: 100, FilePath: "docs/a.md", StartLine: 1, EndLine: 1, SnippetStartLine: 1, SnippetEndLine: 1, Snippet: "a"},
		{Rank: 2, Score: 90, FilePath: "docs/b.md", StartLine: 1, EndLine: 1, SnippetStartLine: 1, SnippetEndLine: 1, Snippet: "b"},
	}
	for parent := range parents {
		for passage := 0; passage < 10; passage++ {
			line := 10 + passage*10
			parents[parent].Passages = append(parents[parent].Passages, SearchPassage{StartLine: line, EndLine: line, FocusLine: line, Snippet: fmt.Sprintf("passage-%d-%d", parent, passage)})
		}
	}
	at8 := expandProseResolution(parents, 8, 0, 0)
	at16 := expandProseResolution(parents, 16, 0, 0)
	identity := func(results []SearchResult) []string {
		out := make([]string, len(results))
		for index, result := range results {
			out[index] = fmt.Sprintf("%s:%d:%d", result.FilePath, result.StartLine, result.EndLine)
		}
		return out
	}
	if !reflect.DeepEqual(identity(at8), identity(at16[:8])) {
		t.Fatalf("multi-resolution prefix changed: %#v -> %#v", identity(at8), identity(at16[:8]))
	}
}

func TestFiniteBudgetOnlyTruncatesStableHead(t *testing.T) {
	q := buildSearchQuery("stable retrieval")
	results := make([]SearchResult, 8)
	for index := range results {
		results[index] = SearchResult{Rank: index + 1, Score: float64(100 - index), FilePath: fmt.Sprintf("src/unit%02d.go", index), StartLine: 1, EndLine: 1, SnippetStartLine: 1, SnippetEndLine: 1, Snippet: strings.Repeat("x", 200)}
	}
	bounded, _, _, _ := fitSearchResultsToBudget(results, q, 1_500)
	if len(bounded) == 0 || len(bounded) >= len(results) {
		t.Fatalf("budget did not exercise truncation: %d results", len(bounded))
	}
	for index := range bounded {
		if bounded[index].FilePath != results[index].FilePath {
			t.Fatalf("budget reordered rank %d: %s -> %s", index+1, results[index].FilePath, bounded[index].FilePath)
		}
	}
}

func BenchmarkSelectHybridCandidatesStableQueues(b *testing.B) {
	semantic := make([]searchCandidate, 2_000)
	sparse := make([]searchCandidate, 100_000)
	for index := range semantic {
		path := fmt.Sprintf("src/unit%04d.go", index%500)
		semantic[index] = projectionCandidate(path, fmt.Sprintf("Semantic%04d", index), index+1, float64(4_000-index))
	}
	for index := range sparse {
		path := fmt.Sprintf("sparse/unit%06d.go", index%25_000)
		sparse[index] = projectionCandidate(path, fmt.Sprintf("Sparse%06d", index), index+10_000, float64(100_000-index))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		_ = selectHybridCandidates(semantic, sparse, 64)
	}
}

func BenchmarkProseSelectionSharedPassagePlans(b *testing.B) {
	for _, sections := range []int{64, 128} {
		b.Run(fmt.Sprintf("4docs_%dsections", sections), func(b *testing.B) {
			q := buildSearchQuery("stable retrieval evidence")
			candidates := make([]searchCandidate, 0, 4*sections)
			for document := 0; document < 4; document++ {
				for section := 0; section < sections; section++ {
					candidate := projectionCandidate(
						fmt.Sprintf("docs/note%d.md", document), fmt.Sprintf("Section%03d", section), section*4+1,
						float64(sections-section),
					)
					candidate.result.Kind = "section"
					candidate.result.Snippet = "stable retrieval evidence"
					candidate.termCounts = map[string]int{"stable": 1, "retrieval": 1, "evidence": 1}
					candidate.proseSection = fmt.Sprintf("docs/note%d.md#%d", document, section*4+1)
					candidates = append(candidates, candidate)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				_ = selectSearchCandidates(candidates, q, 64, 3, true)
			}
		})
	}
}

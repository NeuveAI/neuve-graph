package sem

import (
	"strings"
	"testing"
)

func TestParserIdentityRevisionsSnapshotAndCaches(t *testing.T) {
	header := leanHeader(sourceContext{}, "same-release", profileSpec{})
	// "5": `block` symbols are emitted for HCL only (Rust function bodies lost theirs).
	// "6": every import statement the syntax tree shows has an IMPORTS relation.
	// "7": a call relation's evidence says where the call is (call_site_lines).
	// "8": those lines rest on code, and an argument bound where it is passed
	// is not a relation (ADR 0013).
	if header.IdentityRevision != "8" {
		t.Fatalf("identity=%q", header.IdentityRevision)
	}
	if !strings.HasSuffix(searchSnapshotCacheVersion, "-"+header.IdentityRevision) || !strings.HasSuffix(providerRecordsCacheVersion, "-"+header.IdentityRevision) ||
		!strings.HasSuffix(extractionCacheVersion, "-"+header.IdentityRevision) || !strings.HasSuffix(preparedGenerationVersion, "-"+header.IdentityRevision) {
		t.Fatal("parser identity does not invalidate cached snapshots")
	}
}

func TestDefaultExportIdentityCorrectionIsRevisioned(t *testing.T) {
	entities := javascriptDefaultExportEntities("helper.js", "export default classifier => classifier()\n")
	symbols := entitySymbols("local/example", "helper.js", "JavaScript", entities)
	if len(symbols) != 1 || symbols[0].ID != "local/example:JavaScript:helper.js:function:helper" {
		t.Fatalf("corrected default export symbols = %+v", symbols)
	}
	if IdentityRevision == "js-ts-callable-scope-1" {
		t.Fatal("default export identity correction must invalidate the previous parser revision")
	}
}

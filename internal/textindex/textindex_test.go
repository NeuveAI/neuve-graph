//go:build !windows

package textindex

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"math/rand"
	"strings"
	"testing"
)

func TestCandidatesAreASupersetOfFoldedContainment(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	alphabet := []string{"a", "b", "c", "A", "B", "C", "x", "\n", " ", "é", "K", "ſ", "_", "1"}
	blobs := map[string][]byte{}
	var oids []string
	for i := 0; i < 400; i++ {
		var b strings.Builder
		for j := 0; j < 5+rng.Intn(200); j++ {
			b.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		data := []byte(b.String())
		sum := sha1.Sum(data)
		oid := hex.EncodeToString(sum[:])
		if _, ok := blobs[oid]; !ok {
			oids = append(oids, oid)
		}
		blobs[oid] = data
	}
	store := Open(t.TempDir(), 0)
	read := func(ctx context.Context, want []string, visit func(string, []byte) error) error {
		for _, oid := range want {
			if err := visit(oid, blobs[oid]); err != nil {
				return err
			}
		}
		return nil
	}
	// Index half, so the other half must come back as uncovered.
	receipt, err := store.Build(t.Context(), oids[:200], read)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("receipt %+v", receipt)
	for trial := 0; trial < 300; trial++ {
		var literals []string
		for k := 0; k < 1+rng.Intn(3); k++ {
			var l strings.Builder
			for j := 0; j < 3+rng.Intn(3); j++ {
				l.WriteString([]string{"a", "b", "c", "x", " ", "_", "1"}[rng.Intn(7)])
			}
			literals = append(literals, l.String())
		}
		got, _, err := store.Candidates(t.Context(), oids, literals)
		if err != nil {
			t.Fatal(err)
		}
		for i, oid := range oids {
			folded := []byte(blobs[oid])
			for k, c := range folded {
				if c >= 'A' && c <= 'Z' {
					folded[k] = c + 32
				}
			}
			contains := false
			for _, l := range literals {
				if strings.Contains(string(folded), l) {
					contains = true
				}
			}
			if contains && !got[oid] {
				t.Fatalf("trial %d: blob %d holds one of %q but is not a candidate", trial, i, literals)
			}
			if i >= 200 && !got[oid] {
				t.Fatalf("unindexed blob %d is not a candidate", i)
			}
		}
	}
}

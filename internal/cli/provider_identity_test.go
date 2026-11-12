package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

func TestVersionAdvertisesParserIdentity(t *testing.T) {
	var out bytes.Buffer
	if err := Run(context.Background(), Options{Stdout: &out, Version: "same-release"}, []string{"version", "--json"}); err != nil {
		t.Fatal(err)
	}
	var info map[string]string
	if err := json.Unmarshal(out.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info["identity_revision"] != "5" {
		t.Fatalf("identity=%v", info)
	}
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	if info["build_identity"] != hex.EncodeToString(digest[:]) {
		t.Fatalf("version build identity=%q does not identify executed file", info["build_identity"])
	}
}

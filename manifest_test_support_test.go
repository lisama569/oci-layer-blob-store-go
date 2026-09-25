package ocistore

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func repeat0(nBytes int) string { return strings.Repeat("00", nBytes) }

func writeRefFile(t *testing.T, p string, rec refRecord) {
	t.Helper()
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

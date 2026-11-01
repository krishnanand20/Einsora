package catalog

import (
	"os"
	"testing"
)

// The built-in catalog is a copy of images/catalog.yaml: it must never drift from it.
func TestDefaultMatchesImages(t *testing.T) {
	want, err := os.ReadFile("../../../images/catalog.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != string(defaultCatalog) {
		t.Fatal("internal/catalog/default.yaml differs from images/catalog.yaml: run `make catalog`")
	}
	if _, err := Load(""); err != nil {
		t.Fatalf("the built-in catalog: %v", err)
	}
}

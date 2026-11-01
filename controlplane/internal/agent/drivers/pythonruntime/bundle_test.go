package pythonruntime

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestBundleMatchesMLSources(t *testing.T) {
	archive, err := zip.NewReader(bytes.NewReader(Bytes()), int64(len(Bytes())))
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join("..", "..", "..", "..", "..", "ml", "src")
	files, err := filepath.Glob(filepath.Join(source, "einsora_ml", "*.py"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(archive.File) {
		t.Fatal("runtime bundle is stale: run python3 ml/scripts/bundle_native.py")
	}
	for _, file := range archive.File {
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		original, err := os.ReadFile(filepath.Join(source, filepath.FromSlash(file.Name)))
		if err != nil || !bytes.Equal(data, original) {
			t.Errorf("%s differs from its ML source; regenerate runtime.zip", file.Name)
		}
	}
}

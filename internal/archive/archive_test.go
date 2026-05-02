package archive

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCreateAndExtractTarGz(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dst := filepath.Join(root, "snapshot.tar.gz")
	out := filepath.Join(root, "out")

	if err := os.MkdirAll(filepath.Join(src, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "nested", "data.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	size, err := CreateTarGz(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if size == 0 {
		t.Fatal("expected non-empty archive")
	}
	if err := ExtractTarGz(dst, out); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(out, "nested", "data.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello" {
		t.Fatalf("unexpected extracted content: %q", string(b))
	}
}

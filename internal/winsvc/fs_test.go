package winsvc

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// A file opened by a viewer must not stop the recorder renaming it or the
// mover and retention deleting it (spec 16.9).
func TestOpenSharedAllowsRenameAndDelete(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.part.mp4")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := OpenShared(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	q := filepath.Join(dir, "a.mp4")
	if err := os.Rename(p, q); err != nil {
		t.Fatalf("rename while open: %v", err)
	}
	if err := os.Remove(q); err != nil {
		t.Fatalf("delete while open: %v", err)
	}
	b, err := io.ReadAll(f)
	if err != nil || string(b) != "hello" {
		t.Fatalf("reading after delete: %q %v", b, err)
	}
}

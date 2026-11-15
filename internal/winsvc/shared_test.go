package winsvc

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// A file held open with OpenShared can still be appended to, renamed and
// deleted (spec 16.9, rule 4). On Windows this fails with a plain os.Open.
func TestOpenSharedAllowsWriteRenameAndDelete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.part.mp4")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := OpenShared(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	w, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("writing while a reader holds the file: %v", err)
	}
	if _, err := w.WriteString(" world"); err != nil {
		t.Fatal(err)
	}
	w.Close()

	final := filepath.Join(dir, "a.mp4")
	if err := os.Rename(path, final); err != nil {
		t.Fatalf("renaming while a reader holds the file: %v", err)
	}
	if err := os.Remove(final); err != nil {
		t.Fatalf("deleting while a reader holds the file: %v", err)
	}
	// The reader keeps working until it closes its handle.
	data, err := io.ReadAll(r)
	if err != nil || string(data) != "hello world" {
		t.Fatalf("read %q, %v", data, err)
	}
	if _, err := os.Stat(final); !os.IsNotExist(err) {
		t.Fatalf("the file is still listed after its deletion: %v", err)
	}
}

package mover

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/ipc"
	"github.com/jxburros/buttercams/internal/naming"
	"github.com/jxburros/buttercams/internal/winsvc"
)

// PartSuffix marks a copy that is still being written on a destination.
const PartSuffix = ".part"

const copyBuffer = 1 << 20

// errSourceMissing means the staging file is gone; the copy can never succeed.
var errSourceMissing = errors.New("the staging file no longer exists")

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

func hashFile(ctx context.Context, path string, buf []byte) (string, int64, error) {
	f, err := winsvc.OpenShared(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.CopyBuffer(h, ctxReader{ctx, f}, buf)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// connect authenticates to a UNC destination when it has its own credentials.
func connect(d config.Destination) error {
	if d.Type != config.DestUNC || d.Username == "" {
		return nil
	}
	pw, err := config.Unprotect(d.PasswordEnc)
	if err != nil {
		return err
	}
	return winsvc.ConnectShare(d.Path, d.Username, pw)
}

// copyFile copies one closed segment to a destination (spec 7.2 step 2):
// write <name>.part, flush, verify size and SHA-256 by reading the copy back,
// then rename to the final name. It returns the SHA-256 and size of the file.
func copyFile(ctx context.Context, job *ipc.CopyJob, dest config.Destination) (sum string, size int64, err error) {
	rel, ok := naming.RelPath(job.FileName)
	if !ok {
		return "", 0, fmt.Errorf("%q is not a ButterCams recording name", job.FileName)
	}
	src, err := winsvc.OpenShared(job.SourcePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", 0, errSourceMissing
		}
		return "", 0, err
	}
	defer src.Close()

	final := filepath.Join(dest.Path, rel)
	part := final + PartSuffix
	buf := make([]byte, copyBuffer)

	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return "", 0, err
	}
	out, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	size, err = io.CopyBuffer(io.MultiWriter(out, h), ctxReader{ctx, src}, buf)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(part)
		return "", 0, err
	}
	sum = hex.EncodeToString(h.Sum(nil))

	// The staging file must still be what the recorder wrote.
	if job.Bytes > 0 && size != job.Bytes {
		os.Remove(part)
		return "", 0, fmt.Errorf("staging file is %d bytes but %d were recorded", size, job.Bytes)
	}
	if job.SHA256 != "" && sum != job.SHA256 {
		os.Remove(part)
		return "", 0, fmt.Errorf("staging file does not match its recorded SHA-256")
	}

	// Verify what actually landed on the destination.
	gotSum, gotSize, err := hashFile(ctx, part, buf)
	if err != nil {
		os.Remove(part)
		return "", 0, fmt.Errorf("verifying the copy: %w", err)
	}
	if gotSize != size || gotSum != sum {
		os.Remove(part)
		return "", 0, fmt.Errorf("verification failed: the copy on the destination differs from the staging file")
	}
	if err := os.Rename(part, final); err != nil {
		os.Remove(part)
		return "", 0, err
	}
	return sum, size, nil
}

// TestResult is the outcome of the destination test (spec 7.4).
type TestResult struct {
	FreeBytes  uint64
	TotalBytes uint64
}

// TestDestination creates the folder if needed, writes a 1 MB file, reads it
// back, compares, deletes it, and reports free and total space. Errors carry
// the operating system's own message.
func TestDestination(d config.Destination, password string) (TestResult, error) {
	var res TestResult
	if d.Type == config.DestUNC && d.Username != "" {
		if err := winsvc.ConnectShare(d.Path, d.Username, password); err != nil {
			return res, err
		}
	}
	if err := os.MkdirAll(d.Path, 0o755); err != nil {
		return res, err
	}
	data := make([]byte, 1<<20)
	if _, err := rand.Read(data); err != nil {
		return res, err
	}
	name := filepath.Join(d.Path, "buttercams-test-"+hex.EncodeToString(data[:6])+".tmp")
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return res, err
	}
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(name)
		return res, werr
	}
	back, err := os.ReadFile(name)
	if rerr := os.Remove(name); err == nil && rerr != nil {
		err = fmt.Errorf("the test file could not be deleted: %w", rerr)
	}
	if err != nil {
		return res, err
	}
	if !bytes.Equal(back, data) {
		return res, errors.New("the test file read back differently from what was written")
	}
	free, total, err := winsvc.DiskFree(d.Path)
	if err != nil {
		return res, err
	}
	res.FreeBytes, res.TotalBytes = free, total
	return res, nil
}

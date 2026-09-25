package ocistore

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s
}

func TestPutGetOK(t *testing.T) {
	s := newTestStore(t)
	content := []byte("hello-oci-layer")
	d := DigestOf(content)
	t.Logf("blob digest=%s size=%d", d, len(content))

	info, err := s.Put(d, int64(len(content)), bytes.NewReader(content))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if info.Digest != d {
		t.Fatalf("digest = %s, want %s", info.Digest, d)
	}

	rc, gi, err := s.Get(d)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, content) {
		t.Fatalf("content mismatch")
	}
	if gi.Size != int64(len(content)) {
		t.Fatalf("got size %d", gi.Size)
	}
}

func TestPutFailureReasons(t *testing.T) {
	s := newTestStore(t)
	content := []byte("layer-content-xyz")
	realD := DigestOf(content)

	// 1) 摘要不符
	wrong := DigestOf([]byte("different"))
	t.Logf("declared digest=%s but actual digest=%s", wrong, realD)
	if _, err := s.Put(wrong, int64(len(content)), bytes.NewReader(content)); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("want ErrDigestMismatch, got %v", err)
	}

	// 2) 内容被截断（声明大小大于实际）
	t.Logf("declared size=%d actual size=%d", len(content)+10, len(content))
	if _, err := s.Put(realD, int64(len(content)+10), bytes.NewReader(content)); !errors.Is(err, ErrTruncated) {
		t.Fatalf("want ErrTruncated, got %v", err)
	}

	// 3) 大小不符（声明大小小于实际）
	t.Logf("declared size=%d actual size=%d", len(content)-4, len(content))
	if _, err := s.Put(realD, int64(len(content)-4), bytes.NewReader(content)); !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("want ErrSizeMismatch, got %v", err)
	}

	// 失败后不留半成品，且真实摘要内容也不存在
	if s.Has(realD) {
		t.Fatalf("blob must not exist after failed writes")
	}
	entries, _ := os.ReadDir(filepath.Join(s.Root(), "tmp"))
	for _, e := range entries {
		t.Fatalf("temp file leaked: %s", e.Name())
	}
}

func TestDedupSameDigestSingleFile(t *testing.T) {
	s := newTestStore(t)
	content := []byte("shared-layer-data")
	d := DigestOf(content)

	for i := 0; i < 3; i++ {
		if _, err := s.Put(d, int64(len(content)), bytes.NewReader(content)); err != nil {
			t.Fatalf("put #%d: %v", i, err)
		}
	}
	blobs, err := s.ListBlobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(blobs) != 1 {
		t.Fatalf("want exactly 1 blob (dedup), got %d: %+v", len(blobs), blobs)
	}
	t.Logf("dedup: 3 writes of digest=%s -> 1 file on disk", d)
}

func TestGetNotFound(t *testing.T) {
	s := newTestStore(t)
	missing := DigestOf([]byte("ghost"))
	if _, _, err := s.Get(missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	t.Logf("missing digest=%s correctly rejected", missing)
}

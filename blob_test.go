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

// 内容已在库中时，再写一遍仍须以实际读到的内容为准做校验：
// 声明摘要/大小与内容对不上，照样按对应原因拒绝，不能因“库里已有一份”放过。
func TestPutExistingContentStillVerifies(t *testing.T) {
	s := newTestStore(t)
	content := []byte("already-stored-content")
	d := DigestOf(content)
	if _, err := s.Put(d, int64(len(content)), bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}

	// 1) 库中已有 d，但这次输入是别的内容、却仍声明 d -> ErrDigestMismatch
	other := []byte("totally-different-content")
	if _, err := s.Put(d, int64(len(other)), bytes.NewReader(other)); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("existing digest with different content want ErrDigestMismatch, got %v", err)
	}
	t.Logf("rewrite existing %s with mismatched content rejected (ErrDigestMismatch)", d)

	// 2) 内容正确但声明大小偏大（截断）-> ErrTruncated
	if _, err := s.Put(d, int64(len(content))+5, bytes.NewReader(content)); !errors.Is(err, ErrTruncated) {
		t.Fatalf("existing digest with short content want ErrTruncated, got %v", err)
	}
	t.Log("rewrite existing with short declared size rejected (ErrTruncated)")

	// 3) 内容正确但声明大小偏小 -> ErrSizeMismatch
	if _, err := s.Put(d, int64(len(content))-5, bytes.NewReader(content)); !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("existing digest with long content want ErrSizeMismatch, got %v", err)
	}
	t.Log("rewrite existing with long declared size rejected (ErrSizeMismatch)")

	// 4) 用相同内容再写一遍仍成功（幂等命中，库里仍是一份）
	if _, err := s.Put(d, int64(len(content)), bytes.NewReader(content)); err != nil {
		t.Fatalf("identical rewrite should succeed, got %v", err)
	}
	blobs, _ := s.ListBlobs()
	if len(blobs) != 1 {
		t.Fatalf("want still 1 blob, got %d", len(blobs))
	}
	rc, _, err := s.Get(d)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, content) {
		t.Fatal("stored content altered by rejected rewrites")
	}
	t.Log("identical re-put is an idempotent hit; stored content untouched after rejections")
}

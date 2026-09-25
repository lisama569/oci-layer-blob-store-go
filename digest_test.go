package ocistore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDigestInvalid(t *testing.T) {
	bad := []string{"", "sha256", "sha256:", "md5:deadbeef", "sha256:zz", "sha256:01"}
	for _, s := range bad {
		_, err := ParseDigest(s)
		if !errors.Is(err, ErrInvalidDigest) {
			t.Fatalf("ParseDigest(%q) err=%v, want ErrInvalidDigest", s, err)
		}
		t.Logf("ParseDigest(%q) -> ErrInvalidDigest (%v)", s, err)
	}
}

func TestPutBlobDigestMismatch(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	data := []byte("layer-content-A")
	wrong := digestOf([]byte("different-content"))

	_, err := s.PutBlob(ctx, bytes.NewReader(data), wrong, int64(len(data)))
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("want ErrDigestMismatch, got %v", err)
	}
	var dm *DigestMismatchError
	if errors.As(err, &dm) {
		t.Logf("拒绝依据: expected=%s actual=%s => digest mismatch", dm.Expected, dm.Got)
		if dm.Got != digestOf(data) {
			t.Fatalf("actual digest should be computed: got %s want %s", dm.Got, digestOf(data))
		}
	}
	assertNoContent(t, s, digestOf(data))
}

func TestPutBlobTruncated(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	data := []byte("short")

	_, err := s.PutBlob(ctx, bytes.NewReader(data), digestOf(data), 128)
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("want ErrTruncated, got %v", err)
	}
	var te *TruncatedError
	if errors.As(err, &te) {
		t.Logf("拒绝依据: declared=%d actual=%d => truncated", te.Declared, te.Actual)
	}
	assertNoContent(t, s, digestOf(data))
}

func TestPutBlobSizeMismatchLonger(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	data := []byte("this-is-longer-than-declared")
	d := digestOf(data)

	_, err := s.PutBlob(ctx, bytes.NewReader(data), d, 3)
	if !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("want ErrSizeMismatch, got %v", err)
	}
	var sm *SizeMismatchError
	if errors.As(err, &sm) {
		t.Logf("拒绝依据: declared=%d actual=%d => size mismatch", sm.Declared, sm.Actual)
	}
	assertNoContent(t, s, d)
}

func TestPutBlobDedupAndGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	data := []byte("shared-layer-data")
	d := digestOf(data)

	d1, err := s.PutBlob(ctx, bytes.NewReader(data), d, int64(len(data)))
	if err != nil {
		t.Fatalf("first put: %v", err)
	}
	d2, err := s.PutBlob(ctx, bytes.NewReader(append([]byte(nil), data...)), d, int64(len(data)))
	if err != nil {
		t.Fatalf("second put: %v", err)
	}
	if d1.Digest != d || d2.Digest != d {
		t.Fatalf("dedup digest mismatch: %s %s want %s", d1.Digest, d2.Digest, d)
	}
	all, _ := s.blobs.ListBlobs(ctx)
	if len(all) != 1 || all[0] != d {
		t.Fatalf("same digest must land once, got %v", all)
	}
	t.Logf("去重判定: digest=%s 写入2次 落盘=%v", d, all)

	got := blobContent(t, s, d)
	if !bytes.Equal(got, data) {
		t.Fatalf("read back mismatch")
	}
	missing := mustParseDigest(t, "sha256:"+strings.Repeat("00", 32))
	if _, _, err := s.GetBlob(ctx, missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing blob want ErrNotFound, got %v", err)
	}
}

func TestPutBlobNoExpectedDigest(t *testing.T) {
	s := newTestStore(t)
	desc, err := s.PutBlob(context.Background(), bytes.NewReader([]byte("auto")), "", -1)
	if err != nil {
		t.Fatalf("put without expected digest: %v", err)
	}
	if want := digestOf([]byte("auto")); desc.Digest != want {
		t.Fatalf("auto digest got %s want %s", desc.Digest, want)
	}
	t.Logf("未声明摘要时按实际内容定址: digest=%s size=%d", desc.Digest, desc.Size)
}

func TestTruncatedReadFromBrokenReader(t *testing.T) {
	s := newTestStore(t)
	data := bytes.Repeat([]byte("x"), 100)
	d := digestOf(data)
	_, err := s.PutBlob(context.Background(), ioLimitReader(bytes.NewReader(data), 60), d, int64(len(data)))
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("want ErrTruncated on broken stream, got %v", err)
	}
	t.Logf("半截流拒绝依据: %v", err)
	assertNoContent(t, s, d)
}

// assertNoContent 校验 blob 不存在且 tmp 目录无残留。
func assertNoContent(t *testing.T, s *Store, d Digest) {
	t.Helper()
	if ok, _ := s.HasBlob(context.Background(), d); ok {
		t.Fatalf("blob %s should not exist after rejected write", d)
	}
	tmpEntries, err := os.ReadDir(filepath.Join(s.root, "tmp"))
	if err != nil {
		t.Fatalf("read tmp: %v", err)
	}
	if len(tmpEntries) != 0 {
		t.Fatalf("rejected write left temp files: %d", len(tmpEntries))
	}
}

// ioLimitReader 返回只允许读 n 字节然后返回意外错误的 reader。
func ioLimitReader(src *bytes.Reader, n int64) *errAfterReader {
	return &errAfterReader{src: src, remain: n}
}

type errAfterReader struct {
	src    *bytes.Reader
	remain int64
}

func (e *errAfterReader) Read(p []byte) (int, error) {
	if e.remain <= 0 {
		return 0, fmt.Errorf("unexpected EOF")
	}
	if int64(len(p)) > e.remain {
		p = p[:e.remain]
	}
	n, err := e.src.Read(p)
	e.remain -= int64(n)
	return n, err
}

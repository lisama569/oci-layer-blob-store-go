package ocistore

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"testing"
)

func TestGetManifestRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	d := buildSimpleImage(t, s, "rt")

	mt, raw, err := s.GetManifest(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if mt != MediaTypeImageManifest {
		t.Fatalf("mediaType=%s", mt)
	}
	t.Logf("GetManifest digest=%s mediaType=%s bytes=%d", d, mt, len(raw))

	if _, _, err := s.GetManifest(ctx, mustParseDigest(t, "sha256:"+repeat0(32))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing manifest want ErrNotFound, got %v", err)
	}

	st, err := s.StatBlob(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size != int64(len(raw)) {
		t.Fatalf("stat size %d want %d", st.Size, len(raw))
	}
	if ok, _ := s.HasBlob(ctx, d); !ok {
		t.Fatal("HasBlob should be true")
	}
}

func TestPutManifestMalformedJSON(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.PutManifest(context.Background(), MediaTypeImageManifest, []byte("{not-json")); err == nil {
		t.Fatal("malformed manifest must be rejected")
	} else {
		t.Logf("坏 JSON 清单被拒绝: %v", err)
	}
	if _, err := s.PutManifest(context.Background(), MediaTypeImageIndex, []byte("[]")); err == nil {
		t.Fatal("non-object index must be rejected")
	}
}

func TestInvalidRefNames(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	d := buildSimpleImage(t, s, "x")
	for _, bad := range []string{"", "a/b", `a\b`} {
		if _, err := s.Tag(ctx, bad, d); err == nil {
			t.Fatalf("tag name %q must be rejected", bad)
		}
		if _, err := s.ResolveTag(ctx, bad); err == nil {
			t.Fatalf("resolve name %q must be rejected", bad)
		}
	}
	t.Log("非法引用名（空、含路径分隔符）均被拒绝")
}

func TestSHA512Blob(t *testing.T) {
	s := newTestStore(t)
	data := []byte("sha512-content")
	sum := sha512.Sum512(data)
	d := Digest("sha512:" + hex.EncodeToString(sum[:]))

	desc, err := s.PutBlob(context.Background(), bytes.NewReader(data), d, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if desc.Digest != d {
		t.Fatalf("digest %s want %s", desc.Digest, d)
	}
	got := blobContent(t, s, d)
	if !bytes.Equal(got, data) {
		t.Fatal("sha512 content mismatch")
	}
	all, _ := s.blobs.ListBlobs(context.Background())
	t.Logf("sha512 定址: %s, list=%v", d, all)
}

func TestRootAndClose(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("empty root must fail")
	}
	s := newTestStore(t)
	if s.Root() == "" {
		t.Fatal("root empty")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

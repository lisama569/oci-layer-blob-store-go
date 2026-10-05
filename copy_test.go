package ocistore

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// buildImage 构造单 manifest 镜像并登记名字，返回各内容摘要。
func buildImage(t *testing.T, s *Store, ref string, cfg []byte, layers ...[]byte) (Digest, []Digest) {
	t.Helper()
	var all []Digest
	put := func(b []byte) Digest {
		info, err := s.PutBytes(b)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, info.Digest)
		return info.Digest
	}
	cfgD := put(cfg)
	m := Manifest{MediaType: MediaTypeImageManifest,
		Config: Descriptor{Digest: cfgD}}
	for _, l := range layers {
		m.Layers = append(m.Layers, Descriptor{Digest: put(l)})
	}
	mData, _ := json.Marshal(m)
	mD := put(mData)
	if err := s.Tag(ref, mD); err != nil {
		t.Fatal(err)
	}
	return mD, all
}

func TestCopyImageBasic(t *testing.T) {
	src := newTestStore(t)
	dst := newTestStore(t)

	root, all := buildImage(t, src, "app/web:v1",
		[]byte("cfg"), []byte("layer-1"), []byte("layer-2"))

	rep, err := CopyImage(t.Context(), dst, src, "app/web:v1")
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	t.Logf("copied=%d reused=%d root=%s", len(rep.Copied), len(rep.Reused), rep.Root)

	// 名字与摘要两边对得上
	d, err := dst.ResolveRef("app/web:v1")
	if err != nil {
		t.Fatalf("resolve in dst: %v", err)
	}
	if d != root {
		t.Fatalf("dst ref = %s, want %s", d, root)
	}
	for _, dg := range all {
		if !dst.Has(dg) {
			t.Fatalf("dst missing %s", dg)
		}
	}
	// 内容一致
	got, _ := dst.GetBytes(root)
	want, _ := src.GetBytes(root)
	if !bytes.Equal(got, want) {
		t.Fatal("root manifest content differs between src and dst")
	}
}

func TestCopyImageDedupAcrossImages(t *testing.T) {
	src := newTestStore(t)
	dst := newTestStore(t)

	shared := []byte("shared-base-layer")
	buildImage(t, src, "app/a:v1", []byte("cfg-a"), shared, []byte("layer-a"))
	buildImage(t, src, "app/b:v1", []byte("cfg-b"), shared, []byte("layer-b"))

	if _, err := CopyImage(t.Context(), dst, src, "app/a:v1"); err != nil {
		t.Fatal(err)
	}
	rep, err := CopyImage(t.Context(), dst, src, "app/b:v1")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("second copy: copied=%d reused=%d", len(rep.Copied), len(rep.Reused))

	// 共享层在目标仓库只存一份：第二次搬运应去重跳过
	sharedD := DigestOf(shared)
	if !containsDigest(rep.Reused, sharedD) {
		t.Fatalf("shared layer %s should be reused, report=%+v", sharedD, rep)
	}
	// 目标仓库 blob 总数 = 两边各自独占 + 共享层一份
	blobs, _ := dst.ListBlobs()
	// a: cfg-a, shared, layer-a, manifest-a (4); b 新增: cfg-b, layer-b, manifest-b (3)
	if len(blobs) != 7 {
		t.Fatalf("dst should hold 7 blobs (shared layer stored once), got %d", len(blobs))
	}
}

func TestCopyImageSourceMissing(t *testing.T) {
	src := newTestStore(t)
	dst := newTestStore(t)

	layer := []byte("will-go-missing")
	_, all := buildImage(t, src, "app/broken:v1", []byte("cfg"), layer)
	layerD := DigestOf(layer)

	// 源端删掉层内容
	if err := os.Remove(filepath.Join(src.Root(), "blobs", "sha256", layerD.Hex())); err != nil {
		t.Fatal(err)
	}

	_, err := CopyImage(t.Context(), dst, src, "app/broken:v1")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	t.Logf("missing-source error: %v", err)
	// 目标仓库不得留下指向残缺内容的名字
	if _, rerr := dst.ResolveRef("app/broken:v1"); !errors.Is(rerr, ErrRefNotFound) {
		t.Fatalf("dst must not register a broken ref, got %v", rerr)
	}

	// 修复源端后重试：应成功，且已搬运内容去重
	if _, err := src.PutBytes(layer); err != nil {
		t.Fatal(err)
	}
	rep, err := CopyImage(t.Context(), dst, src, "app/broken:v1")
	if err != nil {
		t.Fatalf("retry after restore: %v", err)
	}
	t.Logf("retry: copied=%d reused=%d", len(rep.Copied), len(rep.Reused))
	if len(rep.Reused) == 0 {
		t.Fatal("retry should reuse blobs copied before the failure")
	}
	for _, dg := range all {
		if !dst.Has(dg) {
			t.Fatalf("dst missing %s after retry", dg)
		}
	}
	if d, _ := dst.ResolveRef("app/broken:v1"); d == "" {
		t.Fatal("ref must be registered after successful retry")
	}
}

func TestCopyImageSourceCorrupt(t *testing.T) {
	src := newTestStore(t)
	dst := newTestStore(t)

	layer := []byte("integrity-matters")
	buildImage(t, src, "app/corrupt:v1", []byte("cfg"), layer)
	layerD := DigestOf(layer)

	// 篡改源端层内容（文件名不变，内容变了）
	p := filepath.Join(src.Root(), "blobs", "sha256", layerD.Hex())
	if err := os.WriteFile(p, []byte("tampered-content!!"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := CopyImage(t.Context(), dst, src, "app/corrupt:v1")
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("want ErrDigestMismatch, got %v", err)
	}
	t.Logf("corrupt-source error: %v", err)
	if _, rerr := dst.ResolveRef("app/corrupt:v1"); !errors.Is(rerr, ErrRefNotFound) {
		t.Fatalf("dst must not register ref after corrupt copy, got %v", rerr)
	}
}

func TestCopyImageLargeLayerStreaming(t *testing.T) {
	src := newTestStore(t)
	dst := newTestStore(t)

	// 8 MiB 随机层 + 多层，验证大镜像流式搬运的正确性
	var layers [][]byte
	for i := 0; i < 4; i++ {
		l := make([]byte, 2<<20)
		if _, err := rand.Read(l); err != nil {
			t.Fatal(err)
		}
		layers = append(layers, l)
	}
	_, all := buildImage(t, src, "app/big:v1", []byte("cfg"), layers...)

	if _, err := CopyImage(t.Context(), dst, src, "app/big:v1"); err != nil {
		t.Fatal(err)
	}
	for _, dg := range all {
		got, err := dst.GetBytes(dg)
		if err != nil {
			t.Fatalf("dst missing %s: %v", dg, err)
		}
		if DigestOf(got) != dg {
			t.Fatalf("dst content for %s hashes to %s", dg, DigestOf(got))
		}
	}
}

func TestCopyImageUnknownRef(t *testing.T) {
	src := newTestStore(t)
	dst := newTestStore(t)
	_, err := CopyImage(t.Context(), dst, src, "app/ghost:v9")
	if !errors.Is(err, ErrRefNotFound) {
		t.Fatalf("want ErrRefNotFound, got %v", err)
	}
}

func containsDigest(ds []Digest, d Digest) bool {
	for _, x := range ds {
		if x == d {
			return true
		}
	}
	return false
}

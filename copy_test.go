package ocistore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"testing"
)

// buildMultiArchStore 构造一个多架构源仓库并登记 name:tag。
func buildMultiArchStore(t *testing.T, name, tag string) (*Store, Digest, Digest, Digest) {
	t.Helper()
	s := newTestStore(t)
	dAMD := buildTaggedChild(t, s, "amd64")
	dARM := buildTaggedChild(t, s, "arm64")
	idx := buildIndex(t, s, dAMD, dARM)
	if err := s.Tag(name, tag, idx); err != nil {
		t.Fatal(err)
	}
	return s, idx, dAMD, dARM
}

func TestCopyFromFullImage(t *testing.T) {
	src, idx, dAMD, dARM := buildMultiArchStore(t, "registry.io/app", "1.0")
	dst := newTestStore(t)

	rep, err := dst.CopyFrom(context.Background(), src, "registry.io/app", "1.0")
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	// index + 2 manifest + 2 cfg + 2 layer = 7 全部实际传输
	if len(rep.Copied) != 7 {
		t.Fatalf("want 7 copied blobs, got %d (%v)", len(rep.Copied), rep.Copied)
	}
	if len(rep.Present) != 0 {
		t.Fatalf("want nothing pre-present, got %v", rep.Present)
	}
	t.Logf("copied %d blobs; tag %s:%s -> %s", len(rep.Copied), rep.Tag.Name, rep.Tag.Tag, rep.Tag.Digest)

	// 目标端名字/摘要与源端一致
	if rep.Tag.Digest != idx {
		t.Fatalf("target tag digest %s != source %s", rep.Tag.Digest, idx)
	}
	got, err := dst.Lookup("registry.io/app", "1.0")
	if err != nil {
		t.Fatalf("target lookup: %v", err)
	}
	if got != idx {
		t.Fatalf("target tag points %s, want %s", got, idx)
	}

	// 目标端能从 index 解析到 arm64
	m, err := dst.Resolve(idx, Platform{OS: "linux", Architecture: "arm64"})
	if err != nil {
		t.Fatalf("resolve copied image: %v", err)
	}
	if m.Config.Digest != DigestOf([]byte("cfg-arm64")) {
		t.Fatalf("copied arm64 config wrong: %s", m.Config.Digest)
	}

	// 内容逐字节一致
	for _, d := range []Digest{idx, dAMD, dARM, DigestOf([]byte("cfg-amd64")), DigestOf([]byte("layer-amd64"))} {
		srcData, _ := src.GetBytes(d)
		dstData, gerr := dst.GetBytes(d)
		if gerr != nil {
			t.Fatalf("dst get %s: %v", d, gerr)
		}
		if !bytes.Equal(srcData, dstData) {
			t.Fatalf("blob %s content differs after copy", d)
		}
	}
	t.Log("target image resolvable and byte-identical across both architectures")
}

func TestCopyFromDeduplicates(t *testing.T) {
	src1, idx1, _, _ := buildMultiArchStore(t, "reg/app", "1.0")
	dst := newTestStore(t)

	if _, err := dst.CopyFrom(context.Background(), src1, "reg/app", "1.0"); err != nil {
		t.Fatal(err)
	}

	// 第二个源仓库：共享 amd64 子镜像内容，index 不同（amd64 + ppc64le）
	src2 := newTestStore(t)
	dAMD := buildTaggedChild(t, src2, "amd64") // 与 src1 的 amd64 内容完全相同
	dPPC := buildTaggedChild(t, src2, "ppc64le")
	idx2 := buildIndex(t, src2, dAMD, dPPC)
	if err := src2.Tag("reg/app", "2.0", idx2); err != nil {
		t.Fatal(err)
	}

	rep, err := dst.CopyFrom(context.Background(), src2, "reg/app", "2.0")
	if err != nil {
		t.Fatalf("copy second: %v", err)
	}
	// idx2 + ppc manifest + ppc cfg + ppc layer = 4 个新内容；amd64 三件去重
	if len(rep.Copied) != 4 {
		t.Fatalf("want 4 copied, got %d (%v)", len(rep.Copied), rep.Copied)
	}
	if len(rep.Present) != 3 {
		t.Fatalf("want 3 present (amd64 triple), got %d (%v)", len(rep.Present), rep.Present)
	}
	t.Logf("second copy: copied=%d present=%d (shared amd64 deduplicated)", len(rep.Copied), len(rep.Present))

	// 两张图各 7 个 blob，amd64 三件共享：7 + 7 - 3 = 11 份唯一 blob
	blobs, _ := dst.ListBlobs()
	if len(blobs) != 11 {
		t.Fatalf("want 11 unique blobs in dst, got %d", len(blobs))
	}
	if got, _ := dst.Lookup("reg/app", "1.0"); got != idx1 {
		t.Fatalf("1.0 tag lost: %s", got)
	}
	if got, _ := dst.Lookup("reg/app", "2.0"); got != idx2 {
		t.Fatalf("2.0 tag wrong: %s", got)
	}
}

func TestCopyFromIdempotentRetry(t *testing.T) {
	src, idx, _, _ := buildMultiArchStore(t, "app", "latest")
	dst := newTestStore(t)

	if _, err := dst.CopyFrom(context.Background(), src, "app", "latest"); err != nil {
		t.Fatal(err)
	}
	// 再搬一次：全部去重，不新增内容
	rep, err := dst.CopyFrom(context.Background(), src, "app", "latest")
	if err != nil {
		t.Fatalf("repeat copy: %v", err)
	}
	if len(rep.Copied) != 0 || len(rep.Present) != 7 {
		t.Fatalf("retry should be all-present, got copied=%d present=%d", len(rep.Copied), len(rep.Present))
	}
	blobs, _ := dst.ListBlobs()
	if len(blobs) != 7 {
		t.Fatalf("blob count grew after retry: %d", len(blobs))
	}
	t.Logf("retry copied=0 present=%d, no duplicated content", len(rep.Present))

	// 模拟中途失败后续跑：先手工放 3 个 blob 到目标，再跑 CopyFrom
	dst2 := newTestStore(t)
	allBlobs, _ := src.ListBlobs()
	for i, b := range allBlobs {
		if i >= 3 {
			break
		}
		data, _ := src.GetBytes(b.Digest)
		if _, err := dst2.Put(b.Digest, b.Size, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
	}
	rep2, err := dst2.CopyFrom(context.Background(), src, "app", "latest")
	if err != nil {
		t.Fatalf("resume copy: %v", err)
	}
	if len(rep2.Present) != 3 || len(rep2.Copied) != 4 {
		t.Fatalf("resume want 3 present/4 copied, got %d/%d", len(rep2.Present), len(rep2.Copied))
	}
	if got, _ := dst2.Lookup("app", "latest"); got != idx {
		t.Fatal("tag missing after resumed copy")
	}
	t.Logf("resumed: %d already there + %d streamed, final image intact", len(rep2.Present), len(rep2.Copied))
}

func TestCopyFromSourceMissingBlob(t *testing.T) {
	src, idx, _, _ := buildMultiArchStore(t, "app", "latest")
	missingLayer := DigestOf([]byte("layer-arm64"))
	if err := os.Remove(src.blobPath(missingLayer)); err != nil {
		t.Fatal(err)
	}

	dst := newTestStore(t)
	_, err := dst.CopyFrom(context.Background(), src, "app", "latest")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	t.Logf("missing layer rejected distinctly: %v", err)

	// 目标不得出现指向残缺内容的标签
	if _, lerr := dst.Lookup("app", "latest"); !errors.Is(lerr, ErrNameNotFound) {
		t.Fatalf("target must not register a half-copied tag, got %v", lerr)
	}
	// 已拷入的 blob 仍各自完整可读，目标仓库保持自洽
	dstBlobs, _ := dst.ListBlobs()
	for _, b := range dstBlobs {
		rc, _, gerr := dst.Get(b.Digest)
		if gerr != nil {
			t.Fatalf("target blob %s corrupted: %v", b.Digest, gerr)
		}
		rc.Close()
	}
	t.Logf("after failure target has %d intact blobs and no half tag (root index=%s)", len(dstBlobs), idx)

	// 源端补齐后重试，应成功且不重复堆内容
	layerData := []byte("layer-arm64")
	if _, err := src.Put(missingLayer, int64(len(layerData)), bytes.NewReader(layerData)); err != nil {
		t.Fatal(err)
	}
	rep2, err := dst.CopyFrom(context.Background(), src, "app", "latest")
	if err != nil {
		t.Fatalf("retry after repair: %v", err)
	}
	if got, _ := dst.Lookup("app", "latest"); got != idx {
		t.Fatalf("tag after retry wrong: %s", got)
	}
	t.Logf("retry succeeded: copied=%d present=%d", len(rep2.Copied), len(rep2.Present))
}

func TestCopyFromSourceCorruptContent(t *testing.T) {
	src, idx, _, _ := buildMultiArchStore(t, "app", "latest")
	// 保留摘要文件名但覆写内容，制造“声明摘要与实际内容对不上”
	corruptLayer := DigestOf([]byte("layer-arm64"))
	if err := os.WriteFile(src.blobPath(corruptLayer), []byte("corrupted-bytes-not-matching-digest"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("corrupted on-disk blob keeps digest name %s", corruptLayer)

	dst := newTestStore(t)
	_, err := dst.CopyFrom(context.Background(), src, "app", "latest")
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("want ErrDigestMismatch for corrupted layer, got %v", err)
	}
	t.Logf("corrupt content rejected distinctly: %v", err)
	if _, lerr := dst.Lookup("app", "latest"); !errors.Is(lerr, ErrNameNotFound) {
		t.Fatalf("no half tag allowed, got %v", lerr)
	}
	_ = idx
}

func TestCopyFromUnknownName(t *testing.T) {
	src, _, _, _ := buildMultiArchStore(t, "app", "latest")
	dst := newTestStore(t)
	if _, err := dst.CopyFrom(context.Background(), src, "nope/ghost", "latest"); !errors.Is(err, ErrNameNotFound) {
		t.Fatalf("want ErrNameNotFound, got %v", err)
	}
}

func TestCopyFromStreamsLargeBlobs(t *testing.T) {
	src := newTestStore(t)
	// 一个 20MiB 的大层，验证按流式搬运、大小与内容正确。
	const size = 20 << 20
	zr := &zeroReader{n: size}
	big, err := src.Put(DigestOf(make([]byte, size)), size, zr)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := src.PutBytes([]byte("cfg-big"))
	m := &Manifest{
		MediaType: MediaTypeImageManifest,
		Config:    Descriptor{Digest: cfg.Digest},
		Layers:    []Descriptor{{Digest: big.Digest, Size: big.Size}},
	}
	raw, _ := json.Marshal(m)
	mInfo, _ := src.PutBytes(raw)
	if err := src.Tag("big/app", "latest", mInfo.Digest); err != nil {
		t.Fatal(err)
	}

	dst := newTestStore(t)
	rep, err := dst.CopyFrom(context.Background(), src, "big/app", "latest")
	if err != nil {
		t.Fatalf("copy big: %v", err)
	}
	if !dst.Has(big.Digest) {
		t.Fatal("big layer missing in dst")
	}
	fi, err := os.Stat(dst.blobPath(big.Digest))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != size {
		t.Fatalf("big layer size = %d, want %d", fi.Size(), size)
	}
	// 逐块比对内容
	rc, _, err := dst.Get(big.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	buf := make([]byte, 1<<16)
	var total int64
	for {
		n, rerr := rc.Read(buf)
		for _, b := range buf[:n] {
			if b != 0 {
				t.Fatal("big layer contains non-zero byte")
			}
		}
		total += int64(n)
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			t.Fatal(rerr)
		}
	}
	if total != size {
		t.Fatalf("read back %d bytes, want %d", total, size)
	}
	t.Logf("streamed big layer size=%d in %d blobs (32KiB buffers, not fully buffered)", size, len(rep.Copied))
}

type zeroReader struct{ n int64 }

func (z *zeroReader) Read(p []byte) (int, error) {
	if z.n <= 0 {
		return 0, io.EOF
	}
	c := int64(len(p))
	if c > z.n {
		c = z.n
	}
	for i := range p[:c] {
		p[i] = 0
	}
	z.n -= c
	return int(c), nil
}

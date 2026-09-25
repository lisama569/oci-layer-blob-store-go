package ocistore

import (
	"context"
	"errors"
	"testing"
)

// buildSimpleImage 写入 config+单一层的镜像，返回清单摘要。
func buildSimpleImage(t *testing.T, s *Store, name string) Digest {
	t.Helper()
	return buildManifest(t, s,
		[]byte(`{"name":"`+name+`"}`),
		[]byte("layer-"+name))
}

func TestResolveTagAndPlatform(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	amd := buildSimpleImage(t, s, "amd64")
	arm := buildSimpleImage(t, s, "arm64v8")

	indexD := buildIndex(t, s, []IndexEntry{
		{Platform: Platform{OS: "linux", Architecture: "amd64"}, Digest: amd},
		{Platform: Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}, Digest: arm},
	})
	if _, err := s.Tag(ctx, "multiarch:v1", indexD); err != nil {
		t.Fatalf("tag: %v", err)
	}
	logRefs(t, s)

	cases := []struct {
		platform Platform
		want     Digest
	}{
		{Platform{OS: "linux", Architecture: "amd64"}, amd},
		{Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}, arm},
		// 目标未指定 variant 时接受任意 variant。
		{Platform{OS: "linux", Architecture: "arm64"}, arm},
	}
	for _, c := range cases {
		res, err := s.Resolve(ctx, "multiarch:v1", c.platform)
		if err != nil {
			t.Fatalf("resolve %s: %v", c.platform, err)
		}
		if res.ManifestDigest != c.want {
			t.Fatalf("platform %s resolved %s want %s", c.platform, res.ManifestDigest, c.want)
		}
		t.Logf("解析依据: tag=multiarch:v1 want=%s -> manifest=%s", c.platform, res.ManifestDigest)
	}
}

func TestResolvePlatformNotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	amd := buildSimpleImage(t, s, "amd64")
	indexD := buildIndex(t, s, []IndexEntry{
		{Platform: Platform{OS: "linux", Architecture: "amd64"}, Digest: amd},
	})
	if _, err := s.Tag(ctx, "img", indexD); err != nil {
		t.Fatal(err)
	}

	want := Platform{OS: "windows", Architecture: "amd64"}
	_, err := s.Resolve(ctx, "img", want)
	var pnf *PlatformNotFoundError
	if !errors.As(err, &pnf) {
		t.Fatalf("want PlatformNotFoundError, got %v", err)
	}
	t.Logf("平台缺失判定: want=%s available=%v err=%v", pnf.Wanted, pnf.Available, err)

	// 未知 tag。
	if _, err := s.Resolve(ctx, "nope:latest", Platform{}); !errors.Is(err, ErrRefNotFound) {
		t.Fatalf("want ErrRefNotFound, got %v", err)
	}
}

func TestResolveDanglingIndexEntry(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	missing := mustParseDigest(t, "sha256:"+repeat0(32))
	indexD := buildIndex(t, s, []IndexEntry{
		{Platform: Platform{OS: "linux", Architecture: "amd64"}, Digest: missing},
	})
	if _, err := s.Tag(ctx, "broken", indexD); err != nil {
		t.Fatal(err)
	}

	_, err := s.Resolve(ctx, "broken", Platform{OS: "linux", Architecture: "amd64"})
	var dangle *DanglingReferenceError
	if !errors.As(err, &dangle) {
		t.Fatalf("want DanglingReferenceError, got %v", err)
	}
	if dangle.Missing != missing {
		t.Fatalf("dangling missing=%s want %s", dangle.Missing, missing)
	}
	t.Logf("悬空引用判定: location=%s missing=%s", dangle.Location, dangle.Missing)
}

func TestResolveDanglingTag(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	missing := mustParseDigest(t, "sha256:"+repeat0(32))
	// 直接写一个 ref 文件指向缺失 blob，模拟底层被外部破坏。
	p, err := s.refPath("ghost")
	if err != nil {
		t.Fatal(err)
	}
	writeRefFile(t, p, refRecord{MediaType: MediaTypeImageManifest, Digest: missing, Size: 1})

	_, err = s.ResolveTag(ctx, "ghost")
	if !errors.Is(err, ErrDanglingReference) {
		t.Fatalf("want dangling tag, got %v", err)
	}
	t.Logf("悬空 tag 判定: ghost -> %s err=%v", missing, err)
}

func TestPutManifestUnsupportedMediaType(t *testing.T) {
	s := newTestStore(t)
	_, err := s.PutManifest(context.Background(), "application/octet-stream", []byte("{}"))
	if !errors.Is(err, ErrUnsupportedMediaType) {
		t.Fatalf("want unsupported media type, got %v", err)
	}

	// 直接写一段非清单内容，再按摘要解析，应明确报不支持类型。
	d := putLayer(t, s, []byte("not-a-manifest"))
	if _, err := s.Tag(context.Background(), "weird", d.Digest); !errors.Is(err, ErrUnsupportedMediaType) {
		t.Fatalf("tag non-manifest want unsupported, got %v", err)
	}
	t.Logf("不支持类型判定: blob=%s err=%v", d.Digest, err)
}

func TestResolveVariantMustMatchAndNestedIndex(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	v7 := buildSimpleImage(t, s, "armv7")
	v8 := buildSimpleImage(t, s, "armv8")

	// 内层 index：只含 arm v7。
	inner := buildIndex(t, s, []IndexEntry{
		{Platform: Platform{OS: "linux", Architecture: "arm", Variant: "v7"}, Digest: v7},
	})
	// 外层 index：一个 arm v8 条目 + 一个指向下层 index 的 arm v7 条目（嵌套）。
	outer := buildIndex(t, s, []IndexEntry{
		{Platform: Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}, Digest: v8},
		{Platform: Platform{OS: "linux", Architecture: "arm", Variant: "v7"}, Digest: inner},
	})
	if _, err := s.Tag(ctx, "nested", outer); err != nil {
		t.Fatal(err)
	}

	// 指定 v7 必须精确命中 v7，而不能落到 v8。
	res, err := s.Resolve(ctx, "nested", Platform{OS: "linux", Architecture: "arm", Variant: "v7"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ManifestDigest != v7 {
		t.Fatalf("variant v7 resolved %s want %s（不得任意挑选）", res.ManifestDigest, v7)
	}
	t.Logf("嵌套 index 精确解析: nested -> inner %s -> manifest %s", inner, v7)

	// 请求一个存在 arch 但 variant 不符的平台：必须失败，不得降级。
	_, err = s.Resolve(ctx, "nested", Platform{OS: "linux", Architecture: "arm64", Variant: "v9"})
	var pnf *PlatformNotFoundError
	if !errors.As(err, &pnf) {
		t.Fatalf("variant mismatch want PlatformNotFoundError, got %v", err)
	}
	t.Logf("variant 不符判定: want=%s available=%v err=%v", pnf.Wanted, pnf.Available, err)
}

func TestResolveByDigest(t *testing.T) {
	s := newTestStore(t)
	d := buildSimpleImage(t, s, "direct")
	res, err := s.Resolve(context.Background(), string(d), Platform{})
	if err != nil {
		t.Fatal(err)
	}
	if res.ManifestDigest != d {
		t.Fatalf("digest resolve got %s want %s", res.ManifestDigest, d)
	}
	t.Logf("按摘要解析: %s -> manifest", d)
}

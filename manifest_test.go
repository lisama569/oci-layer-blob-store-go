package ocistore

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestParseManifest(t *testing.T) {
	raw := []byte(`{
      "mediaType":"application/vnd.oci.image.manifest.v1+json",
      "config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":1},
      "layers":[
        {"digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","size":2},
        {"digest":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","size":3}
      ]}`)
	doc, err := ParseManifestOrIndex(raw)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := doc.(*Manifest)
	if !ok {
		t.Fatalf("want *Manifest, got %T", doc)
	}
	refs := m.Refs()
	t.Logf("manifest refs: %v", refs)
	if len(refs) != 3 {
		t.Fatalf("want 3 refs, got %d", len(refs))
	}
}

func TestSelectPlatformMultiArch(t *testing.T) {
	idx := &Index{
		MediaType: MediaTypeImageIndex,
		Manifests: []Descriptor{
			{Digest: DigestOf([]byte("amd64")), Platform: &Platform{OS: "linux", Architecture: "amd64"}},
			{Digest: DigestOf([]byte("armv7")), Platform: &Platform{OS: "linux", Architecture: "arm", Variant: "v7"}},
			{Digest: DigestOf([]byte("armv8")), Platform: &Platform{OS: "linux", Architecture: "arm64"}},
			{Digest: DigestOf([]byte("attest")), Platform: nil},
		},
	}
	for _, tc := range []struct {
		p       Platform
		content string
	}{
		{Platform{OS: "linux", Architecture: "amd64"}, "amd64"},
		{Platform{OS: "linux", Architecture: "arm", Variant: "v7"}, "armv7"},
		{Platform{OS: "linux", Architecture: "arm64"}, "armv8"},
	} {
		d, err := SelectPlatform(idx, tc.p)
		if err != nil {
			t.Fatalf("select %+v: %v", tc.p, err)
		}
		if d.Digest != DigestOf([]byte(tc.content)) {
			t.Fatalf("platform %+v resolved to %s, want content %q", tc.p, d.Digest, tc.content)
		}
		t.Logf("platform %+v -> %s", tc.p, d.Digest)
	}

	if _, err := SelectPlatform(idx, Platform{OS: "windows", Architecture: "amd64"}); !errors.Is(err, ErrPlatformNotFound) {
		t.Fatalf("want ErrPlatformNotFound, got %v", err)
	}
	t.Logf("missing platform windows/amd64 correctly refused among %d entries", len(idx.Manifests))
}

func TestUnsupportedMediaType(t *testing.T) {
	if _, err := ParseManifestOrIndex([]byte(`{"mediaType":"application/vnd.unknown+json"}`)); !errors.Is(err, ErrUnsupportedMediaType) {
		t.Fatalf("want ErrUnsupportedMediaType, got %v", err)
	}
	t.Log("unknown media type rejected")
}

// buildChild 在存储中写入一个子 manifest 及其 config/layer。
func buildChild(t *testing.T, s *Store, arch string) Digest {
	t.Helper()
	cfg, err := s.PutBytes([]byte("cfg-" + arch))
	if err != nil {
		t.Fatal(err)
	}
	layer, err := s.PutBytes([]byte("layer-" + arch))
	if err != nil {
		t.Fatal(err)
	}
	m := &Manifest{
		MediaType: MediaTypeImageManifest,
		Config:    Descriptor{Digest: cfg.Digest},
		Layers:    []Descriptor{{Digest: layer.Digest}},
	}
	raw, _ := json.Marshal(m)
	info, err := s.PutBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("child manifest arch=%s digest=%s refs=[%s,%s]", arch, info.Digest, cfg.Digest, layer.Digest)
	return info.Digest
}

func TestResolveIndex(t *testing.T) {
	s := newTestStore(t)
	dAMD64 := buildChild(t, s, "amd64")
	dARM64 := buildChild(t, s, "arm64")

	idx := &Index{
		MediaType: MediaTypeImageIndex,
		Manifests: []Descriptor{
			{Digest: dAMD64, Platform: &Platform{OS: "linux", Architecture: "amd64"}},
			{Digest: dARM64, Platform: &Platform{OS: "linux", Architecture: "arm64"}},
		},
	}
	idxRaw, _ := json.Marshal(idx)
	idxInfo, err := s.PutBytes(idxRaw)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("index digest=%s -> [%s=amd64, %s=arm64]", idxInfo.Digest, dAMD64, dARM64)

	m, err := s.Resolve(idxInfo.Digest, Platform{OS: "linux", Architecture: "arm64"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if m.Config.Digest != DigestOf([]byte("cfg-arm64")) {
		t.Fatalf("resolved wrong child, config=%s", m.Config.Digest)
	}
	t.Logf("resolved arm64 config=%s layer=%s", m.Config.Digest, m.Layers[0].Digest)

	if _, err := s.Resolve(idxInfo.Digest, Platform{OS: "linux", Architecture: "riscv64"}); !errors.Is(err, ErrPlatformNotFound) {
		t.Fatalf("want ErrPlatformNotFound, got %v", err)
	}
	t.Log("riscv64 platform resolution refused")
}

func TestResolveDanglingRef(t *testing.T) {
	s := newTestStore(t)
	ghost := DigestOf([]byte("never-written"))

	// index 引用一个不存在的 manifest blob
	idx := &Index{
		MediaType: MediaTypeImageIndex,
		Manifests: []Descriptor{
			{Digest: ghost, Platform: &Platform{OS: "linux", Architecture: "amd64"}},
		},
	}
	idxRaw, _ := json.Marshal(idx)
	idxInfo, err := s.PutBytes(idxRaw)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("index %s has dangling entry %s", idxInfo.Digest, ghost)
	if _, err := s.Resolve(idxInfo.Digest, Platform{OS: "linux", Architecture: "amd64"}); !errors.Is(err, ErrDanglingRef) {
		t.Fatalf("want ErrDanglingRef, got %v", err)
	}

	// manifest 引用不存在的层
	cfg, _ := s.PutBytes([]byte("cfg"))
	mDangling := &Manifest{
		MediaType: MediaTypeImageManifest,
		Config:    Descriptor{Digest: cfg.Digest},
		Layers:    []Descriptor{{Digest: ghost}},
	}
	if err := VerifyRefs(s, mDangling); !errors.Is(err, ErrDanglingRef) {
		t.Fatalf("want ErrDanglingRef for missing layer, got %v", err)
	}
	t.Logf("dangling layer ref %s rejected", ghost)
}

package ocistore

import (
	"encoding/json"
	"fmt"
	"testing"
)

// buildMultiArchImage 在 s 中构造一个两平台镜像：
// index -> {manifestAMD64, manifestARM64} -> config + layers，
// 返回 index 摘要与全部内容摘要。
func buildMultiArchImage(t *testing.T, s *Store, tag string) (Digest, []Digest) {
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
	manifestFor := func(os, arch string) Digest {
		cfg := put([]byte(fmt.Sprintf("cfg-%s-%s", os, arch)))
		layer := put([]byte(fmt.Sprintf("layer-%s-%s", os, arch)))
		m := Manifest{
			MediaType: MediaTypeImageManifest,
			Config:    Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: cfg},
			Layers:    []Descriptor{{MediaType: "application/vnd.oci.image.layer.v1.tar", Digest: layer}},
		}
		data, _ := json.Marshal(m)
		return put(data)
	}
	mAMD := manifestFor("linux", "amd64")
	mARM := manifestFor("linux", "arm64")
	idx := Index{
		MediaType: MediaTypeImageIndex,
		Manifests: []Descriptor{
			{MediaType: MediaTypeImageManifest, Digest: mAMD, Platform: &Platform{OS: "linux", Architecture: "amd64"}},
			{MediaType: MediaTypeImageManifest, Digest: mARM, Platform: &Platform{OS: "linux", Architecture: "arm64"}},
		},
	}
	idxData, _ := json.Marshal(idx)
	idxDigest := put(idxData)
	if err := s.Tag(tag, idxDigest); err != nil {
		t.Fatal(err)
	}
	return idxDigest, all
}

func logSweep(t *testing.T, label string, rep SweepReport) {
	t.Helper()
	t.Logf("%s (dryRun=%v):", label, rep.DryRun)
	for _, e := range rep.Kept {
		t.Logf("  KEEP   %s  <- %s", e.Digest, e.Reason)
	}
	for _, e := range rep.Removed {
		t.Logf("  REMOVE %s  <- %s", e.Digest, e.Reason)
	}
}

func TestSweepFromRegisteredRefs(t *testing.T) {
	s := newTestStore(t)

	// 登记的多架构镜像：index 链上全部内容都应存活
	_, chainDigests := buildMultiArchImage(t, s, "app/web:latest")
	// 未登记的孤儿内容
	orphan, _ := s.PutBytes([]byte("orphan-layer"))

	t.Logf("registered chain has %d blobs; orphan=%s", len(chainDigests), orphan.Digest)

	// 预演：只判定不删除
	dry, err := s.Sweep(t.Context(), SweepOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	logSweep(t, "sweep dry-run", dry)
	if len(dry.Removed) != 1 || dry.Removed[0].Digest != orphan.Digest {
		t.Fatalf("dry-run should flag only orphan, got %v", dry.Removed)
	}
	if !s.Has(orphan.Digest) {
		t.Fatal("dry-run must not delete anything")
	}

	// 正式回收
	rep, err := s.Sweep(t.Context(), SweepOptions{})
	if err != nil {
		t.Fatal(err)
	}
	logSweep(t, "sweep real", rep)
	if s.Has(orphan.Digest) {
		t.Fatal("orphan must be removed")
	}
	for _, d := range chainDigests {
		if !s.Has(d) {
			t.Fatalf("referenced blob %s wrongly removed", d)
		}
	}
	// 保留理由必须能追到登记名
	for _, e := range rep.Kept {
		if e.Reason != "referenced by app/web:latest" {
			t.Fatalf("unexpected keep reason %q for %s", e.Reason, e.Digest)
		}
	}

	// 幂等：再跑一次无可删
	rep2, err := s.Sweep(t.Context(), SweepOptions{})
	if err != nil {
		t.Fatal(err)
	}
	logSweep(t, "sweep idempotent repeat", rep2)
	if len(rep2.Removed) != 0 {
		t.Fatalf("repeat sweep must be idempotent, removed %v", rep2.Removed)
	}
}

func TestSweepFollowsLatestTag(t *testing.T) {
	s := newTestStore(t)

	// v1：单 manifest 镜像
	cfg1, _ := s.PutBytes([]byte("cfg-v1"))
	layer1, _ := s.PutBytes([]byte("layer-v1"))
	m1 := Manifest{MediaType: MediaTypeImageManifest,
		Config: Descriptor{Digest: cfg1.Digest},
		Layers: []Descriptor{{Digest: layer1.Digest}}}
	m1Data, _ := json.Marshal(m1)
	m1Info, _ := s.PutBytes(m1Data)
	if err := s.Tag("app/svc:latest", m1Info.Digest); err != nil {
		t.Fatal(err)
	}

	// v2：改指向后，v1 的独占内容应被回收
	cfg2, _ := s.PutBytes([]byte("cfg-v2"))
	layer2, _ := s.PutBytes([]byte("layer-v2"))
	m2 := Manifest{MediaType: MediaTypeImageManifest,
		Config: Descriptor{Digest: cfg2.Digest},
		Layers: []Descriptor{{Digest: layer2.Digest}}}
	m2Data, _ := json.Marshal(m2)
	m2Info, _ := s.PutBytes(m2Data)
	if err := s.Tag("app/svc:latest", m2Info.Digest); err != nil {
		t.Fatal(err)
	}

	rep, err := s.Sweep(t.Context(), SweepOptions{})
	if err != nil {
		t.Fatal(err)
	}
	logSweep(t, "sweep after repoint", rep)
	for _, d := range []Digest{m1Info.Digest, cfg1.Digest, layer1.Digest} {
		if s.Has(d) {
			t.Fatalf("stale v1 blob %s must be collected after repoint", d)
		}
	}
	for _, d := range []Digest{m2Info.Digest, cfg2.Digest, layer2.Digest} {
		if !s.Has(d) {
			t.Fatalf("live v2 blob %s wrongly removed", d)
		}
	}
}

func TestSweepKeepsPinnedBlob(t *testing.T) {
	s := newTestStore(t)
	orphan, _ := s.PutBytes([]byte("orphan-being-read"))

	// 进行中读取钉住该 blob，回收不得删除
	rc, _, err := s.Get(orphan.Digest)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := s.Sweep(t.Context(), SweepOptions{})
	if err != nil {
		t.Fatal(err)
	}
	logSweep(t, "sweep with pinned orphan", rep)
	if !s.Has(orphan.Digest) {
		t.Fatal("pinned blob must survive sweep")
	}
	found := false
	for _, e := range rep.Kept {
		if e.Digest == orphan.Digest && e.Reason == "pinned by in-flight read" {
			found = true
		}
	}
	if !found {
		t.Fatalf("pinned keep reason missing in report: %v", rep.Kept)
	}
	rc.Close()

	// 关闭读取后再回收即可删除
	rep2, _ := s.Sweep(t.Context(), SweepOptions{})
	logSweep(t, "sweep after unpin", rep2)
	if s.Has(orphan.Digest) {
		t.Fatal("unpinned orphan must be collected")
	}
}

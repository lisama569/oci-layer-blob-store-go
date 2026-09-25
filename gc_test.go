package ocistore

import (
	"testing"
)

func TestGCSharingAndIdempotent(t *testing.T) {
	s := newTestStore(t)

	// 基础镜像 A：base-cfg + base-layer
	baseCfg, _ := s.PutBytes([]byte("base-config"))
	baseLayer, _ := s.PutBytes([]byte("shared-base-layer"))
	// 镜像 A 独占的层
	layerA, _ := s.PutBytes([]byte("only-in-A"))
	// 镜像 B 独占的层
	layerB, _ := s.PutBytes([]byte("only-in-B"))
	// 完全没有引用的孤儿层
	orphan, _ := s.PutBytes([]byte("nobody-references-me"))

	mA := &Manifest{
		MediaType: MediaTypeImageManifest,
		Config:    Descriptor{Digest: baseCfg.Digest},
		Layers:    []Descriptor{{Digest: baseLayer.Digest}, {Digest: layerA.Digest}},
	}
	mB := &Manifest{
		MediaType: MediaTypeImageManifest,
		Config:    Descriptor{Digest: baseCfg.Digest}, // 共享 config
		Layers:    []Descriptor{{Digest: baseLayer.Digest}, {Digest: layerB.Digest}},
	}

	t.Logf("reference graph:")
	t.Logf("  mA -> %s,%s,%s", baseCfg.Digest, baseLayer.Digest, layerA.Digest)
	t.Logf("  mB -> %s,%s,%s", baseCfg.Digest, baseLayer.Digest, layerB.Digest)
	t.Logf("  orphan (unreferenced) -> %s", orphan.Digest)

	// 第一次回收：只应删除 orphan
	rep1, err := s.Collect(t.Context(), []*Manifest{mA, mB})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("gc#1 kept=%v removed=%v", rep1.Kept, rep1.Removed)
	if len(rep1.Removed) != 1 || rep1.Removed[0] != orphan.Digest {
		t.Fatalf("gc#1 should remove only orphan, got %v", rep1.Removed)
	}
	if s.Has(orphan.Digest) {
		t.Fatal("orphan must be gone")
	}
	// 共享层必须保留
	for _, d := range []Digest{baseCfg.Digest, baseLayer.Digest, layerA.Digest, layerB.Digest} {
		if !s.Has(d) {
			t.Fatalf("shared/referenced blob %s wrongly removed", d)
		}
	}

	// 删除镜像 A 后回收：layerA 失去引用应被删；共享层仍被 B 引用，保留
	rep2, err := s.Collect(t.Context(), []*Manifest{mB})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("gc#2 (only mB alive) kept=%v removed=%v", rep2.Kept, rep2.Removed)
	if len(rep2.Removed) != 1 || rep2.Removed[0] != layerA.Digest {
		t.Fatalf("gc#2 should remove only layerA, got %v", rep2.Removed)
	}
	if !s.Has(baseLayer.Digest) || !s.Has(baseCfg.Digest) {
		t.Fatal("shared base blobs must survive while mB references them")
	}

	// 幂等：第三次执行（仍只有 mB）应无删除
	rep3, err := s.Collect(t.Context(), []*Manifest{mB})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("gc#3 (idempotent repeat) kept=%v removed=%v", rep3.Kept, rep3.Removed)
	if len(rep3.Removed) != 0 {
		t.Fatalf("repeat gc must be idempotent, but removed %v", rep3.Removed)
	}

	// 全部镜像消失：剩余引用内容全部回收
	rep4, err := s.Collect(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("gc#4 (no manifests) removed=%v", rep4.Removed)
	blobs, _ := s.ListBlobs()
	if len(blobs) != 0 {
		t.Fatalf("store should be empty after all manifests gone, got %d blobs", len(blobs))
	}
}

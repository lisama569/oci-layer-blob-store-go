package ocistore

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
)

// 构造一个平台子镜像（config+layer+manifest），返回 manifest 摘要。
func buildTaggedChild(t *testing.T, s *Store, arch string) Digest {
	t.Helper()
	return buildChild(t, s, arch)
}

func buildIndex(t *testing.T, s *Store, children ...Digest) Digest {
	t.Helper()
	descs := make([]Descriptor, 0, len(children))
	arches := []string{"amd64", "arm64", "386"}
	for i, c := range children {
		arch := arches[i%len(arches)]
		descs = append(descs, Descriptor{Digest: c, Platform: &Platform{OS: "linux", Architecture: arch}})
	}
	idx := &Index{MediaType: MediaTypeImageIndex, Manifests: descs}
	raw, _ := json.Marshal(idx)
	info, err := s.PutBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	return info.Digest
}

func logCollectRound(t *testing.T, round string, rep CollectResult) {
	t.Helper()
	t.Logf("gc[%s] dryRun=%v roots=%d live=%d pinned=%d removed=%v",
		round, rep.DryRun, len(rep.Roots), len(rep.Live), len(rep.Pinned), rep.Removed)
	for _, d := range rep.Roots {
		t.Logf("  root  %s", d)
	}
}

func TestCollectRegisteredMultiArch(t *testing.T) {
	s := newTestStore(t)

	dAMD := buildTaggedChild(t, s, "amd64")
	dARM := buildTaggedChild(t, s, "arm64")
	idx := buildIndex(t, s, dAMD, dARM)

	// 额外的一个孤儿 blob 和一个悬空 manifest 链（不被任何标签引用）
	orphan, _ := s.PutBytes([]byte("orphan-blob"))
	extraCfg, _ := s.PutBytes([]byte("extra-cfg"))
	extraLayer, _ := s.PutBytes([]byte("extra-layer"))
	extraM := &Manifest{
		MediaType: MediaTypeImageManifest,
		Config:    Descriptor{Digest: extraCfg.Digest},
		Layers:    []Descriptor{{Digest: extraLayer.Digest}},
	}
	extraRaw, _ := json.Marshal(extraM)
	extraInfo, _ := s.PutBytes(extraRaw)

	if err := s.Tag("multi/app", "1.0", idx); err != nil {
		t.Fatal(err)
	}
	t.Logf("tag multi/app:1.0 -> index %s", idx)

	// 期望可达集合：index + 2 子 manifest + 2 cfg + 2 layer = 7
	wantLive := map[Digest]bool{
		idx: true, dAMD: true, dARM: true,
		DigestOf([]byte("cfg-amd64")): true, DigestOf([]byte("layer-amd64")): true,
		DigestOf([]byte("cfg-arm64")): true, DigestOf([]byte("layer-arm64")): true,
	}

	// 预演：不删任何东西
	pre, err := s.CollectRegistered(context.Background(), CollectOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	logCollectRound(t, "dryrun", pre)
	if !pre.DryRun || len(pre.Removed) != 4 {
		t.Fatalf("dry-run should report 4 removable, got %+v", pre)
	}
	for d := range wantLive {
		if !s.Has(d) {
			t.Fatalf("live blob missing before gc: %s", d)
		}
	}
	for _, d := range []Digest{orphan.Digest, extraCfg.Digest, extraLayer.Digest, extraInfo.Digest} {
		if !s.Has(d) {
			t.Fatalf("dry-run must not delete %s", d)
		}
	}

	// 正式回收
	rep, err := s.CollectRegistered(context.Background(), CollectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	logCollectRound(t, "real#1", rep)
	for d := range wantLive {
		if !s.Has(d) {
			t.Fatalf("reachable blob %s wrongly removed", d)
		}
	}
	for _, d := range []Digest{orphan.Digest, extraCfg.Digest, extraLayer.Digest, extraInfo.Digest} {
		if s.Has(d) {
			t.Fatalf("unreachable blob %s should be removed", d)
		}
	}
	if len(rep.Live) != 7 {
		t.Fatalf("want 7 live, got %d", len(rep.Live))
	}

	// 幂等：再跑一轮无删除
	rep2, err := s.CollectRegistered(context.Background(), CollectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	logCollectRound(t, "real#2-idempotent", rep2)
	if len(rep2.Removed) != 0 {
		t.Fatalf("repeat collect must remove nothing, got %v", rep2.Removed)
	}
}

func TestCollectRegisteredRetagChangesReachability(t *testing.T) {
	s := newTestStore(t)

	dAMD := buildTaggedChild(t, s, "amd64")
	dARM := buildTaggedChild(t, s, "arm64")
	idx := buildIndex(t, s, dAMD, dARM)

	// 单平台镜像
	singleCfg, _ := s.PutBytes([]byte("cfg-single"))
	singleLayer, _ := s.PutBytes([]byte("layer-single"))
	singleM := &Manifest{
		MediaType: MediaTypeImageManifest,
		Config:    Descriptor{Digest: singleCfg.Digest},
		Layers:    []Descriptor{{Digest: singleLayer.Digest}},
	}
	singleRaw, _ := json.Marshal(singleM)
	singleInfo, _ := s.PutBytes(singleRaw)

	if err := s.Tag("app", "latest", idx); err != nil {
		t.Fatal(err)
	}
	if err := s.Tag("app", "single", singleInfo.Digest); err != nil {
		t.Fatal(err)
	}

	rep, err := s.CollectRegistered(context.Background(), CollectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	logCollectRound(t, "both-tags", rep)

	// 改指向：删掉 single 标签后，单平台镜像内容应变为不可达
	if err := s.Untag("app", "single"); err != nil {
		t.Fatal(err)
	}
	rep2, err := s.CollectRegistered(context.Background(), CollectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	logCollectRound(t, "after-untag-single", rep2)
	for _, d := range []Digest{singleInfo.Digest, singleCfg.Digest, singleLayer.Digest} {
		if s.Has(d) {
			t.Fatalf("blob %s should become unreachable after untag", d)
		}
	}
	for _, d := range []Digest{idx, dAMD, dARM} {
		if !s.Has(d) {
			t.Fatal("multi-arch chain must stay reachable via latest")
		}
	}

	// latest 改指向：重新放一份单平台镜像（上一轮被回收了），
	// 然后让 latest 指向它，多架构链整体失活。
	sc2, _ := s.PutBytes([]byte("cfg-single"))
	sl2, _ := s.PutBytes([]byte("layer-single"))
	m2 := &Manifest{
		MediaType: MediaTypeImageManifest,
		Config:    Descriptor{Digest: sc2.Digest},
		Layers:    []Descriptor{{Digest: sl2.Digest}},
	}
	raw2, _ := json.Marshal(m2)
	si2, _ := s.PutBytes(raw2)
	if err := s.Tag("app", "latest", si2.Digest); err != nil {
		t.Fatal(err)
	}
	rep3, err := s.CollectRegistered(context.Background(), CollectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	logCollectRound(t, "after-retag-latest-to-single", rep3)
	for _, d := range []Digest{idx, dAMD, dARM} {
		if s.Has(d) {
			t.Fatalf("multi-arch blob %s should be collected after latest moved away", d)
		}
	}
}

func TestCollectRegisteredProtectsPinnedReads(t *testing.T) {
	s := newTestStore(t)
	// 一个最小的真实镜像作为登记根
	root := buildTaggedChild(t, s, "amd64")
	if err := s.Tag("app", "latest", root); err != nil {
		t.Fatal(err)
	}
	// 一个不可达 blob，在回收期间被读取
	pinned, _ := s.PutBytes([]byte("being-read"))

	const readers = 8
	var wg sync.WaitGroup
	var ready sync.WaitGroup
	release := make(chan struct{})
	ready.Add(readers)
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rc, _, err := s.Get(pinned.Digest)
			if err != nil {
				t.Errorf("get: %v", err)
				ready.Done()
				return
			}
			ready.Done()
			<-release // 持有读取句柄（pin）直到 GC 判定完成
			got, rerr := io.ReadAll(rc)
			cerr := rc.Close()
			if rerr != nil || cerr != nil || string(got) != "being-read" {
				t.Errorf("read corrupted: %q %v %v", got, rerr, cerr)
			}
		}()
	}
	ready.Wait() // 所有读取句柄都已拿到，pinned 必然 > 0
	rep, err := s.CollectRegistered(context.Background(), CollectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	wg.Wait()
	logCollectRound(t, "pinned-read", rep)
	if len(rep.Pinned) != 1 || rep.Pinned[0] != pinned.Digest {
		t.Fatalf("want pinned %s, got pinned=%v removed=%v", pinned.Digest, rep.Pinned, rep.Removed)
	}
	if !s.Has(pinned.Digest) {
		t.Fatal("pinned blob must survive gc")
	}
	// 读取结束后再回收，应被删除
	rep2, _ := s.CollectRegistered(context.Background(), CollectOptions{})
	logCollectRound(t, "after-read-done", rep2)
	if s.Has(pinned.Digest) {
		t.Fatal("blob must be collected once reads finish")
	}
}

func TestCollectRegisteredDanglingRootAborts(t *testing.T) {
	s := newTestStore(t)
	doc, _ := s.PutBytes([]byte("doc"))
	orphan, _ := s.PutBytes([]byte("orphan"))
	if err := s.Tag("app", "latest", doc.Digest); err != nil {
		t.Fatal(err)
	}
	// 直接从磁盘删掉被标签引用的内容，制造悬空根
	if err := s.remove(doc.Digest); err != nil {
		t.Fatal(err)
	}
	rep, err := s.CollectRegistered(context.Background(), CollectOptions{})
	if !errors.Is(err, ErrDanglingRef) {
		t.Fatalf("want ErrDanglingRef, got %v (rep=%+v)", err, rep)
	}
	t.Logf("dangling root aborted gc: %v", err)
	// 中止时不得误删任何东西
	if !s.Has(orphan.Digest) {
		t.Fatal("gc must not delete anything when a root is dangling")
	}
}

func TestCollectRegisteredNoTagsCollectsAll(t *testing.T) {
	s := newTestStore(t)
	a, _ := s.PutBytes([]byte("a"))
	b, _ := s.PutBytes([]byte("b"))
	rep, err := s.CollectRegistered(context.Background(), CollectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	logCollectRound(t, "no-tags", rep)
	if s.Has(a.Digest) || s.Has(b.Digest) {
		t.Fatal("with no registered names everything is unreachable")
	}
}

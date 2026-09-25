package ocistore

import (
	"context"
	"errors"
	"io"
	"sort"
	"sync"
	"testing"
)

func TestGCSharedLayersAndCollect(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// 镜像 A、B 共享同一层；镜像 C 是独立内容。
	shared := []byte("shared-base-layer")
	sharedD := digestOf(shared)
	a := buildManifest(t, s, []byte(`{"image":"a"}`), shared, []byte("layer-a-only"))
	b := buildManifest(t, s, []byte(`{"image":"b"}`), shared) // 与 A 共享层
	c := buildManifest(t, s, []byte(`{"image":"c"}`), []byte("layer-c"))
	if _, err := s.Tag(ctx, "img-a", a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Tag(ctx, "img-b", b); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Tag(ctx, "img-c", c); err != nil {
		t.Fatal(err)
	}
	logRefs(t, s)

	rep1, err := s.GC(ctx)
	if err != nil {
		t.Fatalf("gc1: %v", err)
	}
	if len(rep1.Removed) != 0 {
		t.Fatalf("nothing unreachable yet, removed=%v", rep1.Removed)
	}
	// 共享层必须在可达集合中。
	if _, ok := rep1.Reachable[sharedD]; !ok {
		t.Fatalf("shared layer %s must be reachable", sharedD)
	}
	t.Logf("首次 GC: roots=%d reachable=%d removed=%d; 共享层 %s 保留（被 img-a,img-b 引用）",
		len(rep1.Roots), len(rep1.Reachable), len(rep1.Removed), sharedD)
	logBlobs(t, s, rep1.Reachable)

	// 删除 img-c 后，C 的 manifest/config/layer 应全部变为不可达。
	if err := s.Untag(ctx, "img-c"); err != nil {
		t.Fatal(err)
	}
	rep2, err := s.GC(ctx)
	if err != nil {
		t.Fatalf("gc2: %v", err)
	}
	// 预期删除 3 个：C 的 manifest、config、唯一层。
	if len(rep2.Removed) != 3 {
		t.Fatalf("after untag img-c want 3 removed, got %v", rep2.Removed)
	}
	for _, d := range rep2.Removed {
		if ok, _ := s.HasBlob(ctx, d); ok {
			t.Fatalf("removed blob %s still present", d)
		}
	}
	t.Logf("回收判定: untag=img-c removed=%v（其 manifest/config/layer 不再被任何镜像引用）", rep2.Removed)
	// 共享层依旧保留。
	if ok, _ := s.HasBlob(ctx, sharedD); !ok {
		t.Fatalf("shared layer removed while still referenced")
	}

	// 幂等：再跑一次什么都不删。
	rep3, err := s.GC(ctx)
	if err != nil {
		t.Fatalf("gc3: %v", err)
	}
	if len(rep3.Removed) != 0 {
		t.Fatalf("second GC must be idempotent, removed=%v", rep3.Removed)
	}
	if len(rep3.Reachable) != len(rep2.Reachable) {
		t.Fatalf("reachable set changed between runs: %d vs %d", len(rep2.Reachable), len(rep3.Reachable))
	}
	t.Logf("幂等判定: 第二次 GC reachable=%d removed=0", len(rep3.Reachable))
}

func TestGCKeepsOnlyTagRoots(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	d := buildManifest(t, s, []byte("cfg"), []byte("layer"))
	// 只写入并打 tag 一个镜像；另写入一个“悬空”的未引用清单。
	orphan := buildManifest(t, s, []byte("orphan-cfg"), []byte("orphan-layer"))
	if _, err := s.Tag(ctx, "kept", d); err != nil {
		t.Fatal(err)
	}

	rep, err := s.GC(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// orphan 的 manifest/config/layer 共 3 个应被回收。
	if len(rep.Removed) != 3 {
		t.Fatalf("want orphan manifest+config+layer removed, got %v", rep.Removed)
	}
	wantRemoved := map[Digest]bool{orphan: true, digestOf([]byte("orphan-cfg")): true, digestOf([]byte("orphan-layer")): true}
	for _, r := range rep.Removed {
		if !wantRemoved[r] {
			t.Fatalf("unexpected removed %s", r)
		}
	}
	t.Logf("无 tag 根的内容被回收: %v", rep.Removed)
}

func TestGCDanglingReferenceReported(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	missing := mustParseDigest(t, "sha256:"+repeat0(32))
	p, err := s.refPath("ghost")
	if err != nil {
		t.Fatal(err)
	}
	writeRefFile(t, p, refRecord{MediaType: MediaTypeImageManifest, Digest: missing, Size: 1})

	// 另放一个正常镜像，确保 GC 不因悬空而整体失败。
	ok := buildManifest(t, s, []byte("cfg"), []byte("layer"))
	if _, err := s.Tag(ctx, "good", ok); err != nil {
		t.Fatal(err)
	}

	rep, err := s.GC(ctx)
	if err != nil {
		t.Fatalf("GC should tolerate dangling refs: %v", err)
	}
	found := false
	for _, d := range rep.Dangling {
		t.Logf("悬空引用记录: %v", d)
		if d.Missing == missing && d.Location == "ref:ghost" {
			found = true
		}
	}
	if !found {
		t.Fatalf("dangling ref not reported: %+v", rep.Dangling)
	}
}

func TestGCDoesNotBreakInProgressRead(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// 一个将被回收的孤立层。
	orphanData := []byte("doomed-layer")
	orphanD := putLayer(t, s, orphanData).Digest

	// 打开读取句柄（持有 GC 读锁直到 Close）。
	rc, _, err := s.GetBlob(ctx, orphanD)
	if err != nil {
		t.Fatal(err)
	}

	gcDone := make(chan struct{})
	go func() {
		_, _ = s.GC(ctx) // 必须阻塞到读取结束。
		close(gcDone)
	}()

	// GC 进行中时读取必须完整。
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("in-progress read broken: %v", err)
	}
	if string(b) != string(orphanData) {
		t.Fatalf("in-progress read got %q", b)
	}
	_ = rc.Close()
	<-gcDone

	if ok, _ := s.HasBlob(ctx, orphanD); ok {
		t.Fatalf("orphan blob should be collected after read finished")
	}
	t.Logf("进行中读取完整完成后 GC 才删除 blob=%s（读取未被破坏，GC 未提前动手）", orphanD)
}

func TestGCConcurrentWithNewWrites(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// 预置一个被引用的镜像，持续 GC + 持续写入新 tag/镜像，
	// 结束后所有被 tag 引用的内容必须仍可完整解析。
	base := buildManifest(t, s, []byte("base-cfg"), []byte("base-layer"))
	if _, err := s.Tag(ctx, "base", base); err != nil {
		t.Fatal(err)
	}

	var refs []string
	for i := 0; i < 20; i++ {
		refs = append(refs, "img-"+itoa(i))
	}

	errCh := make(chan error, 2)
	done := make(chan struct{})
	var bg sync.WaitGroup
	bg.Add(2)
	go func() {
		defer bg.Done()
		for i := 0; i < 50; i++ {
			if _, err := s.GC(ctx); err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}
		}
	}()
	go func() {
		defer bg.Done()
		for i, name := range refs {
			// “写清单”和“打 tag”是两步，期间可能撞上 GC 回收尚未被引用
			// 的清单（真实推送中同样可能发生），故按 ErrNotFound 重试。
			retries := 0
			for {
				d := buildManifest(t, s, []byte("cfg-"+itoa(i)), []byte("layer-"+itoa(i)))
				if _, err := s.Tag(ctx, name, d); err != nil {
					if errors.Is(err, ErrNotFound) && retries < 100 {
						retries++
						continue
					}
					select {
					case errCh <- err:
					default:
					}
					return
				}
				if retries > 0 {
					t.Logf("tag %s 在 GC 间隙被回收后重试 %d 次成功", name, retries)
				}
				break
			}
		}
		close(done)
	}()
	<-done
	rep, err := s.GC(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bg.Wait()
	close(errCh)
	for e := range errCh {
		if e != nil {
			t.Fatalf("concurrent gc/write: %v", e)
		}
	}

	sort.Strings(refs)
	for _, name := range refs {
		d, err := s.ResolveTag(ctx, name)
		if err != nil {
			t.Fatalf("tag %s broken after GC: %v", name, err)
		}
		if _, err := s.Resolve(ctx, name, Platform{}); err != nil {
			t.Fatalf("resolve %s after GC: %v", name, err)
		}
		_ = d
	}
	if _, err := s.ResolveTag(ctx, "base"); err != nil {
		t.Fatalf("base tag broken: %v", err)
	}
	t.Logf("GC 与写入交错完成: 最终 reachable=%d removed(last)=%d，全部 tag 仍可解析",
		len(rep.Reachable), len(rep.Removed))
}

func TestUntagMissing(t *testing.T) {
	s := newTestStore(t)
	if err := s.Untag(context.Background(), "nope"); !errors.Is(err, ErrRefNotFound) {
		t.Fatalf("want ErrRefNotFound, got %v", err)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

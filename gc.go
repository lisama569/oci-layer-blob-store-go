package ocistore

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
)

// GCReport 描述一次回收的判定依据，供日志与测试断言。
type GCReport struct {
	// Roots 是本次标记使用的根：tag 名称 -> 根清单摘要。
	Roots map[string]Digest
	// Reachable 为 GC 后仍被引用而保留的 blob 摘要集合。
	Reachable map[Digest]struct{}
	// Removed 为本次实际删除的 blob 摘要（有序）。
	Removed []Digest
	// Dangling 为标记阶段发现的悬空引用（不阻断 GC）。
	Dangling []DanglingReferenceError
}

// GC 执行一次基于引用关系的标记-清除回收。
//
// 规则：
//   - 根集合为 refs 目录下全部命名引用；
//   - 从每个根清单沿 index.manifests / manifest.config / manifest.layers
//     递归标记可达 blob；层与 config 等不可解析内容作为叶子直接标记；
//   - 仅删除标记结束后仍不可达的 blob；悬空引用记录到报告但不阻断回收；
//   - GC 与所有在途读写互斥（见 store.go 的 gcMu），因此不会删掉正在被读
//     的内容，也不会漏标 GC 期间新提交的内容；
//   - 重复执行幂等：第二次 Reachable 相同、Removed 为空。
func (s *Store) GC(ctx context.Context) (GCReport, error) {
	s.gcMu.Lock()
	defer s.gcMu.Unlock()

	rep := GCReport{
		Roots:     map[string]Digest{},
		Reachable: map[Digest]struct{}{},
	}

	// ---- 标记阶段：以 refs 为根 ----
	refsDir := filepath.Join(s.root, "refs")
	entries, err := os.ReadDir(refsDir)
	if err != nil {
		return rep, fmt.Errorf("read refs: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name, derr := url.PathUnescape(e.Name())
		if derr != nil {
			// 名称无法反转义说明不是本存储写入的引用文件，跳过且不入根。
			continue
		}
		desc, rerr := s.readRefLocked(ctx, filepath.Join(refsDir, e.Name()), name)
		if rerr != nil {
			var dangle *DanglingReferenceError
			if errors.As(rerr, &dangle) {
				rep.Dangling = append(rep.Dangling, *dangle)
				continue
			}
			return rep, fmt.Errorf("load ref %s: %w", name, rerr)
		}
		rep.Roots[name] = desc.Digest
		s.markLocked(ctx, desc.Digest, "ref:"+name, &rep)
	}

	// ---- 清除阶段：删除所有不可达 blob ----
	all, err := s.blobs.ListBlobs(ctx)
	if err != nil {
		return rep, err
	}
	for _, d := range all {
		if _, ok := rep.Reachable[d]; ok {
			continue
		}
		if err := s.blobs.DeleteBlob(ctx, d); err != nil {
			return rep, fmt.Errorf("delete unreachable blob %s: %w", d, err)
		}
		rep.Removed = append(rep.Removed, d)
	}
	sort.Slice(rep.Removed, func(i, j int) bool { return rep.Removed[i] < rep.Removed[j] })
	return rep, nil
}

// markLocked 从 d 出发做 DFS 标记。已经标记的节点直接返回以处理
// index 嵌套 / 共享层带来的汇合与环。
func (s *Store) markLocked(ctx context.Context, d Digest, location string, rep *GCReport) {
	if d == "" {
		return
	}
	if _, ok := rep.Reachable[d]; ok {
		return
	}
	// 引用目标缺失：悬空，记录且不标记。
	if _, err := s.blobs.StatBlob(ctx, d); err != nil {
		if errors.Is(err, ErrNotFound) {
			rep.Dangling = append(rep.Dangling, DanglingReferenceError{Location: location, Missing: d})
		}
		return
	}
	rep.Reachable[d] = struct{}{}

	// 尝试作为清单/索引继续向下；不是清单类型则为叶子 blob。
	mt, manifest, index, derr := s.decodeManifest(ctx, d)
	if derr != nil {
		// 无法识别为清单：层、config 等内容，标记即终止。
		_ = derr
		return
	}
	switch mt {
	case MediaTypeImageManifest:
		s.markChildLocked(ctx, manifest.Config, "manifest:"+string(d), rep)
		for _, l := range manifest.Layers {
			s.markChildLocked(ctx, l, "manifest:"+string(d), rep)
		}
	case MediaTypeImageIndex:
		for _, c := range index.Manifests {
			s.markChildLocked(ctx, c, "index:"+string(d), rep)
		}
	}
}

func (s *Store) markChildLocked(ctx context.Context, c Descriptor, location string, rep *GCReport) {
	s.markLocked(ctx, c.Digest, location, rep)
}

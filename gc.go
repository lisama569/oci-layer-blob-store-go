package ocistore

import (
	"context"
	"fmt"
	"sort"
)

// GCReport 记录一次回收的判定依据。
type GCReport struct {
	Kept    []Digest // 仍被引用或被钉住而保留
	Removed []Digest // 本次实际删除
}

// CollectOptions 控制以登记名字为根的回收行为。
type CollectOptions struct {
	DryRun bool // 只做预演：给出判定结果但不删除任何内容
}

// CollectResult 记录一次以登记名字为根的回收判定。
type CollectResult struct {
	Roots   []Digest // 登记名字直接指向的文档
	Live    []Digest // 从根递归可达而保留
	Pinned  []Digest // 不可达但被进行中读取钉住而保留
	Removed []Digest // 本次实际删除（DryRun 时为预演结果，不真正删除）
	DryRun  bool
}

// reachableFromRoots 从根摘要集合出发做可达性遍历：
// index -> 各子 manifest（可嵌套 index），manifest -> config + layers。
//
// 任一登记根或引用边悬空（文档读不出、解析不了或引用 blob 缺失）都返回
// ErrDanglingRef，回收中止，避免把“图损坏”误判成“不可达”而删掉内容。
func (s *Store) reachableFromRoots(roots []Digest) (map[Digest]bool, error) {
	live := map[Digest]bool{}
	var queue []Digest
	queued := map[Digest]bool{}
	for _, r := range roots {
		live[r] = true
		if !queued[r] {
			queued[r] = true
			queue = append(queue, r)
		}
	}
	for len(queue) > 0 {
		d := queue[0]
		queue = queue[1:]
		doc, err := s.ReadDocument(d)
		if err != nil {
			return nil, fmt.Errorf("%w: referenced document %s unreadable: %v",
				ErrDanglingRef, d, err)
		}
		switch v := doc.(type) {
		case *Manifest:
			for _, ref := range v.Refs() {
				if !s.Has(ref) {
					return nil, fmt.Errorf("%w: manifest %s references missing blob %s",
						ErrDanglingRef, d, ref)
				}
				live[ref] = true
			}
		case *Index:
			for _, child := range v.Manifests {
				if !s.Has(child.Digest) {
					return nil, fmt.Errorf("%w: index %s references missing manifest %s",
						ErrDanglingRef, d, child.Digest)
				}
				live[child.Digest] = true
				if !queued[child.Digest] {
					queued[child.Digest] = true
					queue = append(queue, child.Digest)
				}
			}
		default:
			return nil, fmt.Errorf("%w: unexpected parsed type %T for %s",
				ErrUnsupportedMediaType, doc, d)
		}
	}
	return live, nil
}

// CollectRegistered 以仓库本地登记的全部名字为根做可达性回收。
//
// 判定规则（以最新登记为准）：
//
//   - Roots：登记名字直接指向的文档；
//   - Live：从任一 Root 经多架构 index 展开到 manifest、再展开到
//     config/layers 后递归可达的全部内容；
//   - Pinned：不在 Live 中、但被进行中读取钉住，本次保留；
//   - Removed：其余不可达内容。DryRun 时只预演不删除。
//
// 任一登记根悬空（内容缺失、引用损坏）返回 ErrDanglingRef，不删除任何内容。
// 重复执行幂等：第二轮通常 Removed 为空。
func (s *Store) CollectRegistered(ctx context.Context, opts CollectOptions) (CollectResult, error) {
	refs, err := s.allTagsSnapshot()
	if err != nil {
		return CollectResult{}, err
	}
	var roots []Digest
	rootSet := map[Digest]bool{}
	for _, tags := range refs {
		for _, d := range tags {
			if !s.Has(d) {
				return CollectResult{}, fmt.Errorf("%w: registered tag points to missing digest %s",
					ErrDanglingRef, d)
			}
			if !rootSet[d] {
				rootSet[d] = true
				roots = append(roots, d)
			}
		}
	}

	live, err := s.reachableFromRoots(roots)
	if err != nil {
		return CollectResult{}, err
	}

	blobs, err := s.ListBlobs()
	if err != nil {
		return CollectResult{}, err
	}

	rep := CollectResult{DryRun: opts.DryRun, Roots: roots}
	for _, b := range blobs {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		switch {
		case live[b.Digest]:
			rep.Live = append(rep.Live, b.Digest)
		case s.pinned(b.Digest):
			rep.Pinned = append(rep.Pinned, b.Digest)
		default:
			if !opts.DryRun {
				if err := s.remove(b.Digest); err != nil {
					return rep, err
				}
			}
			rep.Removed = append(rep.Removed, b.Digest)
		}
	}
	digests := func(x []Digest) { sort.Slice(x, func(i, j int) bool { return x[i] < x[j] }) }
	digests(rep.Roots)
	digests(rep.Live)
	digests(rep.Pinned)
	digests(rep.Removed)
	return rep, nil
}

// VerifyRefs 校验 manifest 的全部引用在存储中均存在；
// 任一引用悬空时返回 ErrDanglingRef 并指出缺失的摘要。
func VerifyRefs(s *Store, m *Manifest) error {
	for _, d := range m.Refs() {
		if !s.Has(d) {
			return fmt.Errorf("%w: manifest references missing blob %s", ErrDanglingRef, d)
		}
	}
	return nil
}

// Collect 回收不被任何 manifest 引用且未被进行中读取钉住的 blob。
//
// 幂等：重复执行不会删除仍被引用的内容，第二次执行通常 Removed 为空。
// 判定依据记录在 GCReport 中，便于测试与审计。
func (s *Store) Collect(ctx context.Context, manifests []*Manifest) (GCReport, error) {
	live := map[Digest]bool{}
	for _, m := range manifests {
		for _, d := range m.Refs() {
			live[d] = true
		}
	}

	blobs, err := s.ListBlobs()
	if err != nil {
		return GCReport{}, err
	}

	var rep GCReport
	for _, b := range blobs {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		switch {
		case live[b.Digest]:
			rep.Kept = append(rep.Kept, b.Digest)
		case s.pinned(b.Digest):
			rep.Kept = append(rep.Kept, b.Digest) // 进行中的读取受保护
		default:
			if err := s.remove(b.Digest); err != nil {
				return rep, err
			}
			rep.Removed = append(rep.Removed, b.Digest)
		}
	}
	sort.Slice(rep.Kept, func(i, j int) bool { return rep.Kept[i] < rep.Kept[j] })
	sort.Slice(rep.Removed, func(i, j int) bool { return rep.Removed[i] < rep.Removed[j] })
	return rep, nil
}

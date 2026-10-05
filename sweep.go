package ocistore

import (
	"context"
	"fmt"
	"io"
	"sort"
)

// SweepOptions 控制一次以登记引用为根的回收。
type SweepOptions struct {
	// DryRun 为 true 时只判定不删除，Removed 记录“将会被删”的内容。
	DryRun bool
}

// SweepEntry 记录单个 blob 的判定结果与依据。
type SweepEntry struct {
	Digest Digest
	Reason string
}

// SweepReport 记录一轮回收的完整判定依据。
type SweepReport struct {
	DryRun  bool
	Kept    []SweepEntry // 被登记引用递归可达、或被进行中读取钉住
	Removed []SweepEntry // 无人引用（DryRun 时未实际删除）
}

// liveSet 从全部登记引用出发，递归展开 index -> manifest -> config+layers，
// 返回每个存活摘要及首次到达它的引用名（作为保留依据）。
func (s *Store) liveSet(ctx context.Context) (map[Digest]string, error) {
	refs, err := s.ListRefs()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(refs))
	for k := range refs {
		names = append(names, k)
	}
	sort.Strings(names)

	live := map[Digest]string{}
	type item struct {
		d   Digest
		via string
	}
	var queue []item
	for _, name := range names {
		queue = append(queue, item{refs[name], name})
	}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		it := queue[0]
		queue = queue[1:]
		if _, seen := live[it.d]; seen {
			continue
		}
		live[it.d] = it.via

		rc, _, err := s.Get(it.d)
		if err != nil {
			continue // 引用目标已不在库中：无可保留，跳过
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			continue
		}
		doc, err := ParseManifestOrIndex(data)
		if err != nil {
			continue // 普通层 blob：叶子节点
		}
		switch v := doc.(type) {
		case *Manifest:
			for _, d := range v.Refs() {
				queue = append(queue, item{d, it.via})
			}
		case *Index:
			for _, e := range v.Manifests {
				queue = append(queue, item{e.Digest, it.via})
			}
		}
	}
	return live, nil
}

// Sweep 以本地登记的名字为回收起点：凡能被任一登记引用递归
// （index -> manifest -> config/layers）到达的内容都保留；被进行中
// 读取钉住的内容也保留；其余删除。
//
//   - 幂等：重复执行不会误删，第二次 Removed 为空；
//   - DryRun 只判定不删除，便于正式清理前预演；
//   - 判定依据（被谁引用 / 被钉住 / 无人引用）记录在 SweepReport 中。
func (s *Store) Sweep(ctx context.Context, opts SweepOptions) (SweepReport, error) {
	rep := SweepReport{DryRun: opts.DryRun}

	live, err := s.liveSet(ctx)
	if err != nil {
		return rep, err
	}
	blobs, err := s.ListBlobs()
	if err != nil {
		return rep, err
	}

	for _, b := range blobs {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		switch {
		case live[b.Digest] != "":
			rep.Kept = append(rep.Kept, SweepEntry{b.Digest,
				fmt.Sprintf("referenced by %s", live[b.Digest])})
		case s.pinned(b.Digest):
			rep.Kept = append(rep.Kept, SweepEntry{b.Digest,
				"pinned by in-flight read"})
		default:
			entry := SweepEntry{b.Digest, "unreferenced"}
			if !opts.DryRun {
				if err := s.remove(b.Digest); err != nil {
					return rep, err
				}
			}
			rep.Removed = append(rep.Removed, entry)
		}
	}
	sortSweep(rep.Kept)
	sortSweep(rep.Removed)
	return rep, nil
}

func sortSweep(es []SweepEntry) {
	sort.Slice(es, func(i, j int) bool { return es[i].Digest < es[j].Digest })
}

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

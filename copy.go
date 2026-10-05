package ocistore

import (
	"context"
	"fmt"
)

// CopyReport 记录一次跨仓库搬运的结果。
type CopyReport struct {
	Ref    string   // 搬运的引用（name:tag）
	Root   Digest   // 引用指向的根摘要
	Copied []Digest // 本次实际写入目标仓库的内容
	Reused []Digest // 目标仓库已有、去重跳过的内容
}

// CopyImage 把源仓库中已登记的引用 ref 连同其递归引用的全部内容
// （index -> manifest -> config/layers）搬运到目标仓库，并在目标仓库
// 登记同名引用。
//
//   - 流式：逐 blob 边读边写，不把整份内容读进内存；
//   - 去重：目标仓库已有的内容只存一份（Put 的摘要去重）；
//   - 可区分失败：源端内容缺失为 ErrNotFound，源端内容与声明摘要
//     不符为 ErrDigestMismatch；
//   - 可重试：全部内容落地后才登记名字，中途失败不会在目标仓库
//     留下指向残缺内容的名字；重试时已搬运的内容自动去重。
func CopyImage(ctx context.Context, dst, src *Store, ref string) (CopyReport, error) {
	r, err := ParseReference(ref)
	if err != nil {
		return CopyReport{}, err
	}
	root, err := src.ResolveRef(ref)
	if err != nil {
		return CopyReport{}, err
	}
	rep := CopyReport{Ref: r.String(), Root: root}

	// 先收集完整引用闭包（manifest/index 很小，可整体解析；
	// 大层不在这里读，只记录摘要）。
	var order []Digest
	seen := map[Digest]bool{}
	queue := []Digest{root}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		d := queue[0]
		queue = queue[1:]
		if seen[d] {
			continue
		}
		seen[d] = true
		order = append(order, d)

		// 能解析为 manifest/index 就继续展开；普通层 blob 或暂时
		// 缺失的内容都先当叶子，缺失会在搬运阶段以 ErrNotFound 报出。
		doc, err := src.ReadDocument(d)
		if err != nil {
			continue // 普通层 blob：叶子节点
		}
		switch v := doc.(type) {
		case *Manifest:
			queue = append(queue, v.Refs()...)
		case *Index:
			for _, e := range v.Manifests {
				queue = append(queue, e.Digest)
			}
		}
	}

	// 逐个流式搬运。
	for _, d := range order {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		if dst.Has(d) {
			rep.Reused = append(rep.Reused, d)
			continue
		}
		rc, info, err := src.Get(d)
		if err != nil {
			return rep, fmt.Errorf("copy %s: source unreadable: %w", d, err)
		}
		_, putErr := dst.Put(d, info.Size, rc)
		cerr := rc.Close()
		if putErr != nil {
			return rep, fmt.Errorf("copy %s: %w", d, putErr)
		}
		if cerr != nil {
			return rep, fmt.Errorf("copy %s: %w", d, cerr)
		}
		rep.Copied = append(rep.Copied, d)
	}

	// 全部内容落地后才登记名字，保证名字永远指向完整内容。
	if err := dst.Tag(r.String(), root); err != nil {
		return rep, fmt.Errorf("register %q in destination: %w", r, err)
	}
	return rep, nil
}

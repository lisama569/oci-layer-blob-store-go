package ocistore

import (
	"context"
	"fmt"
	"io"
	"sort"
)

// CopyReport 记录一次跨仓库搬运的结果。
type CopyReport struct {
	Tag     TagRecord // 最终在目标仓库登记的名字/标签
	Copied  []Digest  // 实际传输的 blob
	Present []Digest  // 目标已存在、去重跳过的 blob
}

// copyWalk 在源端做存在性预检并返回整个引用闭包。
//
// 只沿“文档边”解析：root（及 index 的孩子）按 manifest/index 解析，
// config/layer 是不透明叶子，绝不尝试解析。
// 缺失返回 ErrNotFound；文档内容损坏到无法解析返回 ErrDigestMismatch；
// 文档声明的引用缺失返回 ErrNotFound。
func copyWalk(src *Store, root Digest) (map[Digest]bool, error) {
	live := map[Digest]bool{root: true}
	queue := []Digest{root}
	queued := map[Digest]bool{root: true}
	for len(queue) > 0 {
		d := queue[0]
		queue = queue[1:]
		if !src.Has(d) {
			return nil, fmt.Errorf("%w: missing blob %s", ErrNotFound, d)
		}
		rc, _, err := src.Get(d)
		if err != nil {
			return nil, err
		}
		data, rerr := io.ReadAll(rc)
		rc.Close()
		if rerr != nil {
			return nil, rerr
		}
		doc, perr := ParseManifestOrIndex(data)
		if perr != nil {
			// 无法解析的文档不可能是其摘要所声明的内容。
			return nil, fmt.Errorf("%w: source document %s unparseable: %v",
				ErrDigestMismatch, d, perr)
		}
		switch v := doc.(type) {
		case *Manifest:
			for _, ref := range v.Refs() {
				if !src.Has(ref) {
					return nil, fmt.Errorf("%w: manifest %s references missing blob %s",
						ErrNotFound, d, ref)
				}
				live[ref] = true
			}
		case *Index:
			for _, child := range v.Manifests {
				if !src.Has(child.Digest) {
					return nil, fmt.Errorf("%w: index %s references missing manifest %s",
						ErrNotFound, d, child.Digest)
				}
				live[child.Digest] = true
				if !queued[child.Digest] {
					queued[child.Digest] = true
					queue = append(queue, child.Digest)
				}
			}
		}
	}
	return live, nil
}

// streamBlob 用固定缓冲把单个 blob 从 src 流式送入 dst.Put。
// 不把整份内容读入内存；三类不一致由 dst.Put 统一判定：
// 源内容不足 -> ErrTruncated，超出 -> ErrSizeMismatch，摘要不符 -> ErrDigestMismatch。
func streamBlob(ctx context.Context, dst, src *Store, d Digest) error {
	rc, info, err := src.Get(d)
	if err != nil {
		return err
	}
	defer rc.Close()

	pr, pw := io.Pipe()
	go func() {
		buf := make([]byte, 32*1024)
		_, err := io.CopyBuffer(pw, rc, buf)
		pw.CloseWithError(err)
	}()

	if _, err := dst.Put(d, info.Size, pr); err != nil {
		pr.CloseWithError(err)
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// CopyFrom 把 src 中已登记的 name:tag 连同其递归引用的全部内容
// （多架构 index -> 各平台 manifest -> config/layers）流式搬运到 s，
// 并在 s 中以相同名字/标签登记。tag 为空时按源端的 "latest" 解析。
//
// 语义：
//
//   - 内容寻址去重：目标已存在的 blob 不重复传输（Copied/Present 分开汇报）；
//   - 失败可区分：源端缺 blob 返回 ErrNotFound；源内容与声明摘要/大小
//     对不上返回 ErrDigestMismatch/ErrTruncated/ErrSizeMismatch；
//   - 不成半成品：所有 blob 完整落盘并通过校验后才在目标登记名字，
//     中途失败目标仍保持自洽；重试接着做、不重复堆内容；
//   - 大文件按 32KiB 缓冲流式传输，不整份读入内存。
func (s *Store) CopyFrom(ctx context.Context, src *Store, name, tag string) (CopyReport, error) {
	if err := validateName(name); err != nil {
		return CopyReport{}, err
	}
	tag = normalizeTag(tag)
	if err := validateTag(tag); err != nil {
		return CopyReport{}, err
	}

	root, err := src.Lookup(name, tag)
	if err != nil {
		return CopyReport{}, err
	}

	reachable, err := copyWalk(src, root)
	if err != nil {
		return CopyReport{}, err
	}

	var all []Digest
	for d := range reachable {
		all = append(all, d)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })

	rep := CopyReport{Tag: TagRecord{Name: name, Tag: tag, Digest: root}}
	for _, d := range all {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		if s.Has(d) {
			rep.Present = append(rep.Present, d)
			continue
		}
		// 预检后源文件若被移走，Get 返回 ErrNotFound；
		// 损坏内容在流式摘要校验中暴露为 ErrDigestMismatch 等。
		if err := streamBlob(ctx, s, src, d); err != nil {
			return rep, err
		}
		rep.Copied = append(rep.Copied, d)
	}
	sort.Slice(rep.Present, func(i, j int) bool { return rep.Present[i] < rep.Present[j] })
	sort.Slice(rep.Copied, func(i, j int) bool { return rep.Copied[i] < rep.Copied[j] })

	// 全部内容到位后才登记名字，目标仓库不会出现指向残缺内容的标签。
	if err := s.Tag(name, tag, root); err != nil {
		return rep, err
	}
	return rep, nil
}

package ocistore

import (
	"bytes"
	"fmt"
	"io"
)

// PutBytes 按内容自算摘要与大小写入。
func (s *Store) PutBytes(b []byte) (BlobInfo, error) {
	d := DigestOf(b)
	return s.Put(d, int64(len(b)), bytes.NewReader(b))
}

// GetBytes 读取整个 blob。
func (s *Store) GetBytes(d Digest) ([]byte, error) {
	rc, _, err := s.Get(d)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// ReadDocument 读取 blob 并按 manifest/index 解析。
// 返回 *Manifest 或 *Index。
func (s *Store) ReadDocument(d Digest) (any, error) {
	data, err := s.GetBytes(d)
	if err != nil {
		return nil, err
	}
	return ParseManifestOrIndex(data)
}

// Resolve 从文档摘要出发解析到目标平台的具体 manifest。
//
//   - 若 d 直接指向 manifest，则原样返回；
//   - 若 d 指向 index，则按平台 p 挑选，再读取并解析子 manifest；
//
// 匹配不到平台返回 ErrPlatformNotFound；子引用悬空返回 ErrDanglingRef。
func (s *Store) Resolve(d Digest, p Platform) (*Manifest, error) {
	doc, err := s.ReadDocument(d)
	if err != nil {
		return nil, err
	}
	switch v := doc.(type) {
	case *Manifest:
		if err := VerifyRefs(s, v); err != nil {
			return nil, err
		}
		return v, nil
	case *Index:
		child, err := SelectPlatform(v, p)
		if err != nil {
			return nil, err
		}
		sub, err := s.ReadDocument(child.Digest)
		if err != nil {
			return nil, fmt.Errorf("%w: index entry %s unreadable: %v",
				ErrDanglingRef, child.Digest, err)
		}
		m, ok := sub.(*Manifest)
		if !ok {
			return nil, fmt.Errorf("%w: index entry %s is not a manifest",
				ErrUnsupportedMediaType, child.Digest)
		}
		if err := VerifyRefs(s, m); err != nil {
			return nil, err
		}
		return m, nil
	default:
		return nil, fmt.Errorf("%w: unexpected parsed type %T", ErrUnsupportedMediaType, doc)
	}
}

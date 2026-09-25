package ocistore

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Store 是本地 OCI 内容存储的入口，聚合 blob 存储、命名引用与回收。
type Store struct {
	root  string
	blobs *BlobStore

	// 并发与回收的协调模型（详见 docs/design 与 gc.go 注释）：
	//
	//   - 所有 blob 读/写与 refs 变更均持有 gcMu 的读锁；
	//   - GC 持有写锁完成“标记 + 清除”全过程。
	//
	// 因此一次 GC 开始前，所有在途读取（读锁随 ReadCloser.Close 释放）与
	// 在途写入必然已经结束，不会删掉正在被读的内容；GC 进行中新到达的
	// 写入与打 tag 会在 GC 结束后落盘，其内容不会被本轮误删，下一轮
	// GC 再据最新引用关系判定。本地操作耗时很短，串行边界清晰可解释。
	gcMu   sync.RWMutex
	refsMu sync.Mutex
}

// Open 打开（必要时初始化）root 下的本地存储。
//
// 目录布局：
//
//	root/
//	├── blobs/<algorithm>/<hex>   全部按摘要寻址的内容（层/config/manifest）
//	├── tmp/                      写入过程中的临时文件，提交后即消失
//	└── refs/<url-path-escaped tag>  命名引用文件，内容为 JSON Descriptor
func Open(root string) (*Store, error) {
	if root == "" {
		return nil, fmt.Errorf("oci-store: empty root")
	}
	for _, sub := range []string{"blobs", filepath.Join("blobs", "sha256"), "tmp", "refs"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			return nil, fmt.Errorf("init layout %s: %w", sub, err)
		}
	}
	return &Store{root: root, blobs: newBlobStore(root)}, nil
}

// Root 返回存储根目录。
func (s *Store) Root() string { return s.root }

// PutBlob 将 blob 写入存储，语义见 BlobStore.PutBlob。
func (s *Store) PutBlob(ctx context.Context, r io.Reader, expected Digest, declaredSize int64) (Descriptor, error) {
	s.gcMu.RLock()
	defer s.gcMu.RUnlock()
	return s.blobs.PutBlob(ctx, r, expected, declaredSize)
}

// lockedReadCloser 关闭时释放 GC 读锁，保证 GC 不会在读取进行中删除内容。
type lockedReadCloser struct {
	io.ReadCloser
	release func()
	closed  bool
}

func (l *lockedReadCloser) Close() error {
	if l.closed {
		return nil
	}
	l.closed = true
	err := l.ReadCloser.Close()
	l.release()
	return err
}

// GetBlob 按摘要打开 blob；返回的 ReadCloser 必须关闭，
// 关闭前 GC 会等待该读取结束。
func (s *Store) GetBlob(ctx context.Context, d Digest) (io.ReadCloser, Descriptor, error) {
	s.gcMu.RLock()
	rc, desc, err := s.blobs.GetBlob(ctx, d)
	if err != nil {
		s.gcMu.RUnlock()
		return nil, Descriptor{}, err
	}
	return &lockedReadCloser{ReadCloser: rc, release: s.gcMu.RUnlock}, desc, nil
}

// StatBlob 返回 blob 描述符。
func (s *Store) StatBlob(ctx context.Context, d Digest) (Descriptor, error) {
	s.gcMu.RLock()
	defer s.gcMu.RUnlock()
	return s.blobs.StatBlob(ctx, d)
}

// HasBlob 判断 blob 是否存在。
func (s *Store) HasBlob(ctx context.Context, d Digest) (bool, error) {
	s.gcMu.RLock()
	defer s.gcMu.RUnlock()
	return s.blobs.HasBlob(ctx, d)
}

// Close 释放存储资源。当前布局下无后台资源，保留以兼容未来扩展。
func (s *Store) Close() error { return nil }

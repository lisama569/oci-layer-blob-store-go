package ocistore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// BlobInfo 描述一个已存储 blob 的元信息。
type BlobInfo struct {
	Digest Digest
	Size   int64
}

// Store 是基于本地文件系统的内容寻址存储。
//
// 布局：
//
//	<root>/blobs/sha256/<hex>   内容寻址的 blob
//	<root>/tmp/                 写入中的临时文件（校验通过后原子 rename）
//
// 并发语义：同一摘要的并发写入通过“临时文件 + 原子 rename”保证
// 不会出现部分可见；Get 会在进程内钉住（pin）摘要，Close 时释放，
// 回收器不得删除仍被钉住的内容。
type Store struct {
	root string

	mu   sync.Mutex
	pins map[Digest]int
}

// Open 打开（必要时创建）位于 root 的存储。
func Open(root string) (*Store, error) {
	for _, dir := range []string{
		filepath.Join(root, "blobs", "sha256"),
		filepath.Join(root, "tmp"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	return &Store{root: root, pins: map[Digest]int{}}, nil
}

// Root 返回存储根目录。
func (s *Store) Root() string { return s.root }

func (s *Store) blobPath(d Digest) string {
	return filepath.Join(s.root, "blobs", d.Algorithm(), d.Hex())
}

// Put 按声明的摘要与大小写入内容。
//
// 失败原因可区分：ErrTruncated（内容不足声明大小）、
// ErrSizeMismatch（内容超出声明大小）、ErrDigestMismatch（摘要不符）。
// 任何失败都不会在 blobs 目录留下半成品。
func (s *Store) Put(expected Digest, size int64, r io.Reader) (BlobInfo, error) {
	if err := expected.Validate(); err != nil {
		return BlobInfo{}, err
	}
	if size < 0 {
		return BlobInfo{}, fmt.Errorf("%w: negative declared size %d", ErrSizeMismatch, size)
	}

	// 已存在则直接命中（内容寻址，相同摘要即相同内容）。
	if info, ok := s.stat(expected); ok {
		return info, nil
	}

	tmp, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "put-*")
	if err != nil {
		return BlobInfo{}, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // 任何失败路径都会清理临时文件

	h := sha256.New()
	n, err := copyCounting(tmp, h, r)
	if err != nil {
		tmp.Close()
		return BlobInfo{}, err
	}
	if cerr := tmp.Close(); cerr != nil {
		return BlobInfo{}, cerr
	}

	// 大小校验：不足为截断，超出为大小不符。
	switch {
	case n < size:
		return BlobInfo{}, fmt.Errorf("%w: declared %d bytes, got %d", ErrTruncated, size, n)
	case n > size:
		return BlobInfo{}, fmt.Errorf("%w: declared %d bytes, got %d", ErrSizeMismatch, size, n)
	}

	// 摘要校验。
	sum := h.Sum(nil)
	actual := Digest("sha256:" + hex.EncodeToString(sum))
	if actual != expected {
		return BlobInfo{}, fmt.Errorf("%w: declared %s, computed %s", ErrDigestMismatch, expected, actual)
	}

	dst := s.blobPath(expected)
	if err := os.Rename(tmpName, dst); err != nil {
		// 并发写入者可能已落盘同一摘要；此时视为命中。
		if info, ok := s.stat(expected); ok {
			return info, nil
		}
		return BlobInfo{}, err
	}
	return BlobInfo{Digest: expected, Size: size}, nil
}

// copyCounting 将 r 同时写入 w 与 h，返回字节数。
func copyCounting(w io.Writer, h hash.Hash, r io.Reader) (int64, error) {
	return io.Copy(io.MultiWriter(w, h), r)
}

func (s *Store) stat(d Digest) (BlobInfo, bool) {
	fi, err := os.Stat(s.blobPath(d))
	if err != nil {
		return BlobInfo{}, false
	}
	return BlobInfo{Digest: d, Size: fi.Size()}, true
}

// Get 按摘要读取内容。返回的 ReadCloser 关闭前，该摘要被钉住，
// 回收器不会删除它。
func (s *Store) Get(d Digest) (io.ReadCloser, BlobInfo, error) {
	if err := d.Validate(); err != nil {
		return nil, BlobInfo{}, err
	}
	info, ok := s.stat(d)
	if !ok {
		return nil, BlobInfo{}, fmt.Errorf("%w: %s", ErrNotFound, d)
	}
	f, err := os.Open(s.blobPath(d))
	if err != nil {
		return nil, BlobInfo{}, fmt.Errorf("%w: %s (%v)", ErrNotFound, d, err)
	}
	s.pin(d)
	return &pinnedFile{File: f, done: func() { s.unpin(d) }}, info, nil
}

// Has 判断摘要对应内容是否存在。
func (s *Store) Has(d Digest) bool {
	_, ok := s.stat(d)
	return ok
}

// ListBlobs 列出当前全部 blob。
func (s *Store) ListBlobs() ([]BlobInfo, error) {
	dir := filepath.Join(s.root, "blobs", "sha256")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []BlobInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		d := Digest("sha256:" + e.Name())
		if err := d.Validate(); err != nil {
			continue // 忽略非法文件名
		}
		if info, ok := s.stat(d); ok {
			out = append(out, info)
		}
	}
	return out, nil
}

func (s *Store) pin(d Digest) {
	s.mu.Lock()
	s.pins[d]++
	s.mu.Unlock()
}

func (s *Store) unpin(d Digest) {
	s.mu.Lock()
	if s.pins[d] <= 1 {
		delete(s.pins, d)
	} else {
		s.pins[d]--
	}
	s.mu.Unlock()
}

// pinned 报告摘要当前是否被进行中的读取钉住。
func (s *Store) pinned(d Digest) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pins[d] > 0
}

// remove 删除 blob（仅供回收器调用）；被钉住时拒绝。
func (s *Store) remove(d Digest) error {
	if s.pinned(d) {
		return fmt.Errorf("ocistore: %s is pinned by an in-flight read", d)
	}
	err := os.Remove(s.blobPath(d))
	if os.IsNotExist(err) {
		return nil // 幂等
	}
	return err
}

type pinnedFile struct {
	*os.File
	done func()
}

func (p *pinnedFile) Close() error {
	err := p.File.Close()
	p.done()
	return err
}

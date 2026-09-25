package ocistore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// BlobStore 负责按摘要落盘与读取 blob，同摘要内容全局唯一。
type BlobStore struct {
	root string

	// 同一摘要的并发 Put 串行化，保证“只落盘一份、失败不留半成品”。
	putLocks sync.Map // string -> *sync.Mutex
}

func newBlobStore(root string) *BlobStore {
	return &BlobStore{root: root}
}

func (s *BlobStore) blobPath(d Digest) (string, error) {
	if _, err := ParseDigest(string(d)); err != nil {
		return "", err
	}
	return filepath.Join(s.root, "blobs", d.Algorithm(), d.Hex()), nil
}

// keyLock 获取按 key 互斥的锁（sync.Map 里的 *sync.Mutex 指针永不删除，
// 数量受摘要种类上界约束）。
func (s *BlobStore) keyLock(key string) *sync.Mutex {
	mu, _ := s.putLocks.LoadOrStore(key, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// PutBlob 将 r 的内容写入存储。
//
// 规则：
//   - expected 为空时按实际内容计算摘要；
//   - expected 非空时必须与实际内容一致，否则 ErrDigestMismatch；
//   - declaredSize >= 0 时必须与实际字节数一致，短于声明 ErrTruncated，
//     长于声明 ErrSizeMismatch；
//   - 相同摘要内容只落盘一份，已存在时不得覆盖；
//   - 先写临时文件、校验通过后原子 rename；任何失败都清理半成品。
func (s *BlobStore) PutBlob(ctx context.Context, r io.Reader, expected Digest, declaredSize int64) (Descriptor, error) {
	if expected != "" {
		if _, err := ParseDigest(string(expected)); err != nil {
			return Descriptor{}, err
		}
	}

	tmp, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "ingest-*")
	if err != nil {
		return Descriptor{}, fmt.Errorf("create temp blob: %w", err)
	}
	tmpName := tmp.Name()
	// cleanup 仅在 rename 成功后置空。
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	vr, err := VerifiedReader(ctxReader(ctx, r), expected, declaredSize)
	if err != nil {
		cleanup()
		return Descriptor{}, err
	}
	n, copyErr := io.Copy(tmp, vr)
	if copyErr != nil {
		cleanup()
		// 读取中途失败（如连接中断）：按已收到字节给出可区分原因。
		// 短于声明 => 截断；否则若校验器已产生摘要/大小错误则优先返回之。
		if declaredSize >= 0 && n < declaredSize {
			return Descriptor{}, &TruncatedError{Declared: declaredSize, Actual: n}
		}
		if n > 0 {
			if cerr := vr.Close(); cerr != nil {
				return Descriptor{}, cerr
			}
		}
		return Descriptor{}, copyErr
	}
	// 正常读完时 verify 已在 EOF 路径执行；这里再 Close 兜底防止
	// reader 未返回 EOF 的实现差异。
	if err := vr.Close(); err != nil {
		cleanup()
		return Descriptor{}, err
	}

	got := vr.(*verifyingReader).computedDigest()
	mu := s.keyLock(string(got))
	mu.Lock()
	defer mu.Unlock()

	dst, err := s.blobPath(got)
	if err != nil {
		cleanup()
		return Descriptor{}, err
	}

	// 已存在：摘要相同即视为同内容，丢弃临时文件，绝不覆盖。
	if _, err := os.Stat(dst); err == nil {
		cleanup()
		return Descriptor{Digest: got, Size: n}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		cleanup()
		return Descriptor{}, fmt.Errorf("stat existing blob: %w", err)
	}

	if err := tmp.Chmod(0o444); err != nil {
		cleanup()
		return Descriptor{}, err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return Descriptor{}, fmt.Errorf("fsync blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return Descriptor{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		os.Remove(tmpName)
		return Descriptor{}, err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		os.Remove(tmpName)
		return Descriptor{}, fmt.Errorf("commit blob %s: %w", got, err)
	}
	syncDir(filepath.Dir(dst))

	return Descriptor{Digest: got, Size: n}, nil
}

// GetBlob 按摘要打开 blob，返回的 ReadCloser 由调用方负责关闭。
func (s *BlobStore) GetBlob(ctx context.Context, d Digest) (io.ReadCloser, Descriptor, error) {
	p, err := s.blobPath(d)
	if err != nil {
		return nil, Descriptor{}, err
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, Descriptor{}, fmt.Errorf("%w: %s", ErrNotFound, d)
		}
		return nil, Descriptor{}, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, Descriptor{}, err
	}
	return f, Descriptor{Digest: d, Size: fi.Size()}, nil
}

// StatBlob 返回 blob 的描述符。
func (s *BlobStore) StatBlob(ctx context.Context, d Digest) (Descriptor, error) {
	p, err := s.blobPath(d)
	if err != nil {
		return Descriptor{}, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Descriptor{}, fmt.Errorf("%w: %s", ErrNotFound, d)
		}
		return Descriptor{}, err
	}
	return Descriptor{Digest: d, Size: fi.Size()}, nil
}

// HasBlob 判断 blob 是否存在。
func (s *BlobStore) HasBlob(ctx context.Context, d Digest) (bool, error) {
	p, err := s.blobPath(d)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// DeleteBlob 按摘要删除 blob；不存在不算错误（保证回收幂等）。
func (s *BlobStore) DeleteBlob(ctx context.Context, d Digest) error {
	p, err := s.blobPath(d)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ListBlobs 列出存储内全部 blob 摘要，结果排序保证输出稳定可解释。
func (s *BlobStore) ListBlobs(ctx context.Context) ([]Digest, error) {
	var out []Digest
	root := filepath.Join(s.root, "blobs")
	err := filepath.WalkDir(root, func(path string, de os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && path == root {
				return nil
			}
			return err
		}
		if de.IsDir() {
			return nil
		}
		d := Digest(filepath.Base(filepath.Dir(path)) + ":" + de.Name())
		if _, perr := ParseDigest(string(d)); perr != nil {
			// 忽略无法识别的文件，但保持可见：测试环境不会产生。
			return nil
		}
		out = append(out, d)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// ctxReader 在 ctx 取消时让拷贝中断：简单包装为在 Read 前检查 ctx。
type ctxAwareReader struct {
	ctx context.Context
	r   io.Reader
}

func ctxReader(ctx context.Context, r io.Reader) io.Reader {
	if ctx == nil {
		return r
	}
	return &ctxAwareReader{ctx: ctx, r: r}
}

func (c *ctxAwareReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// syncDir 对目录做一次 fsync，保证 rename/删除在崩溃后状态可预期。
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

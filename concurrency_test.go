package ocistore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
)

func TestConcurrentPutSameContent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	data := make([]byte, 256*1024)
	for i := range data {
		data[i] = byte(i * 7)
	}
	want := digestOf(data)

	const writers = 32
	var wg sync.WaitGroup
	errCh := make(chan error, writers)
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func() {
			defer wg.Done()
			desc, err := s.PutBlob(ctx, bytes.NewReader(append([]byte(nil), data...)), want, int64(len(data)))
			if err != nil {
				errCh <- err
				return
			}
			if desc.Digest != want {
				errCh <- fmt.Errorf("unexpected digest %s", desc.Digest)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent put: %v", err)
	}

	all, _ := s.blobs.ListBlobs(ctx)
	if len(all) != 1 || all[0] != want {
		t.Fatalf("concurrent writes must land exactly one blob, got %v", all)
	}
	t.Logf("并发%d次写入同一摘要 %s => 落盘仅一份 %v", writers, want, all)

	got := blobContent(t, s, want)
	if !bytes.Equal(got, data) {
		t.Fatalf("blob corrupted under concurrent writes")
	}
}

func TestConcurrentReadWhileWriting(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	blobData := make([][]byte, 8)
	digests := make([]Digest, 8)
	for i := range blobData {
		blobData[i] = bytes.Repeat([]byte{byte('A' + i)}, 1024)
		digests[i] = digestOf(blobData[i])
		putLayer(t, s, blobData[i])
	}

	const rounds = 200
	stopReads := make(chan struct{})
	var readers, writers sync.WaitGroup

	// 读侧：持续读取全部 blob 并逐字节校验，读到损坏即失败。
	for r := 0; r < 8; r++ {
		readers.Add(1)
		go func(id int) {
			defer readers.Done()
			for {
				select {
				case <-stopReads:
					return
				default:
				}
				idx := id % len(digests)
				rc, _, err := s.GetBlob(ctx, digests[idx])
				if err != nil {
					t.Errorf("reader %d get: %v", id, err)
					return
				}
				b, err := io.ReadAll(rc)
				_ = rc.Close()
				if err != nil {
					t.Errorf("reader %d read: %v", id, err)
					return
				}
				if !bytes.Equal(b, blobData[idx]) {
					t.Errorf("reader %d saw corrupted content for %s", id, digests[idx])
					return
				}
			}
		}(r)
	}

	// 写侧：反复写入（含相同摘要去重与新内容）。
	writers.Add(1)
	go func() {
		defer writers.Done()
		for i := 0; i < rounds; i++ {
			idx := i % len(blobData)
			if _, err := s.PutBlob(ctx, bytes.NewReader(append([]byte(nil), blobData[idx]...)), digests[idx], int64(len(blobData[idx]))); err != nil {
				t.Errorf("writer: %v", err)
				return
			}
		}
	}()
	writers.Wait()
	close(stopReads)
	readers.Wait()

	t.Log("并发读写期间未观测到部分可见或损坏数据（建议以 -race 复跑）")
}

func TestConcurrentBadWritesLeaveNothing(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	good := bytes.Repeat([]byte("g"), 4096)
	badDigest := digestOf([]byte("other"))

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.PutBlob(ctx, bytes.NewReader(append([]byte(nil), good...)), badDigest, int64(len(good)))
			if !errors.Is(err, ErrDigestMismatch) {
				t.Errorf("want digest mismatch, got %v", err)
			}
		}()
	}
	wg.Wait()
	all, _ := s.blobs.ListBlobs(ctx)
	if len(all) != 0 {
		t.Fatalf("failed concurrent writes left blobs: %v", all)
	}
	t.Logf("16 个并发非法写入全部拒绝，存储内 blob 数=%d（无半成品）", len(all))
}

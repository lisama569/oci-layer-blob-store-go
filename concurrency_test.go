package ocistore

import (
	"bytes"
	"io"
	"sync"
	"testing"
)

func TestConcurrentPutSameContent(t *testing.T) {
	s := newTestStore(t)
	content := bytes.Repeat([]byte("concurrent-layer-"), 4096)
	d := DigestOf(content)
	t.Logf("concurrent write digest=%s size=%d writers=%d", d, len(content), 32)

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Put(d, int64(len(content)), bytes.NewReader(content))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent put failed: %v", err)
		}
	}
	blobs, _ := s.ListBlobs()
	if len(blobs) != 1 {
		t.Fatalf("want 1 blob after concurrent writes, got %d", len(blobs))
	}

	// 数据必须完整可读
	rc, _, err := s.Get(d)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, content) {
		t.Fatalf("content corrupted after concurrent writes")
	}
	t.Logf("concurrent writes converged to single intact blob %s", d)
}

func TestConcurrentReadWhileGCNeverCorrupts(t *testing.T) {
	s := newTestStore(t)
	// mA 引用 layer1；layer2 无任何引用，应被回收
	cfg1, _ := s.PutBytes([]byte("{\"config\":\"a\"}"))
	layer1, _ := s.PutBytes([]byte("layer-one"))
	layer2, _ := s.PutBytes([]byte("layer-two-orphan"))
	mA := &Manifest{
		Config: Descriptor{Digest: cfg1.Digest},
		Layers: []Descriptor{{Digest: layer1.Digest}},
	}
	t.Logf("refs: manifest->{%s,%s}; orphan=%s", cfg1.Digest, layer1.Digest, layer2.Digest)

	const readers = 16
	var wg sync.WaitGroup
	panicCh := make(chan error, readers)

	// 启动并发读取 layer1，读取期间触发回收
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rc, _, err := s.Get(layer1.Digest)
			if err != nil {
				panicCh <- err
				return
			}
			defer rc.Close()
			got, err := io.ReadAll(rc)
			if err != nil {
				panicCh <- err
				return
			}
			if string(got) != "layer-one" {
				panicCh <- io.ErrUnexpectedEOF
			}
		}()
	}
	rep, err := s.Collect(t.Context(), []*Manifest{mA})
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	wg.Wait()
	close(panicCh)
	for err := range panicCh {
		t.Fatalf("reader failed during gc: %v", err)
	}

	t.Logf("gc kept=%v removed=%v", rep.Kept, rep.Removed)
	if s.Has(layer2.Digest) {
		t.Fatalf("orphan blob %s should have been removed", layer2.Digest)
	}
	if !s.Has(layer1.Digest) || !s.Has(cfg1.Digest) {
		t.Fatalf("referenced blobs must survive gc")
	}
}

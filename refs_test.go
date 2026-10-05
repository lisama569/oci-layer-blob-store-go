package ocistore

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestTagResolveRepointUntag(t *testing.T) {
	s := newTestStore(t)

	v1, _ := s.PutBytes([]byte("image-v1"))
	v2, _ := s.PutBytes([]byte("image-v2-longer"))

	// 登记
	if err := s.Tag("app/web:latest", v1.Digest); err != nil {
		t.Fatalf("tag: %v", err)
	}
	d, err := s.ResolveRef("app/web:latest")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if d != v1.Digest {
		t.Fatalf("resolve = %s, want %s", d, v1.Digest)
	}

	// 省略标签默认为 latest
	d2, err := s.ResolveRef("app/web")
	if err != nil || d2 != v1.Digest {
		t.Fatalf("resolve without tag = %s, %v; want %s", d2, err, v1.Digest)
	}

	// 改指向，以最新登记为准
	if err := s.Tag("app/web:latest", v2.Digest); err != nil {
		t.Fatalf("retag: %v", err)
	}
	if d, _ := s.ResolveRef("app/web:latest"); d != v2.Digest {
		t.Fatalf("after repoint resolve = %s, want %s", d, v2.Digest)
	}

	// 列出
	names, err := s.RefNames()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(names) != 1 || names[0] != "app/web:latest" {
		t.Fatalf("names = %v", names)
	}

	// 删除
	if err := s.Untag("app/web:latest"); err != nil {
		t.Fatalf("untag: %v", err)
	}
	if _, err := s.ResolveRef("app/web:latest"); !errors.Is(err, ErrRefNotFound) {
		t.Fatalf("resolve after untag: want ErrRefNotFound, got %v", err)
	}
	if err := s.Untag("app/web:latest"); !errors.Is(err, ErrRefNotFound) {
		t.Fatalf("double untag: want ErrRefNotFound, got %v", err)
	}
}

func TestTagValidation(t *testing.T) {
	s := newTestStore(t)
	blob, _ := s.PutBytes([]byte("content"))

	cases := []struct {
		name string
		ref  string
		d    Digest
		want error
	}{
		{"bad name uppercase", "App/Web:latest", blob.Digest, ErrInvalidName},
		{"bad name chars", "app$/web:latest", blob.Digest, ErrInvalidName},
		{"empty name", ":latest", blob.Digest, ErrInvalidName},
		{"bad tag", "app/web:-x", blob.Digest, ErrInvalidTag},
		{"bad digest", "app/web:latest", "not-a-digest", ErrInvalidDigest},
		{"missing blob", "app/web:latest", DigestOf([]byte("absent")), ErrNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := s.Tag(c.ref, c.d)
			if !errors.Is(err, c.want) {
				t.Fatalf("Tag(%q, %s) = %v, want %v", c.ref, c.d, err, c.want)
			}
		})
	}
	// 非法登记一个都不应留下
	if names, _ := s.RefNames(); len(names) != 0 {
		t.Fatalf("no ref should be recorded, got %v", names)
	}
}

func TestRefsPersistAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	blob, _ := s1.PutBytes([]byte("persistent-image"))
	if err := s1.Tag("app/db:v1.2", blob.Digest); err != nil {
		t.Fatal(err)
	}

	// 模拟换进程：重新 Open 同一目录
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s2.ResolveRef("app/db:v1.2")
	if err != nil {
		t.Fatalf("resolve after reopen: %v", err)
	}
	if d != blob.Digest {
		t.Fatalf("resolve after reopen = %s, want %s", d, blob.Digest)
	}
}

func TestConcurrentTagSameName(t *testing.T) {
	s := newTestStore(t)
	const n = 16
	digests := make([]Digest, n)
	for i := range digests {
		info, _ := s.PutBytes([]byte(fmt.Sprintf("candidate-%d", i)))
		digests[i] = info.Digest
	}

	// 并发登记/改指向同一名字：不得出现读到半截或丢数据，
	// 最终必须收敛到某一个完整结果。
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := s.Tag("app/hot:latest", digests[i]); err != nil {
				t.Errorf("tag: %v", err)
			}
		}(i)
	}
	wg.Wait()

	final, err := s.ResolveRef("app/hot:latest")
	if err != nil {
		t.Fatalf("final resolve: %v", err)
	}
	found := false
	for _, d := range digests {
		if d == final {
			found = true
		}
	}
	if !found {
		t.Fatalf("final digest %s is not any registered candidate", final)
	}

	// 重新打开后读到的必须与最后结果一致（落盘完整）
	s2, err := Open(s.Root())
	if err != nil {
		t.Fatal(err)
	}
	d2, err := s2.ResolveRef("app/hot:latest")
	if err != nil || d2 != final {
		t.Fatalf("after reopen = %s, %v; want %s", d2, err, final)
	}
}

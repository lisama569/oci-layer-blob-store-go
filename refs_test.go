package ocistore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestTagLookupRetagUntagList(t *testing.T) {
	s := newTestStore(t)
	cfg, _ := s.PutBytes([]byte("cfg-v1"))
	m1, _ := s.PutBytes([]byte("manifest-v1"))
	m2, _ := s.PutBytes([]byte("manifest-v2"))

	// 登记（tag 省略 -> latest）
	if err := s.Tag("registry.example.com/app", "", m1.Digest); err != nil {
		t.Fatalf("tag: %v", err)
	}
	got, err := s.Lookup("registry.example.com/app", "")
	if err != nil {
		t.Fatalf("lookup latest: %v", err)
	}
	if got != m1.Digest {
		t.Fatalf("lookup = %s, want %s", got, m1.Digest)
	}
	t.Logf("tagged registry.example.com/app:latest -> %s", m1.Digest)

	// 额外打一个显式标签
	if err := s.Tag("registry.example.com/app", "v1", m1.Digest); err != nil {
		t.Fatalf("tag v1: %v", err)
	}

	// 改指向
	if err := s.Tag("registry.example.com/app", "latest", m2.Digest); err != nil {
		t.Fatalf("retag: %v", err)
	}
	got, _ = s.Lookup("registry.example.com/app", "latest")
	if got != m2.Digest {
		t.Fatalf("after retag latest = %s, want %s", got, m2.Digest)
	}
	t.Logf("retagged latest -> %s; v1 still -> %s", m2.Digest, m1.Digest)
	got, _ = s.Lookup("registry.example.com/app", "v1")
	if got != m1.Digest {
		t.Fatalf("v1 moved unexpectedly: %s", got)
	}

	// 列表
	list, err := s.ListTags()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 records, got %d: %+v", len(list), list)
	}
	for _, r := range list {
		t.Logf("registered: %s:%s -> %s", r.Name, r.Tag, r.Digest)
	}

	// 删除
	if err := s.Untag("registry.example.com/app", "v1"); err != nil {
		t.Fatalf("untag: %v", err)
	}
	if _, err := s.Lookup("registry.example.com/app", "v1"); !errors.Is(err, ErrNameNotFound) {
		t.Fatalf("want ErrNameNotFound after untag, got %v", err)
	}
	if err := s.Untag("registry.example.com/app", "latest"); err != nil {
		t.Fatalf("untag latest: %v", err)
	}
	list, _ = s.ListTags()
	if len(list) != 0 {
		t.Fatalf("want empty registry, got %+v", list)
	}
	// 再删一次应报错
	if err := s.Untag("registry.example.com/app", "latest"); !errors.Is(err, ErrNameNotFound) {
		t.Fatalf("double untag want ErrNameNotFound, got %v", err)
	}
	_ = cfg
}

func TestTagValidation(t *testing.T) {
	s := newTestStore(t)
	m, _ := s.PutBytes([]byte("m"))

	for _, bad := range []string{"", "UPPER/app", "app//x", "-app", "app:5000/x", "a/b@c"} {
		err := s.Tag(bad, "latest", m.Digest)
		if !errors.Is(err, ErrInvalidName) {
			t.Fatalf("name %q want ErrInvalidName, got %v", bad, err)
		}
		t.Logf("invalid name %q rejected", bad)
	}
	for _, bad := range []string{"-tag", "tag with space", "中文", "a:b"} {
		err := s.Tag("app", bad, m.Digest)
		if !errors.Is(err, ErrInvalidTag) {
			t.Fatalf("tag %q want ErrInvalidTag, got %v", bad, err)
		}
		t.Logf("invalid tag %q rejected", bad)
	}

	// 非法摘要
	badDigest := Digest("sha256:zz")
	if err := s.Tag("app", "latest", badDigest); !errors.Is(err, ErrInvalidDigest) {
		t.Fatalf("want ErrInvalidDigest, got %v", err)
	}
	t.Logf("invalid digest %q rejected", badDigest)

	// 合法格式但内容本地不存在
	ghost := DigestOf([]byte("ghost"))
	if err := s.Tag("app", "latest", ghost); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound for missing content, got %v", err)
	}
	t.Logf("tag to missing digest %s rejected", ghost)
}

func TestTagsPersistAcrossReopen(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := s.PutBytes([]byte("persisted-manifest"))
	if err := s.Tag("team/image", "1.0", m.Digest); err != nil {
		t.Fatal(err)
	}

	// 重新打开（同一进程新句柄）
	s2, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s2.Lookup("team/image", "1.0")
	if err != nil {
		t.Fatalf("lookup after reopen: %v", err)
	}
	if got != m.Digest {
		t.Fatalf("persisted digest mismatch: %s vs %s", got, m.Digest)
	}
	t.Logf("after reopen team/image:1.0 -> %s", got)

	// refs 文件确实落盘
	if _, err := os.Stat(filepath.Join(root, "refs", "tags.json")); err != nil {
		t.Fatalf("tags.json missing: %v", err)
	}
}

// 用独立的子进程测试跨进程持久化，排除纯内存巧合。
func TestTagsPersistAcrossProcess(t *testing.T) {
	if os.Getenv("OCI_TAG_HELPER") == "1" {
		// helper 模式：登记一个标签后退出
		root := os.Getenv("OCI_TAG_ROOT")
		s, err := Open(root)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		info, err := s.PutBytes([]byte("cross-process-manifest"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := s.Tag("cross/proc", "v9", info.Digest); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	root := t.TempDir()
	helper := func(action string) {
		cmd := newTestHelperCmd(t, root, action)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("helper %s failed: %v\n%s", action, err, out)
		}
	}
	helper("write")

	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Lookup("cross/proc", "v9")
	if err != nil {
		t.Fatalf("cross-process lookup: %v", err)
	}
	if want := DigestOf([]byte("cross-process-manifest")); d != want {
		t.Fatalf("cross-process digest = %s, want %s", d, want)
	}
	t.Logf("cross-process tag visible: cross/proc:v9 -> %s", d)
}

func TestConcurrentTagNoLostWrites(t *testing.T) {
	s := newTestStore(t)
	// 多个目标摘要，每个 goroutine 把同一标签改成不同摘要
	const n = 24
	var digests []Digest
	for i := 0; i < n; i++ {
		info, err := s.PutBytes([]byte(fmt.Sprintf("manifest-%02d", i)))
		if err != nil {
			t.Fatal(err)
		}
		digests = append(digests, info.Digest)
	}

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := s.Tag("race/app", fmt.Sprintf("t%02d", i), digests[i]); err != nil {
				t.Errorf("tag t%02d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	list, err := s.ListTags()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != n {
		t.Fatalf("want %d distinct tags, got %d", n, len(list))
	}
	got := map[string]Digest{}
	for _, r := range list {
		if r.Name != "race/app" {
			t.Fatalf("unexpected name %s", r.Name)
		}
		got[r.Tag] = r.Digest
	}
	for i := 0; i < n; i++ {
		tag := fmt.Sprintf("t%02d", i)
		if got[tag] != digests[i] {
			t.Fatalf("tag %s points %s, want %s", tag, got[tag], digests[i])
		}
	}
	t.Logf("concurrent tagging converged: %d intact tags, no lost writes", n)

	// 同一标签并发改指向：必须留下一个完整（合法）结果
	const m = 16
	var wg2 sync.WaitGroup
	for i := 0; i < m; i++ {
		wg2.Add(1)
		go func(i int) {
			defer wg2.Done()
			if err := s.Tag("race/movable", "latest", digests[i%n]); err != nil {
				t.Errorf("retag: %v", err)
			}
		}(i)
	}
	wg2.Wait()
	win, err := s.Lookup("race/movable", "latest")
	if err != nil {
		t.Fatalf("lookup winner: %v", err)
	}
	found := false
	for _, d := range digests {
		if d == win {
			found = true
		}
	}
	if !found {
		t.Fatalf("final tag %s is not any complete result", win)
	}
	t.Logf("concurrent retag settled on %s", win)
}

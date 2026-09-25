package ocistore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
)

// newTestStore 在临时目录打开存储。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// digestOf 计算字节内容的 sha256 摘要字符串。
func digestOf(b []byte) Digest {
	sum := sha256.Sum256(b)
	return Digest("sha256:" + hex.EncodeToString(sum[:]))
}

// putLayer 写入一个层内容，返回其描述符。
func putLayer(t *testing.T, s *Store, data []byte) Descriptor {
	t.Helper()
	desc, err := s.PutBlob(context.Background(), bytes.NewReader(data), digestOf(data), int64(len(data)))
	if err != nil {
		t.Fatalf("put layer: %v", err)
	}
	return desc
}

// buildManifest 构造 config+layers，写入层与清单，返回清单摘要。
func buildManifest(t *testing.T, s *Store, config []byte, layers ...[]byte) Digest {
	t.Helper()
	ctx := context.Background()
	cfgDesc := putLayer(t, s, config)
	cfgDesc.MediaType = MediaTypeConfig
	layerDescs := make([]Descriptor, 0, len(layers))
	for _, l := range layers {
		d := putLayer(t, s, l)
		d.MediaType = MediaTypeLayerGzip
		layerDescs = append(layerDescs, d)
	}
	m := ImageManifest{
		SchemaVersion: 2,
		MediaType:     MediaTypeImageManifest,
		Config:        cfgDesc,
		Layers:        layerDescs,
	}
	raw, _ := json.Marshal(m)
	d, err := s.PutManifest(ctx, MediaTypeImageManifest, raw)
	if err != nil {
		t.Fatalf("put manifest: %v", err)
	}
	t.Logf("manifest built digest=%s config=%s layers=%v", d, cfgDesc.Digest, digestsOf(layerDescs))
	return d
}

func digestsOf(ds []Descriptor) []Digest {
	out := make([]Digest, len(ds))
	for i, d := range ds {
		out[i] = d.Digest
	}
	return out
}

// IndexEntry 是构造多架构 index 的一条目。
type IndexEntry struct {
	Platform Platform
	Digest   Digest
}

// buildIndex 用若干 (platform, manifestDigest) 构造 index 并写入。
func buildIndex(t *testing.T, s *Store, entries []IndexEntry) Digest {
	t.Helper()
	ix := ImageIndex{SchemaVersion: 2, MediaType: MediaTypeImageIndex}
	for _, e := range entries {
		pp := e.Platform
		ix.Manifests = append(ix.Manifests, Descriptor{
			MediaType: MediaTypeImageManifest,
			Digest:    e.Digest,
			Platform:  &pp,
		})
	}
	raw, _ := json.Marshal(ix)
	id, err := s.PutManifest(context.Background(), MediaTypeImageIndex, raw)
	if err != nil {
		t.Fatalf("put index: %v", err)
	}
	t.Logf("index built digest=%s entries=%d", id, len(entries))
	return id
}

// logRefs 打印当前 tag -> digest 引用关系，满足可解释性要求。
func logRefs(t *testing.T, s *Store) {
	t.Helper()
	tags, err := s.ListTags(context.Background())
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	for _, name := range tags {
		d, err := s.ResolveTag(context.Background(), name)
		if err != nil {
			t.Logf("ref %s -> ERROR %v", name, err)
			continue
		}
		t.Logf("ref %s -> %s (mediaType=%s size=%d)", name, d.Digest, d.MediaType, d.Size)
	}
}

// logBlobs 打印全部 blob 摘要与可达判定依据（由测试提供 reachable）。
func logBlobs(t *testing.T, s *Store, reachable map[Digest]struct{}) {
	t.Helper()
	all, err := s.blobs.ListBlobs(context.Background())
	if err != nil {
		t.Fatalf("list blobs: %v", err)
	}
	for _, d := range all {
		_, keep := reachable[d]
		reason := "unreachable-candidate"
		if keep {
			reason = "reachable-from-refs"
		}
		t.Logf("blob %s keep=%v reason=%s", d, keep, reason)
	}
}

func mustParseDigest(t *testing.T, x string) Digest {
	t.Helper()
	d, err := ParseDigest(x)
	if err != nil {
		t.Fatalf("parse digest %q: %v", x, err)
	}
	return d
}

func blobContent(t *testing.T, s *Store, d Digest) []byte {
	t.Helper()
	rc, _, err := s.GetBlob(context.Background(), d)
	if err != nil {
		t.Fatalf("get blob %s: %v", d, err)
	}
	defer rc.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(rc); err != nil {
		t.Fatalf("read blob: %v", err)
	}
	return buf.Bytes()
}

var _ = fmt.Sprintf

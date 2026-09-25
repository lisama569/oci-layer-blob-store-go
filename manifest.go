package ocistore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 本地存储支持的 OCI 内容类型。
const (
	MediaTypeImageManifest = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeImageIndex    = "application/vnd.oci.image.index.v1+json"
	MediaTypeLayer         = "application/vnd.oci.image.layer.v1.tar"
	MediaTypeLayerGzip     = "application/vnd.oci.image.layer.v1.tar+gzip"
	MediaTypeConfig        = "application/vnd.oci.image.config.v1+json"
)

// ImageManifest 对应 OCI image manifest。
type ImageManifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType,omitempty"`
	Config        Descriptor        `json:"config"`
	Layers        []Descriptor      `json:"layers"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// ImageIndex 对应 OCI image index（多架构清单）。
type ImageIndex struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType,omitempty"`
	Manifests     []Descriptor      `json:"manifests"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// ResolvedImage 是按平台解析后的目标镜像。
type ResolvedImage struct {
	ManifestDigest Digest
	Manifest       ImageManifest
}

// refRecord 是 refs/<tag> 文件的磁盘格式。
type refRecord struct {
	MediaType string `json:"mediaType"`
	Digest    Digest `json:"digest"`
	Size      int64  `json:"size"`
}

func (s *Store) refPath(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, "/\\") {
		return "", fmt.Errorf("invalid reference name %q", name)
	}
	enc := url.PathEscape(name)
	if enc == "." || enc == ".." || enc == "" {
		return "", fmt.Errorf("invalid reference name %q", name)
	}
	return filepath.Join(s.root, "refs", enc), nil
}

// PutManifest 写入清单内容。mediaType 必须是支持的两类清单类型；
// 同时按摘要规则做内容校验（调用方一般不知道摘要，故按实际内容定址）。
func (s *Store) PutManifest(ctx context.Context, mediaType string, raw []byte) (Digest, error) {
	if mediaType != MediaTypeImageManifest && mediaType != MediaTypeImageIndex {
		return "", fmt.Errorf("%w: %q", ErrUnsupportedMediaType, mediaType)
	}
	// 先做结构校验：拒绝无法解析或结构不符的清单内容。
	switch mediaType {
	case MediaTypeImageManifest:
		var m ImageManifest
		if err := json.Unmarshal(raw, &m); err != nil {
			return "", fmt.Errorf("parse image manifest: %w", err)
		}
	case MediaTypeImageIndex:
		var ix ImageIndex
		if err := json.Unmarshal(raw, &ix); err != nil {
			return "", fmt.Errorf("parse image index: %w", err)
		}
	}

	s.gcMu.RLock()
	defer s.gcMu.RUnlock()
	desc, err := s.blobs.PutBlob(ctx, bytes.NewReader(raw), "", int64(len(raw)))
	if err != nil {
		return "", err
	}
	return desc.Digest, nil
}

// loadManifestRaw 按摘要读取原始清单字节（不做类型判定）。
func (s *Store) loadManifestRaw(ctx context.Context, d Digest) ([]byte, error) {
	rc, _, err := s.blobs.GetBlob(ctx, d)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// decodeManifest 读取并解码摘要处的清单，返回其媒体类型与任一具体结构。
// 若摘要内容缺失返回 ErrNotFound；可解析但类型未知返回 ErrUnsupportedMediaType。
func (s *Store) decodeManifest(ctx context.Context, d Digest) (string, *ImageManifest, *ImageIndex, error) {
	raw, err := s.loadManifestRaw(ctx, d)
	if err != nil {
		return "", nil, nil, err
	}
	// 先尝试按 mediaType 字段识别（OCI 清单可自描述），否则按字段形态嗅探。
	var probe struct {
		MediaType string          `json:"mediaType"`
		Config    json.RawMessage `json:"config"`
		Manifests json.RawMessage `json:"manifests"`
	}
	if jerr := json.Unmarshal(raw, &probe); jerr != nil {
		return "", nil, nil, fmt.Errorf("%w: digest %s: %v", ErrUnsupportedMediaType, d, jerr)
	}
	switch {
	case probe.MediaType == MediaTypeImageManifest || (probe.MediaType == "" && len(probe.Config) > 0):
		var m ImageManifest
		if err := json.Unmarshal(raw, &m); err != nil {
			return "", nil, nil, err
		}
		return MediaTypeImageManifest, &m, nil, nil
	case probe.MediaType == MediaTypeImageIndex || (probe.MediaType == "" && len(probe.Manifests) > 0):
		var ix ImageIndex
		if err := json.Unmarshal(raw, &ix); err != nil {
			return "", nil, nil, err
		}
		return MediaTypeImageIndex, nil, &ix, nil
	default:
		return probe.MediaType, nil, nil, fmt.Errorf("%w: digest %s mediaType=%q", ErrUnsupportedMediaType, d, probe.MediaType)
	}
}

// GetManifest 按摘要读取原始清单及其媒体类型。
func (s *Store) GetManifest(ctx context.Context, d Digest) (string, []byte, error) {
	s.gcMu.RLock()
	defer s.gcMu.RUnlock()
	raw, err := s.loadManifestRaw(ctx, d)
	if err != nil {
		return "", nil, err
	}
	mt, _, _, err := s.decodeManifest(ctx, d)
	if err != nil {
		return "", nil, err
	}
	return mt, raw, nil
}

// Tag 给清单摘要打上命名引用。目标内容必须存在且是受支持的清单类型。
func (s *Store) Tag(ctx context.Context, name string, d Digest) (Descriptor, error) {
	p, err := s.refPath(name)
	if err != nil {
		return Descriptor{}, err
	}

	s.gcMu.RLock()
	defer s.gcMu.RUnlock()
	s.refsMu.Lock()
	defer s.refsMu.Unlock()

	mt, _, _, err := s.decodeManifest(ctx, d)
	if err != nil {
		return Descriptor{}, err
	}
	st, err := s.blobs.StatBlob(ctx, d)
	if err != nil {
		return Descriptor{}, err
	}
	rec := refRecord{MediaType: mt, Digest: d, Size: st.Size}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return Descriptor{}, err
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		return Descriptor{}, fmt.Errorf("write ref %s: %w", name, err)
	}
	return Descriptor{MediaType: mt, Digest: d, Size: st.Size}, nil
}

// Untag 删除命名引用；引用不存在返回 ErrRefNotFound。
func (s *Store) Untag(ctx context.Context, name string) error {
	p, err := s.refPath(name)
	if err != nil {
		return err
	}
	s.gcMu.RLock()
	defer s.gcMu.RUnlock()
	s.refsMu.Lock()
	defer s.refsMu.Unlock()
	if err := os.Remove(p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrRefNotFound, name)
		}
		return err
	}
	return nil
}

// ResolveTag 将命名引用解析到清单描述符。
func (s *Store) ResolveTag(ctx context.Context, name string) (Descriptor, error) {
	p, err := s.refPath(name)
	if err != nil {
		return Descriptor{}, err
	}
	s.gcMu.RLock()
	defer s.gcMu.RUnlock()
	return s.readRefLocked(ctx, p, name)
}

// readRefLocked 读取并校验 ref 文件；ref 指向的 blob 缺失视为悬空引用。
func (s *Store) readRefLocked(ctx context.Context, p, name string) (Descriptor, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Descriptor{}, fmt.Errorf("%w: %s", ErrRefNotFound, name)
		}
		return Descriptor{}, err
	}
	var rec refRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return Descriptor{}, fmt.Errorf("corrupt ref file %s: %w", name, err)
	}
	if _, err := s.blobs.StatBlob(ctx, rec.Digest); err != nil {
		if errors.Is(err, ErrNotFound) {
			return Descriptor{}, &DanglingReferenceError{Location: "ref:" + name, Missing: rec.Digest}
		}
		return Descriptor{}, err
	}
	return Descriptor{MediaType: rec.MediaType, Digest: rec.Digest, Size: rec.Size}, nil
}

// ListTags 列出全部命名引用（按名称排序）。
func (s *Store) ListTags(ctx context.Context) ([]string, error) {
	s.gcMu.RLock()
	defer s.gcMu.RUnlock()
	entries, err := os.ReadDir(filepath.Join(s.root, "refs"))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		dec, derr := url.PathUnescape(e.Name())
		if derr != nil {
			continue
		}
		names = append(names, dec)
	}
	sort.Strings(names)
	return names, nil
}

// Resolve 从根（tag 名称或 "sha256:..." 摘要）出发，按平台解析目标镜像。
//
// 解析规则（失败必须明确，不得任意挑选）：
//   - 根为 tag：缺失返回 ErrRefNotFound，指向缺失 blob 返回 ErrDanglingReference；
//   - 遇到 index：按平台精确匹配（os/architecture/variant），
//     无匹配返回 ErrPlatformNotFound（携带可用平台列表）；
//   - 遇到 manifest 直接返回；
//   - index 条目指向的内容缺失：ErrDanglingReference；
//   - 遇到不支持的内容类型：ErrUnsupportedMediaType。
//
// index 允许嵌套（index 指向 index）。
func (s *Store) Resolve(ctx context.Context, root string, p Platform) (ResolvedImage, error) {
	s.gcMu.RLock()
	defer s.gcMu.RUnlock()

	var cur Digest
	if d, err := ParseDigest(root); err == nil {
		cur = d
		if _, serr := s.blobs.StatBlob(ctx, cur); serr != nil {
			return ResolvedImage{}, serr
		}
	} else {
		path, rerr := s.refPath(root)
		if rerr != nil {
			return ResolvedImage{}, rerr
		}
		desc, rerr := s.readRefLocked(ctx, path, root)
		if rerr != nil {
			return ResolvedImage{}, rerr
		}
		cur = desc.Digest
	}

	return s.resolveLocked(ctx, cur, p, "root:"+root)
}

func (s *Store) resolveLocked(ctx context.Context, d Digest, want Platform, location string) (ResolvedImage, error) {
	mt, manifest, index, err := s.decodeManifest(ctx, d)
	if err != nil {
		return ResolvedImage{}, err
	}
	switch mt {
	case MediaTypeImageManifest:
		return ResolvedImage{ManifestDigest: d, Manifest: *manifest}, nil
	case MediaTypeImageIndex:
		return s.resolveIndexLocked(ctx, *index, want, d)
	default:
		return ResolvedImage{}, fmt.Errorf("%w: %s at %s", ErrUnsupportedMediaType, mt, location)
	}
}

func (s *Store) resolveIndexLocked(ctx context.Context, ix ImageIndex, want Platform, indexDigest Digest) (ResolvedImage, error) {
	var available []Platform
	var matched *Descriptor
	for i := range ix.Manifests {
		child := ix.Manifests[i]
		if child.Platform != nil {
			available = append(available, *child.Platform)
		}
		if child.Platform != nil && platformMatch(*child.Platform, want) && matched == nil {
			cp := child
			matched = &cp
		}
	}
	if matched == nil {
		return ResolvedImage{}, &PlatformNotFoundError{Wanted: want, Available: available}
	}
	// 选中条目必须存在；缺失即为悬空引用。
	if _, err := s.blobs.StatBlob(ctx, matched.Digest); err != nil {
		if errors.Is(err, ErrNotFound) {
			return ResolvedImage{}, &DanglingReferenceError{Location: "index:" + string(indexDigest), Missing: matched.Digest}
		}
		return ResolvedImage{}, err
	}
	return s.resolveLocked(ctx, matched.Digest, want, "index:"+string(indexDigest))
}

// platformMatch 按 OCI 规则做平台匹配：
// os 与 architecture 必须相等；目标指定了 variant 时必须相等，
// 目标未指定 variant 时接受任意 variant。
func platformMatch(have, want Platform) bool {
	if want.OS != have.OS || want.Architecture != have.Architecture {
		return false
	}
	if want.Variant != "" && want.Variant != have.Variant {
		return false
	}
	return true
}

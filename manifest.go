package ocistore

import (
	"encoding/json"
	"fmt"
)

// 支持的媒体类型。
const (
	MediaTypeImageManifest      = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeImageIndex         = "application/vnd.oci.image.index.v1+json"
	MediaTypeDockerManifest     = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeDockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
)

// Platform 描述目标平台。
type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

// Descriptor 是 OCI 描述符。
type Descriptor struct {
	MediaType string    `json:"mediaType"`
	Digest    Digest    `json:"digest"`
	Size      int64     `json:"size"`
	Platform  *Platform `json:"platform,omitempty"`
}

// Manifest 是已解析的镜像 manifest。
type Manifest struct {
	MediaType string       `json:"mediaType"`
	Config    Descriptor   `json:"config"`
	Layers    []Descriptor `json:"layers"`
}

// Index 是已解析的多架构 index。
type Index struct {
	MediaType string       `json:"mediaType"`
	Manifests []Descriptor `json:"manifests"`
}

// ParseManifestOrIndex 解析 JSON 为 *Manifest 或 *Index。
// 媒体类型缺失时按字段嗅探；不支持的类型返回 ErrUnsupportedMediaType。
func ParseManifestOrIndex(data []byte) (any, error) {
	var head struct {
		MediaType string          `json:"mediaType"`
		Layers    json.RawMessage `json:"layers"`
		Manifests json.RawMessage `json:"manifests"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("ocistore: invalid JSON: %w", err)
	}

	isManifest := head.MediaType == MediaTypeImageManifest || head.MediaType == MediaTypeDockerManifest
	isIndex := head.MediaType == MediaTypeImageIndex || head.MediaType == MediaTypeDockerManifestList
	if head.MediaType == "" {
		// 嗅探：有 layers 视为 manifest，有 manifests 视为 index。
		switch {
		case len(head.Layers) > 0:
			isManifest = true
		case len(head.Manifests) > 0:
			isIndex = true
		}
	}

	switch {
	case isManifest:
		var m Manifest
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("ocistore: invalid manifest: %w", err)
		}
		return &m, nil
	case isIndex:
		var idx Index
		if err := json.Unmarshal(data, &idx); err != nil {
			return nil, fmt.Errorf("ocistore: invalid index: %w", err)
		}
		return &idx, nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedMediaType, head.MediaType)
	}
}

// SelectPlatform 从 index 中按平台挑选目标镜像描述符。
// 无任何匹配时返回 ErrPlatformNotFound，绝不任意挑选。
func SelectPlatform(idx *Index, p Platform) (Descriptor, error) {
	for _, d := range idx.Manifests {
		if d.Platform == nil {
			continue // 无平台信息的条目不参与匹配（如 attestation）
		}
		if d.Platform.OS != p.OS || d.Platform.Architecture != p.Architecture {
			continue
		}
		if p.Variant != "" && d.Platform.Variant != p.Variant {
			continue
		}
		return d, nil
	}
	return Descriptor{}, fmt.Errorf("%w: %s/%s (variant %q) among %d entries",
		ErrPlatformNotFound, p.OS, p.Architecture, p.Variant, len(idx.Manifests))
}

// Refs 返回 manifest 引用的全部摘要（config + layers）。
func (m *Manifest) Refs() []Digest {
	refs := make([]Digest, 0, len(m.Layers)+1)
	if m.Config.Digest != "" {
		refs = append(refs, m.Config.Digest)
	}
	for _, l := range m.Layers {
		refs = append(refs, l.Digest)
	}
	return refs
}

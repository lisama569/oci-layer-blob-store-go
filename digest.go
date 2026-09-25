package ocistore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Digest 表示内容摘要，形如 "sha256:<hex>"。
type Digest string

// String 返回摘要的字符串形式。
func (d Digest) String() string { return string(d) }

// Validate 校验摘要格式是否合法（目前仅支持 sha256）。
func (d Digest) Validate() error {
	algo, hexPart, ok := strings.Cut(string(d), ":")
	if !ok || algo == "" || hexPart == "" {
		return fmt.Errorf("%w: %q (want <algorithm>:<hex>)", ErrInvalidDigest, d)
	}
	if algo != "sha256" {
		return fmt.Errorf("%w: unsupported algorithm %q", ErrInvalidDigest, algo)
	}
	if len(hexPart) != sha256.Size*2 {
		return fmt.Errorf("%w: sha256 hex must be %d chars, got %d", ErrInvalidDigest, sha256.Size*2, len(hexPart))
	}
	if _, err := hex.DecodeString(hexPart); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidDigest, err)
	}
	return nil
}

// Algorithm 返回摘要算法部分。
func (d Digest) Algorithm() string {
	algo, _, _ := strings.Cut(string(d), ":")
	return algo
}

// Hex 返回摘要的十六进制部分。
func (d Digest) Hex() string {
	_, h, _ := strings.Cut(string(d), ":")
	return h
}

// DigestOf 计算内容的 sha256 摘要。
func DigestOf(b []byte) Digest {
	sum := sha256.Sum256(b)
	return Digest("sha256:" + hex.EncodeToString(sum[:]))
}

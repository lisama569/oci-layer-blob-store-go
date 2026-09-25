package ocistore

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"strings"
)

// Digest 是内容摘要，形如 "sha256:hex..."。
type Digest string

// Descriptor 描述一段按摘要寻址的内容。
type Descriptor struct {
	MediaType string `json:"mediaType,omitempty"`
	Digest    Digest `json:"digest,omitempty"`
	Size      int64  `json:"size"`
	// Platform 仅在 index 的 children 条目中出现。
	Platform    *Platform         `json:"platform,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Platform 描述镜像条目适用的运行平台（os/arch[/variant]）。
type Platform struct {
	Architecture string   `json:"architecture,omitempty"`
	OS           string   `json:"os,omitempty"`
	OSVersion    string   `json:"os.version,omitempty"`
	OSFeatures   []string `json:"os.features,omitempty"`
	Variant      string   `json:"variant,omitempty"`
}

func (p Platform) String() string {
	if p.OS == "" && p.Architecture == "" {
		return ""
	}
	if p.Variant != "" {
		return p.OS + "/" + p.Architecture + "/" + p.Variant
	}
	return p.OS + "/" + p.Architecture
}

// ParseDigest 解析 "algorithm:hex" 形式的摘要字符串。
// 仅支持 sha256/sha384/sha512，且十六进制长度必须与算法输出一致。
func ParseDigest(s string) (Digest, error) {
	algo, enc, ok := strings.Cut(s, ":")
	if !ok || enc == "" {
		return "", fmt.Errorf("%w: %q: missing algorithm or encoded part", ErrInvalidDigest, s)
	}
	if err := validateEncoded(algo, enc); err != nil {
		return "", err
	}
	return Digest(s), nil
}

func validateEncoded(algo, enc string) error {
	var wantLen int
	switch algo {
	case "sha256":
		wantLen = 64
	case "sha384":
		wantLen = 96
	case "sha512":
		wantLen = 128
	default:
		return fmt.Errorf("%w: unsupported algorithm %q", ErrInvalidDigest, algo)
	}
	if len(enc) != wantLen {
		return fmt.Errorf("%w: %s encoded length = %d, want %d", ErrInvalidDigest, algo, len(enc), wantLen)
	}
	if _, err := hex.DecodeString(enc); err != nil {
		return fmt.Errorf("%w: %s not hexadecimal: %v", ErrInvalidDigest, algo, err)
	}
	return nil
}

// Algorithm 返回摘要算法名。
func (d Digest) Algorithm() string {
	algo, _, _ := strings.Cut(string(d), ":")
	return algo
}

// Hex 返回摘要的十六进制部分。
func (d Digest) Hex() string {
	_, enc, _ := strings.Cut(string(d), ":")
	return enc
}

func (d Digest) String() string { return string(d) }

// newHasher 按摘要算法构造 hash，空摘要默认 sha256。
func newHasher(algo string) (hash.Hash, error) {
	switch algo {
	case "", "sha256":
		return sha256.New(), nil
	case "sha384":
		return sha512.New384(), nil
	case "sha512":
		return sha512.New(), nil
	default:
		return nil, fmt.Errorf("%w: unsupported algorithm %q", ErrInvalidDigest, algo)
	}
}

// verifyingReader 在透传数据的同时计算摘要与字节数，
// 在流结束（或中途出错）时按声明值校验，产出可区分的失败原因。
type verifyingReader struct {
	r            io.Reader
	h            hash.Hash
	expected     Digest
	declaredSize int64 // < 0 表示未声明
	n            int64
	verified     bool
}

// VerifiedReader 包装 r：调用方读到 EOF 后必须继续调用返回 reader 的
// Close/Verify 才能得到校验结论；这里通过 Close 完成校验。
func VerifiedReader(r io.Reader, expected Digest, declaredSize int64) (io.ReadCloser, error) {
	algo := expected.Algorithm()
	h, err := newHasher(algo)
	if err != nil {
		return nil, err
	}
	return &verifyingReader{r: r, h: h, expected: expected, declaredSize: declaredSize}, nil
}

func (v *verifyingReader) Read(p []byte) (int, error) {
	n, err := v.r.Read(p)
	if n > 0 {
		v.h.Write(p[:n])
		v.n += int64(n)
	}
	if err == io.EOF {
		if verifyErr := v.verify(); verifyErr != nil {
			return n, verifyErr
		}
	}
	return n, err
}

// Close 在校验尚未完成时（例如调用方提前停止读取）补做校验，
// 并尽力关闭底层 reader。
func (v *verifyingReader) Close() error {
	if c, ok := v.r.(io.Closer); ok {
		_ = c.Close()
	}
	if !v.verified {
		return v.verify()
	}
	return nil
}

// verify 按“先大小、后摘要”的顺序给出可区分结论：
//   - 实际短于声明：ErrTruncated（截断通常同时导致摘要不符，截断是更直接原因）
//   - 实际长于声明：ErrSizeMismatch
//   - 大小一致但摘要不符：ErrDigestMismatch
func (v *verifyingReader) verify() error {
	v.verified = true
	if v.declaredSize >= 0 && v.n < v.declaredSize {
		return &TruncatedError{Declared: v.declaredSize, Actual: v.n}
	}
	if v.declaredSize >= 0 && v.n > v.declaredSize {
		return &SizeMismatchError{Declared: v.declaredSize, Actual: v.n}
	}
	got := v.computedDigest()
	if v.expected != "" && got != v.expected {
		return &DigestMismatchError{Expected: v.expected, Got: got}
	}
	return nil
}

// computedDigest 返回流式计算出的实际摘要。
func (v *verifyingReader) computedDigest() Digest {
	algo := v.expected.Algorithm()
	if algo == "" {
		algo = "sha256"
	}
	return Digest(algo + ":" + hex.EncodeToString(v.h.Sum(nil)))
}

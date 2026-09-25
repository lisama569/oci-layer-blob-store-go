package ocistore

import (
	"errors"
	"fmt"
)

// 可区分的失败原因，调用方可用 errors.Is 判定。
var (
	// ErrInvalidDigest 摘要字符串格式非法。
	ErrInvalidDigest = errors.New("oci-store: invalid digest")
	// ErrNotFound 按摘要找不到 blob 内容。
	ErrNotFound = errors.New("oci-store: blob not found")
	// ErrRefNotFound 命名引用（tag）不存在。
	ErrRefNotFound = errors.New("oci-store: reference not found")
	// ErrDigestMismatch 实际内容摘要与声明摘要不一致。
	ErrDigestMismatch = errors.New("oci-store: digest mismatch")
	// ErrSizeMismatch 实际字节数与声明大小不一致（非截断类）。
	ErrSizeMismatch = errors.New("oci-store: declared size mismatch")
	// ErrTruncated 实际内容短于声明大小，疑似被截断。
	ErrTruncated = errors.New("oci-store: content truncated")
	// ErrUnsupportedMediaType 清单/索引内容类型无法识别。
	ErrUnsupportedMediaType = errors.New("oci-store: unsupported media type")
	// ErrPlatformNotFound index 中不存在与目标平台匹配的条目。
	ErrPlatformNotFound = errors.New("oci-store: no manifest for platform")
	// ErrDanglingReference 引用指向的内容缺失（悬空引用）。
	ErrDanglingReference = errors.New("oci-store: dangling reference")
)

// DigestMismatchError 携带期望与实际摘要，便于日志解释判定依据。
type DigestMismatchError struct {
	Expected Digest
	Got      Digest
}

func (e *DigestMismatchError) Error() string {
	return fmt.Sprintf("digest mismatch: expected=%s got=%s", e.Expected, e.Got)
}

func (e *DigestMismatchError) Is(target error) bool { return target == ErrDigestMismatch }

// SizeMismatchError 实际长度超过声明大小时使用。
type SizeMismatchError struct {
	Declared int64
	Actual   int64
}

func (e *SizeMismatchError) Error() string {
	return fmt.Sprintf("size mismatch: declared=%d actual=%d", e.Declared, e.Actual)
}

func (e *SizeMismatchError) Is(target error) bool { return target == ErrSizeMismatch }

// TruncatedError 实际长度短于声明大小时使用。
type TruncatedError struct {
	Declared int64
	Actual   int64
}

func (e *TruncatedError) Error() string {
	return fmt.Sprintf("content truncated: declared=%d actual=%d", e.Declared, e.Actual)
}

func (e *TruncatedError) Is(target error) bool { return target == ErrTruncated }

// DanglingReferenceError 描述悬空发生的位置与缺失摘要。
type DanglingReferenceError struct {
	Location string // 引用来源，例如 ref 名称或 index 摘要
	Missing  Digest
}

func (e *DanglingReferenceError) Error() string {
	return fmt.Sprintf("dangling reference at %s: missing blob %s", e.Location, e.Missing)
}

func (e *DanglingReferenceError) Is(target error) bool { return target == ErrDanglingReference }

// PlatformNotFoundError 携带目标平台与可选平台列表。
type PlatformNotFoundError struct {
	Wanted    Platform
	Available []Platform
}

func (e *PlatformNotFoundError) Error() string {
	return fmt.Sprintf("no manifest matched platform %s", e.Wanted)
}

func (e *PlatformNotFoundError) Is(target error) bool { return target == ErrPlatformNotFound }

package ocistore

import "errors"

// 可区分的失败原因，调用方可用 errors.Is 判定。
var (
	ErrDigestMismatch       = errors.New("ocistore: digest mismatch")
	ErrSizeMismatch         = errors.New("ocistore: size mismatch")
	ErrTruncated            = errors.New("ocistore: content truncated")
	ErrNotFound             = errors.New("ocistore: content not found")
	ErrUnsupportedMediaType = errors.New("ocistore: unsupported media type")
	ErrPlatformNotFound     = errors.New("ocistore: no matching platform")
	ErrDanglingRef          = errors.New("ocistore: dangling reference")
	ErrInvalidDigest        = errors.New("ocistore: invalid digest")
)

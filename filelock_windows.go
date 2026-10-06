//go:build windows

package ocistore

import "os"

// Windows 平台退化为仅依赖进程内锁（仓库主要面向 POSIX 本地文件系统）。
type fileLock struct{ f *os.File }

func acquireFileLock(path string, exclusive bool) (*fileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	return &fileLock{f: f}, nil
}

func (l *fileLock) release() error { return l.f.Close() }

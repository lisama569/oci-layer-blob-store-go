package ocistore

import (
	"os"
	"os/exec"
	"testing"
)

// newTestHelperCmd 以当前测试二进制启动一个带 OCI_TAG_HELPER 标记的子进程。
func newTestHelperCmd(t *testing.T, root, action string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestTagsPersistAcrossProcess")
	cmd.Env = append(os.Environ(),
		"OCI_TAG_HELPER=1",
		"OCI_TAG_ROOT="+root,
	)
	return cmd
}

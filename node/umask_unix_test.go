//go:build unix

package node_test

import (
	"os"
	"syscall"
	"testing"
)

// TestMain sets the umask to 022, as testdata/fsgen.mjs does when it
// records fs_node.json, so that the modes of new files and directories
// do not depend on the umask the tests start with.
func TestMain(m *testing.M) {
	syscall.Umask(0o022)
	os.Exit(m.Run())
}

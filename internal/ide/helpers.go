package ide

import (
	"os"
	"os/exec"
	"runtime"
)

// findBinary checks if a binary is on PATH.
func findBinary(name string) (string, error) {
	return exec.LookPath(name)
}

// exists returns true when the path exists, regardless of file type.
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// getCWD wraps os.Getwd with a clearer error.
func getCWD() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return cwd, nil
}

// goos indirects runtime.GOOS so the OS-specific config-path branches are
// reachable from tests on any host. Every adapter resolves a different
// directory per platform, and a wrong path writes an MCP config the IDE never
// reads -- a silent failure this seam lets us pin.
var goos = runtime.GOOS

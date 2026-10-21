//go:build !darwin && !linux

package installation

import "os"

func renameExclusive(_ *os.Root, _, _ string) error { return failure(Unavailable) }

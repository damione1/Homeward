//go:build !darwin && !linux

package archive

import "os"

func renameExclusive(_ *os.Root, _, _ string) error { return fail(Unsupported) }

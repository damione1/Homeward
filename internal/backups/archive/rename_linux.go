package archive

import (
	"golang.org/x/sys/unix"
	"os"
)

func renameExclusive(root *os.Root, from, to string) error {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	return unix.Renameat2(int(f.Fd()), from, int(f.Fd()), to, unix.RENAME_NOREPLACE)
}

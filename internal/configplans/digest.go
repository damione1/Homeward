package configplans

import (
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"sort"

	"github.com/damione1/Homeward/compat/ha/parser"
)

func part(h hash.Hash, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = h.Write(size[:])
	_, _ = h.Write(value)
}
func sourceDigest(files []parser.SourceFile) [32]byte {
	// A single source's hash remains the exact bytes hash used by 149/151.
	if len(files) == 1 {
		return files[0].SHA256()
	}
	sorted := append([]parser.SourceFile(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path() < sorted[j].Path() })
	h := sha256.New()
	part(h, []byte("homeward.plan.sources/1"))
	for _, file := range sorted {
		part(h, []byte(file.Path()))
		sum := file.SHA256()
		part(h, sum[:])
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

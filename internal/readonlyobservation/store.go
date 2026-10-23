// Package readonlyobservation collects sanitized evidence, never action calls.
package readonlyobservation

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const SegmentBytes = 16 << 20
const RunBytes = 1 << 30
const MaxRecordBytes = 2 << 20

var ErrStorage = errors.New("observation evidence unavailable or full")

type Record struct {
	Sequence uint64          `json:"sequence"`
	UTC      time.Time       `json:"utc"`
	Kind     string          `json:"kind"`
	Data     json.RawMessage `json:"data"`
}

type Checkpoint struct {
	FatalReason      string        `json:"fatal_reason,omitempty"`
	TotalSummary     Summary       `json:"total_summary"`
	WindowSummary    Summary       `json:"window_summary"`
	Sequence         uint64        `json:"sequence"`
	Segment          int           `json:"segment"`
	Bytes            int64         `json:"bytes"`
	WindowStart      time.Time     `json:"window_start"`
	WindowElapsed    time.Duration `json:"window_elapsed_ns"`
	LastSample       time.Time     `json:"last_sample"`
	ProcessEpoch     string        `json:"process_epoch"`
	LogSequence      uint64        `json:"log_sequence"`
	Samples          uint64        `json:"samples"`
	Interruptions    uint64        `json:"interruptions"`
	Complete         bool          `json:"complete"`
	RecoveredPartial bool          `json:"recovered_partial"`
}

type Store struct {
	dir        string
	lock, file *os.File
	size       int64
	State      Checkpoint
}

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, ErrStorage
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrStorage
	}
	lock, err := os.OpenFile(filepath.Join(dir, "writer.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, ErrStorage
	}
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		lock.Close()
		return nil, ErrStorage
	}
	s := &Store{dir: dir, lock: lock}
	fail := func() (*Store, error) { s.Close(); return nil, ErrStorage }
	raw, err := os.ReadFile(filepath.Join(dir, "checkpoint.json"))
	if err == nil {
		if len(raw) > 64<<10 || json.Unmarshal(raw, &s.State) != nil || s.State.Segment < 0 || s.State.Sequence == 0 {
			return fail()
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fail()
	}
	// Every incarnation opens a new exclusive segment. A crash after append but
	// before checkpoint cannot overwrite old evidence or silently reuse a cursor.
	entries, err := filepath.Glob(filepath.Join(dir, "segment-*.ndjson"))
	if err != nil || len(entries) > 128 {
		return fail()
	}
	var total int64
	last := 0
	for _, path := range entries {
		var index int
		if _, err := fmt.Sscanf(filepath.Base(path), "segment-%06d.ndjson", &index); err != nil || index < 1 {
			return fail()
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > SegmentBytes {
			return fail()
		}
		total += info.Size()
		if index > last {
			last = index
		}
	}
	if total > RunBytes {
		return fail()
	}
	rows, partial, maximum, err := scanEvidence(dir)
	if err != nil || s.State.Sequence > maximum || (s.State.Sequence > 0 && rows == 0) {
		return fail()
	}
	s.State.Sequence = maximum
	s.State.RecoveredPartial = partial
	s.State.Bytes = total
	if last > s.State.Segment {
		s.State.Segment = last
	}
	if err := s.rotate(); err != nil {
		return fail()
	}
	return s, nil
}

func (s *Store) rotate() error {
	if s.file != nil {
		if err := s.file.Close(); err != nil {
			return ErrStorage
		}
	}
	s.State.Segment++
	file, err := os.OpenFile(filepath.Join(s.dir, fmt.Sprintf("segment-%06d.ndjson", s.State.Segment)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return ErrStorage
	}
	s.file = file
	s.size = 0
	return syncDirectory(s.dir)
}

func (s *Store) Append(kind string, data any) error {
	if s == nil || s.file == nil || len(kind) > 64 {
		return ErrStorage
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return ErrStorage
	}
	row := Record{Sequence: s.State.Sequence + 1, UTC: time.Now().UTC(), Kind: kind, Data: payload}
	raw, err := json.Marshal(row)
	if err != nil || len(raw)+1 > MaxRecordBytes {
		return ErrStorage
	}
	raw = append(raw, '\n')
	if s.State.Bytes+int64(len(raw)) > RunBytes {
		return ErrStorage
	}
	if s.size+int64(len(raw)) > SegmentBytes {
		if err := s.rotate(); err != nil {
			return err
		}
	}
	if _, err := s.file.Write(raw); err != nil {
		return ErrStorage
	}
	if err := s.file.Sync(); err != nil {
		return ErrStorage
	}
	s.size += int64(len(raw))
	s.State.Bytes += int64(len(raw))
	s.State.Sequence++
	return s.Checkpoint()
}
func (s *Store) Checkpoint() error      { return atomicJSON(s.dir, "checkpoint.json", s.State) }
func (s *Store) Report(value any) error { return atomicJSON(s.dir, "report.json", value) }
func atomicJSON(dir, name string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return ErrStorage
	}
	raw = append(raw, '\n')
	file, err := os.CreateTemp(dir, ".pending-")
	if err != nil {
		return ErrStorage
	}
	path := file.Name()
	defer os.Remove(path)
	if file.Chmod(0600) != nil {
		file.Close()
		return ErrStorage
	}
	_, err = file.Write(raw)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return ErrStorage
	}
	if os.Rename(path, filepath.Join(dir, name)) != nil {
		return ErrStorage
	}
	return syncDirectory(dir)
}
func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return ErrStorage
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return ErrStorage
	}
	return nil
}
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	var result error
	if s.file != nil {
		result = s.file.Close()
		s.file = nil
	}
	if s.lock != nil {
		_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		err := s.lock.Close()
		s.lock = nil
		result = errors.Join(result, err)
	}
	return result
}

// ValidateEvidence counts complete durable records. A partial final row is
// explicitly reported, never silently counted as an observation.
func ValidateEvidence(dir string) (rows uint64, partial bool, err error) {
	rows, partial, _, err = scanEvidence(dir)
	return
}
func scanEvidence(dir string) (rows uint64, partial bool, maximum uint64, err error) {
	paths, err := filepath.Glob(filepath.Join(dir, "segment-*.ndjson"))
	if err != nil {
		return 0, false, 0, ErrStorage
	}
	for _, path := range paths {
		file, e := os.Open(path)
		if e != nil {
			return rows, partial, maximum, ErrStorage
		}
		reader := bufio.NewReaderSize(file, 64<<10)
		for {
			line, e := reader.ReadBytes('\n')
			if len(line) > MaxRecordBytes {
				file.Close()
				return rows, partial, maximum, ErrStorage
			}
			if e == io.EOF {
				if len(line) > 0 {
					partial = true
				}
				break
			}
			if e != nil {
				file.Close()
				return rows, partial, maximum, ErrStorage
			}
			var record Record
			if json.Unmarshal(line, &record) != nil || record.Sequence == 0 || record.Sequence != maximum+1 {
				file.Close()
				return rows, partial, maximum, ErrStorage
			}
			maximum = record.Sequence
			rows++
		}
		if file.Close() != nil {
			return rows, partial, maximum, ErrStorage
		}
	}
	return rows, partial, maximum, nil
}

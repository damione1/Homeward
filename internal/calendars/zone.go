package calendars

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"io"
	"slices"
	"strings"
	"sync"
	"time"
)

// TimezoneDataID versions the exact data, without inventing an IANA release.
// Its source/licences are in THIRD-PARTY-NOTICES.txt and the Obsidian report.
const TimezoneDataID = "go1.27.2/sha256:b2d18a7c8fa8142097a48c99609fb3c92db5ee98bc740294e57eab8ae9f94779"
const timezoneSHA256 = "b2d18a7c8fa8142097a48c99609fb3c92db5ee98bc740294e57eab8ae9f94779"

//go:embed zoneinfo.zip
var timezoneArchive []byte

type zoneCache struct {
	mu        sync.Mutex
	entries   map[string]*zip.File
	locations map[string]*time.Location
	used      int64
	err       error
	once      sync.Once
}

var sharedZones zoneCache

func (c *zoneCache) index() {
	if len(timezoneArchive) > 512*1024 {
		c.err = failure(InvalidZone)
		return
	}
	hash := sha256.Sum256(timezoneArchive)
	if hex.EncodeToString(hash[:]) != timezoneSHA256 {
		c.err = failure(InvalidZone)
		return
	}
	reader, err := zip.NewReader(bytes.NewReader(timezoneArchive), int64(len(timezoneArchive)))
	if err != nil || len(reader.File) > 1024 {
		c.err = failure(InvalidZone)
		return
	}
	entries := make(map[string]*zip.File, len(reader.File))
	var total uint64
	for _, entry := range reader.File {
		// All checks precede entry.Open/decompression. The qualified archive
		// contains stored entries; no deflate stream or ambient file is used.
		if len(entry.Name) == 0 || len(entry.Name) > 128 || strings.HasPrefix(entry.Name, "/") || strings.Contains(entry.Name, "..") || entry.Method != zip.Store || entry.UncompressedSize64 > 4096 || entry.CompressedSize64 == 0 || entry.UncompressedSize64 > entry.CompressedSize64 {
			c.err = failure(InvalidZone)
			return
		}
		if _, duplicate := entries[entry.Name]; duplicate {
			c.err = failure(InvalidZone)
			return
		}
		total += entry.UncompressedSize64
		if total > 512*1024 {
			c.err = failure(InvalidZone)
			return
		}
		entries[entry.Name] = entry
	}
	c.entries = entries
	c.locations = make(map[string]*time.Location)
}
func (c *zoneCache) location(name string) (*time.Location, error) {
	c.once.Do(c.index)
	if c.err != nil {
		return nil, c.err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.locations[name]; ok {
		return existing, nil
	}
	entry, ok := c.entries[name]
	if !ok {
		return nil, failure(InvalidZone)
	}
	// A source-bounded32x TZif allowance plus4KiB covers retained Go zone/
	// transition objects, input/decompression buffers and temporary parsing.
	// Misses are serialized; names/bytes are never evicted while Zones live.
	charge := int64(entry.UncompressedSize64)*32 + 4096
	if len(c.locations) >= 64 || c.used+charge > 4*1024*1024 {
		return nil, failure(CapacityExceeded)
	}
	reader, err := entry.Open()
	if err != nil {
		return nil, failure(InvalidZone)
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, 4097))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || len(data) > 4096 || uint64(len(data)) != entry.UncompressedSize64 {
		return nil, failure(InvalidZone)
	}
	location, err := time.LoadLocationFromTZData(name, data)
	if err != nil {
		return nil, failure(InvalidZone)
	}
	c.locations[name] = location
	c.used += charge
	return location, nil
}

type Zone struct {
	name     string
	location *time.Location
	embedded *embeddedZone
}

func ParseZone(name string) (Zone, error) {
	if name == "" || len(name) > 128 || name == "Local" || strings.ContainsAny(name, "\x00\r\n\\") {
		return Zone{}, failure(InvalidZone)
	}
	name = strings.Clone(name)
	location, err := sharedZones.location(name)
	if err != nil {
		return Zone{}, err
	}
	return Zone{name: name, location: location}, nil
}
func (z Zone) Valid() bool {
	return z.name != "" && (z.location != nil) != (z.embedded != nil) && z.name != "Local"
}
func (z Zone) Name() string {
	if !z.Valid() {
		return ""
	}
	return z.name
}
func (z Zone) DataID() string {
	if !z.Valid() {
		return ""
	}
	if z.embedded != nil {
		return z.embedded.dataID
	}
	return TimezoneDataID
}

func (z Zone) offsetAt(ctx context.Context, at time.Time) (int, error) {
	if !z.Valid() {
		return 0, failure(InvalidZone)
	}
	if z.embedded != nil {
		return z.embedded.offsetAt(ctx, at)
	}
	_, offset := at.In(z.location).Zone()
	return offset, nil
}

// The embedded timeline can contain Go zero as a real raw transition.
// Absence must therefore be named separately from its time value.
type zoneBoundary struct {
	at      time.Time
	present bool
}

func (z Zone) boundsAt(ctx context.Context, at time.Time) (zoneBoundary, error) {
	if !z.Valid() {
		return zoneBoundary{}, failure(InvalidZone)
	}
	if z.embedded != nil {
		return z.embedded.nextTransition(ctx, at)
	}
	_, end := at.In(z.location).ZoneBounds()
	// The pinned IANA API represents an unbounded edge by its documented zero.
	return zoneBoundary{at: end, present: !end.IsZero()}, nil
}
func (z Zone) localAt(ctx context.Context, at time.Time) (LocalDateTime, error) {
	if !z.Valid() {
		return LocalDateTime{}, failure(InvalidZone)
	}
	if z.embedded == nil {
		return localFromTime(at.In(z.location)), nil
	}
	offset, err := z.embedded.offsetAt(ctx, at)
	if err != nil {
		return LocalDateTime{}, err
	}
	local := localFromTime(at.UTC().Add(time.Duration(offset) * time.Second))
	// Candidate projections outside the civil domain are absent guesses, as
	// on the IANA branch; boundary constructors still reject invalid locals.
	return local, nil
}

// At most16 actual transitions in a52h envelope are inspected. Fold chooses
// the first instant. A generated gap is absent; a DTSTART/RDATE DATE-TIME value
// uses the previous offset (RFC5545 3.3.5 versus3.3.10). No zoned time.Date.
func resolveWall(ctx context.Context, value LocalDateTime, zone Zone, initial bool) (Instant, bool, error) {
	raw, present, err := resolveWallTime(ctx, value, zone, initial)
	if err != nil || !present {
		return Instant{}, false, err
	}
	instant, err := NewInstant(raw)
	return instant, err == nil, err
}

// Projection of civil DATE boundaries may use the raw year-zero edge without
// admitting that value as a timed business Instant. Presence is never a zero.
func resolveWallTime(ctx context.Context, value LocalDateTime, zone Zone, initial bool) (time.Time, bool, error) {
	candidates, gap, gapFound, err := wallTimeCandidates(ctx, value, zone)
	if err != nil {
		return time.Time{}, false, err
	}
	if len(candidates) > 0 {
		return candidates[0], true, nil
	}
	if initial && gapFound {
		return gap, true, nil
	}
	return time.Time{}, false, nil
}

// LocalTimeCandidate is an exact server-side projection using the pinned data.
// An empty set is a gap; callers must explicitly select among multiple values.
type LocalTimeCandidate struct {
	instant Instant
	offset  int
}

func (c LocalTimeCandidate) Instant() Instant   { return c.instant }
func (c LocalTimeCandidate) OffsetSeconds() int { return c.offset }
func ResolveLocalTime(ctx context.Context, value LocalDateTime, zone Zone) ([]LocalTimeCandidate, error) {
	raw, _, _, err := wallTimeCandidates(ctx, value, zone)
	if err != nil {
		return nil, err
	}
	if len(raw) > 2 {
		return nil, failure(InvalidZone)
	}
	candidates := make([]LocalTimeCandidate, 0, len(raw))
	for _, at := range raw {
		instant, err := NewInstant(at)
		if err != nil {
			return nil, err
		}
		offset := int(value.wallUTC().Sub(at) / time.Second)
		candidates = append(candidates, LocalTimeCandidate{instant: instant, offset: offset})
	}
	return candidates, nil
}

func wallTimeCandidates(ctx context.Context, value LocalDateTime, zone Zone) ([]time.Time, time.Time, bool, error) {
	if err := contextValid(ctx); err != nil {
		return nil, time.Time{}, false, err
	}
	if !value.Valid() || !zone.Valid() {
		return nil, time.Time{}, false, failure(InvalidDate)
	}
	wall := value.wallUTC()
	low, high := wall.Add(-26*time.Hour), wall.Add(26*time.Hour)
	var offsets [16]int
	offsetCount := 0
	current := low
	var gap time.Time
	gapFound := false
	for transitions := 0; ; transitions++ {
		if err := canceled(ctx); err != nil {
			return nil, time.Time{}, false, err
		}
		if transitions >= 16 {
			return nil, time.Time{}, false, failure(BudgetExceeded)
		}
		offset, err := zone.offsetAt(ctx, current)
		if err != nil {
			return nil, time.Time{}, false, err
		}
		if offset < -26*3600 || offset > 26*3600 {
			return nil, time.Time{}, false, failure(InvalidZone)
		}
		seen := false
		for _, existing := range offsets[:offsetCount] {
			if existing == offset {
				seen = true
				break
			}
		}
		if !seen {
			offsets[offsetCount] = offset
			offsetCount++
		}
		boundary, err := zone.boundsAt(ctx, current)
		if err != nil {
			return nil, time.Time{}, false, err
		}
		end := boundary.at
		if !boundary.present || end.After(high) {
			break
		}
		if !end.After(current) {
			return nil, time.Time{}, false, failure(InvalidZone)
		}
		after, err := zone.offsetAt(ctx, end)
		if err != nil {
			return nil, time.Time{}, false, err
		}
		if after > offset && !wall.Before(end.Add(time.Duration(offset)*time.Second)) && wall.Before(end.Add(time.Duration(after)*time.Second)) {
			gap = wall.Add(-time.Duration(offset) * time.Second)
			gapFound = true
		}
		current = end
	}
	var candidates []time.Time
	for _, offset := range offsets[:offsetCount] {
		instant := wall.Add(-time.Duration(offset) * time.Second)
		local, err := zone.localAt(ctx, instant)
		if err != nil {
			return nil, time.Time{}, false, err
		}
		if local == value {
			candidates = append(candidates, instant)
		}
	}
	slices.SortFunc(candidates, func(a, b time.Time) int { return a.Compare(b) })
	return candidates, gap, gapFound, nil
}

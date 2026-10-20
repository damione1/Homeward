package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrHistoryInvalid           = errors.New("execution history: invalid input")
	ErrHistoryIntegrity         = errors.New("execution history: integrity failure")
	ErrHistoryUnavailable       = errors.New("execution history: unavailable")
	ErrHistoryCommitUnknown     = errors.New("execution history: commit outcome unknown")
	ErrHistoryOutsideRetention  = errors.New("execution history: outside retention")
	ErrHistoryRetentionAdvanced = errors.New("execution history: retention advanced")
	ErrHistoryTruncated         = errors.New("execution history: page truncated")
)

type HistoryEpoch struct{ bytes [16]byte }

func ParseHistoryEpoch(text string) (HistoryEpoch, error) {
	if len(text) != 32 || text != strings.ToLower(text) {
		return HistoryEpoch{}, ErrHistoryInvalid
	}
	b, err := hex.DecodeString(text)
	if err != nil {
		return HistoryEpoch{}, ErrHistoryInvalid
	}
	var value HistoryEpoch
	copy(value.bytes[:], b)
	if !value.Valid() {
		return HistoryEpoch{}, ErrHistoryInvalid
	}
	return value, nil
}

func (e HistoryEpoch) Valid() bool { return e.bytes != [16]byte{} }
func (e HistoryEpoch) String() string {
	if !e.Valid() {
		return ""
	}
	return hex.EncodeToString(e.bytes[:])
}

type HistoryTraceID struct {
	epoch   HistoryEpoch
	ordinal uint64
}

func NewHistoryTraceID(epoch HistoryEpoch, ordinal uint64) (HistoryTraceID, error) {
	if !epoch.Valid() || ordinal == 0 || ordinal > math.MaxInt64 {
		return HistoryTraceID{}, ErrHistoryInvalid
	}
	return HistoryTraceID{epoch: epoch, ordinal: ordinal}, nil
}
func (id HistoryTraceID) Epoch() HistoryEpoch { return id.epoch }
func (id HistoryTraceID) Ordinal() uint64     { return id.ordinal }
func (id HistoryTraceID) Valid() bool {
	return id.epoch.Valid() && id.ordinal > 0 && id.ordinal <= math.MaxInt64
}

// These numeric sets are the explicit Go admission of migration 20's CHECKs.
// Each producer's native enum is converted once at the executionhistory edge.
type HistoryRecordKind uint8

const (
	HistoryKindInvalid HistoryRecordKind = iota
	HistoryDecision
	HistoryNode
	HistoryCommand
	HistorySceneMember
	HistorySceneInvocation
	HistoryScriptLink
)

type HistoryProvenanceKind uint8

const (
	HistoryProvenanceInvalid HistoryProvenanceKind = iota
	HistoryUnboundFixture
	HistoryBoundProgram
)

type HistoryLossReason uint8

const (
	HistoryLossInvalid HistoryLossReason = iota
	HistoryLossQueueFull
	HistoryLossTooLarge
	HistoryLossContention
	HistoryLossInvalidMetadata
)

// HistoryRecordInput contains only enumerated outcomes and whitelisted source
// metadata. Callers cannot pass a payload, cause, arguments or free-form error.
type HistoryRecordInput struct {
	ID                                                                            HistoryTraceID
	EmissionTime                                                                  time.Time
	Kind                                                                          HistoryRecordKind
	Decision, NodeStage, CommandOutcome, SceneOutcome, SceneAggregate, ScriptMode uint8
	NativeOrdinal, NodeVisit, CommandOrdinal                                      uint64
	ActionKind, FailureKind, WaitExit                                             uint8
	ControllerGeneration, RunID, ParentRunID                                      uint64
	Provenance                                                                    HistoryProvenanceKind
	ConfigRevision                                                                int64
	ManifestSHA256                                                                [sha256.Size]byte
	ObjectID, SourceFile, SourcePath                                              string
	SourceLine, SourceColumn                                                      int
	TriggerKey, CommandID, SceneInvocation                                        string
	SceneMember                                                                   int
	Branches, Iterations                                                          []int32
}

type HistoryRecord struct {
	input  HistoryRecordInput
	digest [sha256.Size]byte
	charge int
}

func NewHistoryRecord(in HistoryRecordInput) (HistoryRecord, error) {
	if !in.ID.Valid() || in.EmissionTime.IsZero() || in.EmissionTime.Year() < 2000 || in.EmissionTime.Year() > 9999 ||
		!validHistoryOutcome(in) || !validHistoryProvenance(in) || !validHistoryMetadata(in) {
		return HistoryRecord{}, ErrHistoryInvalid
	}
	// SQL stores microsecond precision. Hash the exact value SQL will receive.
	in.EmissionTime = in.EmissionTime.UTC().Truncate(time.Microsecond)
	in.Branches = append([]int32(nil), in.Branches...)
	in.Iterations = append([]int32(nil), in.Iterations...)
	in.ObjectID = strings.Clone(in.ObjectID)
	in.SourceFile = strings.Clone(in.SourceFile)
	in.SourcePath = strings.Clone(in.SourcePath)
	in.TriggerKey = strings.Clone(in.TriggerKey)
	in.CommandID = strings.Clone(in.CommandID)
	in.SceneInvocation = strings.Clone(in.SceneInvocation)
	encoded, err := json.Marshal(struct {
		Epoch   string
		Ordinal uint64
		Value   HistoryRecordInput
	}{in.ID.Epoch().String(), in.ID.Ordinal(), in})
	charge := 512 + 2*len(encoded)
	if err != nil || len(encoded) > 4<<10 || charge > 8<<10 {
		return HistoryRecord{}, ErrHistoryInvalid
	}
	return HistoryRecord{input: in, digest: sha256.Sum256(encoded), charge: charge}, nil
}

func validHistoryOutcome(in HistoryRecordInput) bool {
	switch in.Kind {
	case HistoryDecision:
		return in.Decision >= 1 && in.Decision <= 18 && in.NodeStage == 0 && in.CommandOutcome == 0 && in.SceneOutcome == 0 && in.SceneAggregate == 0 && in.ScriptMode == 0
	case HistoryNode:
		return in.Decision == 0 && in.NodeStage >= 1 && in.NodeStage <= 6 && in.CommandOutcome == 0 && in.SceneOutcome == 0 && in.SceneAggregate == 0 && in.ScriptMode == 0
	case HistoryCommand:
		return in.Decision == 0 && in.NodeStage == 0 && in.CommandOutcome >= 1 && in.CommandOutcome <= 7 && in.SceneOutcome == 0 && in.SceneAggregate == 0 && in.ScriptMode == 0
	case HistorySceneMember:
		return in.Decision == 0 && in.NodeStage == 0 && in.CommandOutcome == 0 && in.SceneOutcome >= 1 && in.SceneOutcome <= 8 && in.SceneAggregate == 0 && in.ScriptMode == 0
	case HistorySceneInvocation:
		return in.Decision == 0 && in.NodeStage == 0 && in.CommandOutcome == 0 && in.SceneOutcome == 0 && in.SceneAggregate >= 1 && in.SceneAggregate <= 5 && in.ScriptMode == 0
	case HistoryScriptLink:
		return in.Decision == 0 && in.NodeStage == 0 && in.CommandOutcome == 0 && in.SceneOutcome == 0 && in.SceneAggregate == 0 && in.ScriptMode >= 1 && in.ScriptMode <= 2
	default:
		return false
	}
}

func validHistoryProvenance(in HistoryRecordInput) bool {
	switch in.Provenance {
	case HistoryUnboundFixture:
		return in.ConfigRevision == 0 && in.ManifestSHA256 == [sha256.Size]byte{}
	case HistoryBoundProgram:
		return in.ConfigRevision > 0 && in.ManifestSHA256 != [sha256.Size]byte{}
	default:
		return false
	}
}

func validHistoryMetadata(in HistoryRecordInput) bool {
	if in.ControllerGeneration > math.MaxInt64 || in.RunID > math.MaxInt64 || in.ParentRunID > math.MaxInt64 ||
		in.NativeOrdinal > math.MaxInt64 || in.NodeVisit > math.MaxInt64 || in.CommandOrdinal > math.MaxInt64 ||
		in.ActionKind > 11 || in.FailureKind > 7 || in.WaitExit > 5 ||
		(in.Kind == HistoryNode && (in.NodeVisit == 0 || in.ActionKind == 0)) ||
		(in.RunID == 0 && (in.ControllerGeneration != 0 || in.ParentRunID != 0)) ||
		(in.RunID != 0 && in.ControllerGeneration == 0) || in.ParentRunID == in.RunID && in.RunID != 0 ||
		len(in.ObjectID) > 256 || len(in.SourceFile) > 512 || len(in.SourcePath) > 1024 ||
		len(in.TriggerKey) > 256 || len(in.CommandID) > 256 || len(in.SceneInvocation) > 256 ||
		!utf8.ValidString(in.ObjectID) || !utf8.ValidString(in.SourceFile) || !utf8.ValidString(in.SourcePath) ||
		!utf8.ValidString(in.TriggerKey) || !utf8.ValidString(in.CommandID) || !utf8.ValidString(in.SceneInvocation) ||
		strings.ContainsAny(in.ObjectID+in.SourceFile+in.SourcePath+in.TriggerKey+in.CommandID+in.SceneInvocation, "\x00\r\n") ||
		in.SourceLine < 0 || in.SourceColumn < 0 || in.SceneMember < 0 || in.SourceLine > math.MaxInt32 || in.SourceColumn > math.MaxInt32 || in.SceneMember > math.MaxInt32 || len(in.Branches) > 8 || len(in.Iterations) > 8 {
		return false
	}
	if in.SourceFile != "" && (path.IsAbs(in.SourceFile) || path.Clean(in.SourceFile) == ".." || strings.HasPrefix(path.Clean(in.SourceFile), "../")) {
		return false
	}
	for _, part := range in.Branches {
		if part < 0 {
			return false
		}
	}
	for _, part := range in.Iterations {
		if part < 1 {
			return false
		}
	}
	return true
}

func (r HistoryRecord) ID() HistoryTraceID        { return r.input.ID }
func (r HistoryRecord) Digest() [sha256.Size]byte { return r.digest }
func (r HistoryRecord) RetainedBytes() int        { return r.charge }
func (r HistoryRecord) Input() HistoryRecordInput {
	in := r.input
	in.Branches = append([]int32(nil), in.Branches...)
	in.Iterations = append([]int32(nil), in.Iterations...)
	return in
}
func (r HistoryRecord) Valid() bool {
	return r.input.ID.Valid() && r.digest != [sha256.Size]byte{} && r.charge > 0 && r.charge <= 8<<10
}

type HistoryLoss struct {
	Reason            HistoryLossReason
	KnownCount        uint64
	From, Through     time.Time
	UncertainEnvelope bool
}

func (l HistoryLoss) valid() bool {
	return l.Reason >= HistoryLossQueueFull && l.Reason <= HistoryLossInvalidMetadata &&
		l.KnownCount > 0 && l.KnownCount <= math.MaxInt64 && !l.From.IsZero() && !l.Through.IsZero() && l.From.Year() >= 2000 && l.Through.Year() <= 9999 && !l.Through.Before(l.From)
}

type HistoryBatch struct {
	epoch   HistoryEpoch
	number  uint64
	records []HistoryRecord
	losses  []HistoryLoss
	digest  [sha256.Size]byte
}

func NewHistoryBatch(epoch HistoryEpoch, number uint64, records []HistoryRecord, losses []HistoryLoss) (HistoryBatch, error) {
	if !epoch.Valid() || number == 0 || number > math.MaxInt64 || len(records)+len(losses) == 0 || len(records) > 128 || len(losses) > 4 {
		return HistoryBatch{}, ErrHistoryInvalid
	}
	owned := HistoryBatch{epoch: epoch, number: number, records: make([]HistoryRecord, len(records)), losses: make([]HistoryLoss, len(losses))}
	var previous uint64
	retained := 0
	for i, record := range records {
		if !record.Valid() || record.ID().Epoch() != epoch || record.ID().Ordinal() <= previous {
			return HistoryBatch{}, ErrHistoryInvalid
		}
		retained += record.RetainedBytes()
		if retained > 512<<10 {
			return HistoryBatch{}, ErrHistoryInvalid
		}
		previous = record.ID().Ordinal()
		owned.records[i] = record
		owned.records[i].input = record.Input()
	}
	seen := [5]bool{}
	for i, loss := range losses {
		if !loss.valid() || seen[loss.Reason] {
			return HistoryBatch{}, ErrHistoryInvalid
		}
		seen[loss.Reason] = true
		loss.From = loss.From.UTC().Truncate(time.Microsecond)
		loss.Through = loss.Through.UTC().Truncate(time.Microsecond)
		owned.losses[i] = loss
	}
	encoded, err := json.Marshal(struct {
		Epoch   string
		Number  uint64
		Records []historyDigestItem
		Losses  []HistoryLoss
	}{epoch.String(), number, batchDigests(owned.records), owned.losses})
	if err != nil || len(encoded) > 512<<10 {
		return HistoryBatch{}, ErrHistoryInvalid
	}
	owned.digest = sha256.Sum256(encoded)
	return owned, nil
}

type historyDigestItem struct {
	Ordinal uint64
	Digest  [sha256.Size]byte
}

func batchDigests(records []HistoryRecord) []historyDigestItem {
	values := make([]historyDigestItem, len(records))
	for i, record := range records {
		values[i] = historyDigestItem{record.ID().Ordinal(), record.Digest()}
	}
	return values
}

func (b HistoryBatch) Valid() bool {
	return b.epoch.Valid() && b.number > 0 && b.digest != [sha256.Size]byte{}
}
func (b HistoryBatch) Epoch() HistoryEpoch       { return b.epoch }
func (b HistoryBatch) Number() uint64            { return b.number }
func (b HistoryBatch) Digest() [sha256.Size]byte { return b.digest }
func (b HistoryBatch) LastOrdinal() uint64 {
	if len(b.records) == 0 {
		return 0
	}
	return b.records[len(b.records)-1].ID().Ordinal()
}

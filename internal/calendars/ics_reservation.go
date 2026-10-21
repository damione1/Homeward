package calendars

import (
	"bytes"
	"context"
	"sync"
)

const parseReservationLimit uint64 = 48 << 20
const parseFixedCredit uint64 = 8 << 20

// Admission counters observe work inside the private operation scope.
type icsAdmissionObservation struct {
	ReservedBytes              uint64
	RequestedBytes             uint64
	ZoneCopyReservedBytes      uint64
	DecodeStagingReservedBytes uint64
	MaskReservedBytes          uint64
	RuleConstructors           uint64
	ParserCalls                uint64
	NextCalls                  uint64
	ZoneCompilations           uint64
	RawObservances             uint64
	UniqueObservances          uint64
	TransitionAppends          uint64
	Work                       int64
	Candidates                 int64
}
type admissionKey struct{}
type admissionLeaseState uint8

const (
	leaseInvalid admissionLeaseState = iota
	leaseOwned
	leaseClosed
)

type admissionSession struct {
	state       admissionLeaseState
	reserved    uint64
	observation icsAdmissionObservation
	image       *imageLease
}

var parses struct {
	sync.Mutex
	active bool
}

func beginAdmission() (*admissionSession, error) {
	parses.Lock()
	defer parses.Unlock()
	if parses.active {
		return nil, failure(Busy)
	}
	parses.active = true
	return &admissionSession{state: leaseOwned, reserved: parseFixedCredit, observation: icsAdmissionObservation{ReservedBytes: parseFixedCredit}}, nil
}
func (a *admissionSession) close() {
	parses.Lock()
	defer parses.Unlock()
	if a != nil && a.state == leaseOwned {
		a.state = leaseClosed
		parses.active = false
	}
}
func admissionFrom(ctx context.Context) *admissionSession {
	a, _ := ctx.Value(admissionKey{}).(*admissionSession)
	return a
}
func (a *admissionSession) reserveTo(total uint64) error {
	if a == nil || a.state != leaseOwned {
		return failure(InvalidInput)
	}
	if total > a.observation.RequestedBytes {
		a.observation.RequestedBytes = total
	}
	if total > parseReservationLimit {
		return failure(CapacityExceeded)
	}
	if total > a.reserved {
		a.reserved = total
		a.observation.ReservedBytes = total
	}
	return nil
}
func (a *admissionSession) reserveMasks(bytes uint64) error {
	if a == nil || a.state != leaseOwned {
		return failure(InvalidInput)
	}
	if bytes > parseReservationLimit-a.reserved {
		a.observation.RequestedBytes = a.reserved + bytes
		return failure(CapacityExceeded)
	}
	if err := a.reserveTo(a.reserved + bytes); err != nil {
		return err
	}
	a.observation.MaskReservedBytes += bytes
	return nil
}
func (a *admissionSession) reserveCopies(bytes uint64) error {
	if a == nil || a.state != leaseOwned {
		return failure(InvalidInput)
	}
	if bytes > parseReservationLimit-a.reserved {
		a.observation.RequestedBytes = a.reserved + bytes
		return failure(CapacityExceeded)
	}
	if a.image != nil {
		if err := a.image.grow(int64(bytes)); err != nil {
			return err
		}
	}
	if err := a.reserveTo(a.reserved + bytes); err != nil {
		return err
	}
	a.observation.ZoneCopyReservedBytes += bytes
	return nil
}

// Codec buffers add cumulative work credit without pretending to be timezone
// copies. The image owner separately reserves their retained/SQL copies.
func (a *admissionSession) reserveData(bytes uint64) error {
	if a == nil || a.state != leaseOwned {
		return failure(InvalidInput)
	}
	if bytes > parseReservationLimit-a.reserved {
		a.observation.RequestedBytes = a.reserved + bytes
		return failure(CapacityExceeded)
	}
	return a.reserveTo(a.reserved + bytes)
}

// Nested domain operations reuse the working reserve of their caller.
func admissionScope(ctx context.Context) (context.Context, func(), error) {
	if err := contextValid(ctx); err != nil {
		return ctx, nil, err
	}
	if a := admissionFrom(ctx); a != nil {
		if a.state != leaseOwned {
			return ctx, nil, failure(InvalidInput)
		}
		return ctx, func() {}, nil
	}
	owner, err := beginAdmission()
	if err != nil {
		return ctx, nil, err
	}
	return context.WithValue(ctx, admissionKey{}, owner), owner.close, nil
}

type parseCharge struct{ Bytes, LogicalLines, Components, Zones, ParameterKeys, ParameterValues, ParameterScratch, Reserved uint64 }

func costTokenByte(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-'
}
func costTokenEnd(line []byte, start int) (int, bool) {
	p := start
	for p < len(line) && !costTokenByte(line[p]) {
		p++
	}
	if p == len(line) {
		return p, false
	}
	for p < len(line) && costTokenByte(line[p]) {
		p++
	}
	return p, true
}

// Allocation-control scanner, not an ICS parser. It follows the exact v0.3.8
// parameter call sites and never returns a syntax acceptance/rejection. The
// upstream name regexp is NOT anchored: starting blindly at the first colon
// would underprice an input the candidate parser can still consume.
func parameterCost(line []byte) (keys, values uint64) {
	p, found := costTokenEnd(line, 0)
	if !found {
		return 0, 0
	}
	for p < len(line) {
		if line[p] == ':' {
			return keys, values
		}
		if line[p] != ';' {
			return keys, values
		}
		p++
		p, found = costTokenEnd(line, p)
		if !found {
			return keys, values
		}
		keys++
		if p >= len(line) || line[p] != '=' {
			return keys, values
		}
		p++
		for {
			values++ // Entry into parsePropertyParamValue allocates len(line).
			start := p
			quoted := false
			done := false
			for p < len(line) && !done {
				c := line[p]
				if c == '\\' {
					if p+2 >= len(line) {
						return keys, values
					}
					p += 2
					continue
				}
				if c == '"' {
					if p == start {
						quoted = true
						p++
						continue
					}
					if !quoted {
						return keys, values
					}
					p++
					done = true
					continue
				}
				if !quoted && (c == ';' || c == ':' || c == ',') {
					done = true
					continue
				}
				// The candidate rejects controls without entering another value.
				if c < 0x20 && c != '\t' {
					return keys, values
				}
				p++
			}
			if p >= len(line) {
				return keys, values
			}
			if line[p] != ',' {
				break
			}
			p++
		}
	}
	return keys, values
}

// The8MiB fixed credit is acquired before guard's bounded normalization copy.
// Remaining credit is calculated without constructing an AST, parameter map,
// or event. A reservation rejection is Capacity, never UnsupportedSyntax.
func estimateNormalized(normalized []byte, stats guardStats) parseCharge {
	c := parseCharge{Bytes: uint64(len(normalized)), LogicalLines: uint64(stats.LogicalLines), Components: uint64(stats.Components), Zones: uint64(stats.Zones)}
	for rest := normalized; len(rest) != 0; {
		i := bytes.IndexByte(rest, '\n')
		var line []byte
		if i < 0 {
			line = rest
			rest = nil
		} else {
			line = rest[:i]
			rest = rest[i+1:]
		}
		keys, values := parameterCost(line)
		c.ParameterKeys += keys
		c.ParameterValues += values
		// Full-line []byte scratch, regex conversion, returned strings and
		// allocator slack. The quadratic driver is explicit, not hidden in B.
		c.ParameterScratch += 4 * uint64(len(line)) * values
	}
	c.Reserved = (8 << 20) + 16*c.Bytes + 512*c.LogicalLines + 1024*c.Components +
		c.ParameterScratch + 512*c.ParameterKeys + 128*c.ParameterValues +
		c.Zones*((3*64<<10)+(1<<20))
	return c
}

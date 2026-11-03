package androidtv

import (
	"bufio"
	"encoding/binary"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protowire"
)

// Wire fields and handshake follow androidtvremote2 f49cf04703af1565e6216f1fc095b7d0477ca4a4,
// src/androidtvremote2/{remote.py,base.py,remotemessage.proto}. We intentionally
// negotiate no KEY, VOICE or APP_LINK feature and never emit their messages.
const maxFrame = 64 * 1024
const (
	featurePing   = 1
	featureIME    = 4
	featurePower  = 32
	featureVolume = 64
)

type field struct {
	number protowire.Number
	kind   protowire.Type
	value  uint64
	data   []byte
}

func fields(b []byte) ([]field, error) {
	var out []field
	seen := map[protowire.Number]bool{}
	for len(b) > 0 {
		n, k, l := protowire.ConsumeTag(b)
		if l < 0 || n < 1 {
			return nil, ErrProtocol
		}
		// All fields consumed by this read-only subset are singular. Reject
		// ambiguity rather than choosing a different value from another client.
		if seen[n] {
			return nil, ErrProtocol
		}
		seen[n] = true
		b = b[l:]
		f := field{number: n, kind: k}
		var count int
		switch k {
		case protowire.VarintType:
			f.value, count = protowire.ConsumeVarint(b)
		case protowire.BytesType:
			f.data, count = protowire.ConsumeBytes(b)
		default:
			count = protowire.ConsumeFieldValue(n, k, b)
		}
		if count < 0 {
			return nil, ErrProtocol
		}
		b = b[count:]
		out = append(out, f)
		if len(out) > 256 {
			return nil, ErrProtocol
		}
	}
	return out, nil
}
func typeMatches(fs []field, n protowire.Number, kind protowire.Type) bool {
	for _, f := range fs {
		if f.number == n && f.kind != kind {
			return false
		}
	}
	return true
}
func number(fs []field, n protowire.Number) (uint64, bool) {
	for _, f := range fs {
		if f.number == n && f.kind == protowire.VarintType {
			return f.value, true
		}
	}
	return 0, false
}
func payload(fs []field, n protowire.Number) ([]byte, bool) {
	for _, f := range fs {
		if f.number == n && f.kind == protowire.BytesType {
			return f.data, true
		}
	}
	return nil, false
}
func varint(n protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, n, protowire.VarintType), v)
}
func message(n protowire.Number, b []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), b)
}
func readFrame(r *bufio.Reader) ([]byte, error) {
	n, e := binary.ReadUvarint(r)
	if e != nil {
		return nil, e
	}
	if n == 0 || n > maxFrame {
		return nil, ErrProtocol
	}
	b := make([]byte, n)
	_, e = io.ReadFull(r, b)
	return b, e
}
func writeFrame(w io.Writer, b []byte) error {
	if len(b) == 0 || len(b) > maxFrame {
		return ErrProtocol
	}
	framed := binary.AppendUvarint(nil, uint64(len(b)))
	framed = append(framed, b...)
	for len(framed) > 0 {
		n, e := w.Write(framed)
		if e != nil {
			return e
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		framed = framed[n:]
	}
	return nil
}

// Observation deliberately has no playback, media title, duration or position:
// Remote v2 cannot prove them. Power remains unknown until remote_start.
type Observation struct {
	Power             *bool
	Source            *string
	VolumeBasisPoints *uint32
}
type protocol struct {
	features            uint64
	configured, started bool
	observation         Observation
}

func newProtocol(ime bool) *protocol {
	f := uint64(featurePing | featurePower | featureVolume)
	if ime {
		f |= featureIME
	}
	return &protocol{features: f}
}
func (p *protocol) handle(b []byte) (reply []byte, changed bool, err error) {
	fs, e := fields(b)
	if e != nil {
		return nil, false, e
	}
	// As in the upstream dispatcher, one response is produced per envelope.
	if cfg, ok := payload(fs, 1); ok {
		if p.configured {
			return nil, false, ErrProtocol
		}
		inner, e := fields(cfg)
		if e != nil {
			return nil, false, e
		}
		supported, ok := number(inner, 1)
		if !ok {
			return nil, false, ErrProtocol
		}
		p.features &= supported
		p.configured = true
		info := append(varint(3, 1), message(4, []byte("1"))...)
		info = append(info, message(5, []byte("atvremote"))...)
		info = append(info, message(6, []byte("1.0.0"))...)
		return message(1, append(varint(1, p.features), message(2, info)...)), false, nil
	}
	if !p.configured {
		return nil, false, ErrProtocol
	}
	if _, ok := payload(fs, 2); ok {
		return message(2, varint(1, p.features)), false, nil
	}
	if _, ok := payload(fs, 3); ok {
		return nil, false, ErrProtocol
	}
	if ping, ok := payload(fs, 8); ok {
		inner, e := fields(ping)
		if e != nil {
			return nil, false, e
		}
		v, _ := number(inner, 1)
		return message(9, varint(1, v)), false, nil
	}
	if start, ok := payload(fs, 40); ok {
		if p.features&featurePower == 0 {
			return nil, false, ErrProtocol
		}
		inner, e := fields(start)
		if e != nil || !typeMatches(inner, 1, protowire.VarintType) {
			return nil, false, ErrProtocol
		}
		v, _ := number(inner, 1)
		if v > 1 {
			return nil, false, ErrProtocol
		}
		on := v == 1
		p.observation.Power = &on
		p.started = true
		return nil, true, nil
	}
	if vol, ok := payload(fs, 50); ok && p.features&featureVolume != 0 {
		inner, e := fields(vol)
		if e != nil || !typeMatches(inner, 6, protowire.VarintType) || !typeMatches(inner, 7, protowire.VarintType) {
			return nil, false, ErrProtocol
		}
		max, _ := number(inner, 6)
		level, _ := number(inner, 7)
		p.observation.VolumeBasisPoints = nil
		if max > 0 && level <= max && max <= 1<<32-1 {
			v := uint32(level * 10000 / max)
			p.observation.VolumeBasisPoints = &v
		}
		return nil, true, nil
	}
	if ime, ok := payload(fs, 20); ok && p.features&featureIME != 0 {
		inner, e := fields(ime)
		if e != nil {
			return nil, false, e
		}
		app, ok := payload(inner, 1)
		if !ok {
			return nil, false, ErrProtocol
		}
		af, e := fields(app)
		if e != nil {
			return nil, false, e
		}
		name, _ := payload(af, 12)
		p.observation.Source = nil
		if len(name) > 0 && len(name) <= 256 && utf8.Valid(name) && strings.IndexFunc(string(name), unicode.IsControl) < 0 {
			s := string(name)
			p.observation.Source = &s
		}
		return nil, true, nil
	}
	return nil, false, nil
}

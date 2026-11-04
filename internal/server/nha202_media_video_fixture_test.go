package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

type nha202VideoSource struct {
	sps, pps           []byte
	width, height, fps uint32
	samples            [][]byte
}

// Trusted original synthetic fixture, bounded and hashed against its retained
// provenance. No request supplies paths, bytes, codec names or shell arguments.
func nha202LoadVideoSource(root string) (*nha202VideoSource, error) {
	file, err := os.Open(filepath.Join(root, "provenance.json"))
	if err != nil {
		return nil, err
	}
	var manifest struct {
		SHA256 string `json:"sha256"`
		Width  uint32 `json:"width"`
		Height uint32 `json:"height"`
		FPS    uint32 `json:"fps"`
		Frames int    `json:"frames"`
		Audio  bool   `json:"audio"`
	}
	decoder := json.NewDecoder(io.LimitReader(file, 8193))
	err = decoder.Decode(&manifest)
	extra := decoder.Decode(new(any))
	file.Close()
	if err != nil || extra != io.EOF || manifest.Audio || manifest.Width == 0 || manifest.Width > 1920 || manifest.Height == 0 || manifest.Height > 1080 || manifest.FPS == 0 || manifest.FPS > 30 || manifest.Frames < 10 || manifest.Frames > 120 {
		return nil, errors.New("invalid synthetic video manifest")
	}
	file, err = os.Open(filepath.Join(root, "synthetic.h264"))
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, 16*1024*1024+1))
	file.Close()
	if err != nil || len(raw) > 16*1024*1024 {
		return nil, errors.New("synthetic video exceeds byte limit")
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != manifest.SHA256 {
		return nil, errors.New("synthetic video source digest changed")
	}
	type start struct{ begin, payload int }
	starts := []start{}
	for offset := 0; offset+3 <= len(raw); {
		length := 0
		switch {
		case bytes.HasPrefix(raw[offset:], []byte{0, 0, 0, 1}):
			length = 4
		case bytes.HasPrefix(raw[offset:], []byte{0, 0, 1}):
			length = 3
		}
		if length == 0 {
			offset++
			continue
		}
		starts = append(starts, start{offset, offset + length})
		offset += length
		if len(starts) > 120*64 {
			return nil, errors.New("too many synthetic video NALs")
		}
	}
	source := &nha202VideoSource{width: manifest.Width, height: manifest.Height, fps: manifest.FPS}
	nals := 0
	for index, start := range starts {
		end := len(raw)
		if index+1 < len(starts) {
			end = starts[index+1].begin
		}
		nal := raw[start.payload:end]
		if len(nal) == 0 || nal[0]&0x80 != 0 {
			return nil, errors.New("invalid synthetic video NAL")
		}
		switch nal[0] & 31 {
		case 7:
			if len(nal) < 4 || len(nal) > 1024 || nal[1] != 66 || source.sps != nil && !bytes.Equal(source.sps, nal) {
				return nil, errors.New("invalid synthetic baseline SPS")
			}
			source.sps = bytes.Clone(nal)
			continue
		case 8:
			if len(nal) < 2 || len(nal) > 1024 || source.pps != nil && !bytes.Equal(source.pps, nal) {
				return nil, errors.New("invalid synthetic PPS")
			}
			source.pps = bytes.Clone(nal)
			continue
		case 9:
			if len(source.samples) >= 120 {
				return nil, errors.New("too many synthetic video samples")
			}
			source.samples = append(source.samples, nil)
			nals = 0
		case 1, 5, 6:
		default:
			return nil, errors.New("unexpected synthetic video NAL")
		}
		nals++
		if len(source.samples) == 0 || nals > 64 {
			return nil, errors.New("invalid synthetic video access unit")
		}
		sample := &source.samples[len(source.samples)-1]
		if len(nal)+len(*sample)+4 > 512*1024 {
			return nil, errors.New("synthetic video sample exceeds limit")
		}
		*sample = binary.BigEndian.AppendUint32(*sample, uint32(len(nal)))
		*sample = append(*sample, nal...)
	}
	if len(source.samples) != manifest.Frames || source.sps == nil || source.pps == nil {
		return nil, errors.New("incomplete synthetic video")
	}
	return source, nil
}

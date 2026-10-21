package storage

// GapSummary copies at most the admitted batch's 32 small metadata rows. No
// state payload, image, error or SQL is copied or scanned to count a commit.
func (b RecorderBatch) GapSummary() ([]RecorderGap, error) {
	if !b.epoch.valid() || len(b.gaps) > 32 {
		return nil, ErrRecorderInvalid
	}
	result := make([]RecorderGap, len(b.gaps))
	for i, gap := range b.gaps {
		if !gap.valid() {
			return nil, ErrRecorderInvalid
		}
		result[i] = gap
		if gap.KnownLost != nil {
			count := *gap.KnownLost
			result[i].KnownLost = &count
		}
	}
	return result, nil
}

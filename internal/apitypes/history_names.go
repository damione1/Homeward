package apitypes

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
)

// HistoryRunName and HistoryControllerName are read-only correlation names
// inside one persisted execution epoch. They allocate no registry or owner.
type HistoryRunName struct{ value string }
type HistoryControllerName struct{ value string }

func (v HistoryRunName) String() string        { return v.value }
func (v HistoryControllerName) String() string { return v.value }
func (v HistoryRunName) Valid() bool           { return v.value != "" }
func (v HistoryControllerName) Valid() bool    { return v.value != "" }
func ParseHistoryRunName(value string) (HistoryRunName, error) {
	if err := resourceName(value, "homes", "executionEpochs", "runs"); err != nil {
		return HistoryRunName{}, err
	}
	return HistoryRunName{value}, nil
}
func ParseHistoryControllerName(value string) (HistoryControllerName, error) {
	if err := resourceName(value, "homes", "executionEpochs", "controllers"); err != nil {
		return HistoryControllerName{}, err
	}
	return HistoryControllerName{value}, nil
}
func historyCorrelationName(home HomeName, epoch [16]byte, native uint64, collection string) (string, error) {
	if !home.Valid() || epoch == ([16]byte{}) || native == 0 {
		return "", ErrResourceName
	}
	epochHash := sha256.New()
	epochHash.Write([]byte("homeward-history-epoch-v2\x00"))
	epochHash.Write([]byte(home.String()))
	epochHash.Write([]byte{0})
	epochHash.Write(epoch[:])
	epochID := "e-" + hex.EncodeToString(epochHash.Sum(nil)[:24])
	identity := sha256.New()
	identity.Write([]byte("homeward-history-correlation-v2\x00" + collection + "\x00"))
	identity.Write([]byte(home.String()))
	identity.Write([]byte{0})
	identity.Write(epoch[:])
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], native)
	identity.Write(encoded[:])
	id := "r-" + hex.EncodeToString(identity.Sum(nil)[:24])
	if collection == "controllers" {
		id = "c-" + hex.EncodeToString(identity.Sum(nil)[:24])
	}
	return home.String() + "/executionEpochs/" + epochID + "/" + collection + "/" + id, nil
}
func HistoryRunNameForIdentity(home HomeName, epoch [16]byte, run uint64) (HistoryRunName, error) {
	value, err := historyCorrelationName(home, epoch, run, "runs")
	if err != nil {
		return HistoryRunName{}, err
	}
	return ParseHistoryRunName(value)
}
func HistoryControllerNameForIdentity(home HomeName, epoch [16]byte, controller uint64) (HistoryControllerName, error) {
	value, err := historyCorrelationName(home, epoch, controller, "controllers")
	if err != nil {
		return HistoryControllerName{}, err
	}
	return ParseHistoryControllerName(value)
}

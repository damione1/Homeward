// Package observability contains bounded values and cells only. It owns no
// goroutine, dispatcher, SQL connection, network client, or producer callback.
package observability

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"

	obs "github.com/damione1/Homeward/gen/home/observability/v1"
)

type ProcessObservationEpoch struct{ value [16]byte }

func NewProcessObservationEpoch() (ProcessObservationEpoch, error) {
	var epoch ProcessObservationEpoch
	if _, err := rand.Read(epoch.value[:]); err != nil || !epoch.valid() {
		return ProcessObservationEpoch{}, failure(obs.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_UNAVAILABLE, obs.ObservabilityField_OBSERVABILITY_FIELD_PROCESS_EPOCH)
	}
	return epoch, nil
}
func (e ProcessObservationEpoch) valid() bool { return e.value != [16]byte{} }
func (e ProcessObservationEpoch) Wire() (*obs.ProcessObservationEpoch, error) {
	if !e.valid() {
		return nil, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_PROCESS_EPOCH)
	}
	return &obs.ProcessObservationEpoch{Value: append([]byte(nil), e.value[:]...)}, nil
}
func (e ProcessObservationEpoch) Hex() (string, error) {
	if !e.valid() {
		return "", invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_PROCESS_EPOCH)
	}
	return hex.EncodeToString(e.value[:]), nil
}

type ReadBinding struct {
	revision uint64
	digest   [32]byte
}

func (b ReadBinding) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("observability: read binding"))
}

// These are authorization.Current.Binding's successful revision and digest
// values, after its separate error has been checked. A binding neither stores
// an account policy nor grants a permission by itself.
func NewReadBinding(revision uint64, digest [32]byte) (ReadBinding, error) {
	if digest == [32]byte{} {
		return ReadBinding{}, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_BINDING)
	}
	return ReadBinding{revision: revision, digest: digest}, nil
}
func (b ReadBinding) valid() bool                  { return b.digest != [32]byte{} }
func (b ReadBinding) Equal(other ReadBinding) bool { return b.valid() && other.valid() && b == other }

type ControllerHandle struct{ value [32]byte }

func (h ControllerHandle) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("observability: controller handle"))
}
func NewControllerHandle(epoch ProcessObservationEpoch) (ControllerHandle, error) {
	if !epoch.valid() {
		return ControllerHandle{}, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_PROCESS_EPOCH)
	}
	var handle ControllerHandle
	copy(handle.value[:16], epoch.value[:])
	if _, err := rand.Read(handle.value[16:]); err != nil || !handle.valid() {
		return ControllerHandle{}, failure(obs.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_UNAVAILABLE, obs.ObservabilityField_OBSERVABILITY_FIELD_CONTROLLER_HANDLE)
	}
	return handle, nil
}
func ParseControllerHandle(value *obs.ControllerHandle) (ControllerHandle, error) {
	if value == nil || len(value.Value) != 32 {
		return ControllerHandle{}, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_CONTROLLER_HANDLE)
	}
	var handle ControllerHandle
	copy(handle.value[:], value.Value)
	if !handle.valid() || len(value.ProtoReflect().GetUnknown()) != 0 {
		return ControllerHandle{}, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_CONTROLLER_HANDLE)
	}
	return handle, nil
}
func (h ControllerHandle) valid() bool {
	var epoch, owner [16]byte
	copy(epoch[:], h.value[:16])
	copy(owner[:], h.value[16:])
	return epoch != [16]byte{} && owner != [16]byte{}
}
func (h ControllerHandle) in(epoch ProcessObservationEpoch) bool {
	return h.valid() && epoch.valid() && bytes.Equal(h.value[:16], epoch.value[:])
}
func (h ControllerHandle) Equal(other ControllerHandle) bool {
	return h.valid() && other.valid() && h == other
}
func (h ControllerHandle) Wire() (*obs.ControllerHandle, error) {
	if !h.valid() {
		return nil, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_CONTROLLER_HANDLE)
	}
	return &obs.ControllerHandle{Value: append([]byte(nil), h.value[:]...)}, nil
}

type PageSize struct{ value uint32 }

func NewPageSize(value uint32) (PageSize, error) {
	if value == 0 || value > 100 {
		return PageSize{}, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_PAGE_SIZE)
	}
	return PageSize{value}, nil
}
func PageSizeFromOptional(value *uint32) (PageSize, error) {
	if value == nil {
		return NewPageSize(20)
	}
	return NewPageSize(*value)
}
func (s PageSize) Value() (uint32, error) {
	if s.value == 0 || s.value > 100 {
		return 0, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_PAGE_SIZE)
	}
	return s.value, nil
}

type ActiveRevision struct{ value uint64 }

func NewActiveRevision(value uint64) (ActiveRevision, error) {
	if value == 0 || value > math.MaxInt64 {
		return ActiveRevision{}, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_REVISION)
	}
	return ActiveRevision{value}, nil
}

func (r ActiveRevision) Wire() (*obs.ActiveRevision, error) {
	if r.value == 0 || r.value > math.MaxInt64 {
		return nil, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_REVISION)
	}
	return &obs.ActiveRevision{Value: r.value}, nil
}

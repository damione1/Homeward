package automationruns

import (
	"sync"
	"unsafe"
)

type nativeObservationPhase uint8

const (
	nativeObservationInvalid nativeObservationPhase = iota
	nativeObservationHeld
	nativeObservationReleased
)

// NativeObservation explicitly owns one existing native program image. Copies
// share the private state: copying a handle cannot duplicate its release.
// Execution joins do not wait for this observation's separate storage lifetime.
type NativeObservation struct {
	state *nativeObservationState
}

type nativeObservationState struct {
	mu         sync.Mutex
	controller *Controller
	image      *programImage
	phase      nativeObservationPhase
}

const nativeObservationCharge = int(unsafe.Sizeof(nativeObservationState{}))

// RetainNativeObservation reserves its header before allocation and retains the
// exact active interpreter image. Existing native getters remain the only data
// readers; the image and its ring keep their original charge until final release.
func (c *Controller) RetainNativeObservation(i *Interpreter) (NativeObservation, error) {
	if c == nil || i == nil {
		return NativeObservation{}, ErrInvalidPlan
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.closeStarted {
		return NativeObservation{}, ErrClosed
	}
	image := c.active
	if image == nil || image.interpreter != i || image.refs <= 0 {
		return NativeObservation{}, ErrInvalidPlan
	}
	if !c.pool.adjust(0, nativeObservationCharge) {
		return NativeObservation{}, ErrCapacity
	}
	if !c.retainImageLocked(image) {
		c.pool.adjust(nativeObservationCharge, 0)
		return NativeObservation{}, ErrInvalidPlan
	}
	return NativeObservation{state: &nativeObservationState{controller: c, image: image, phase: nativeObservationHeld}}, nil
}

// Release consumes this shared owner exactly once, including through aliases.
// It returns only after the last-image cleanup and its own charge restitution.
// A failed or abandoned capture may release before the execution owner closes.
func (h NativeObservation) Release() error {
	state := h.state
	if state == nil {
		return ErrInvalidPlan
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.phase != nativeObservationHeld || state.controller == nil || state.image == nil {
		return ErrInvalidPlan
	}
	c, image := state.controller, state.image
	c.mu.Lock()
	if image.refs <= 0 {
		c.mu.Unlock()
		return ErrInvalidPlan
	}
	finished := c.releaseImageLocked(image)
	state.phase = nativeObservationReleased
	state.controller, state.image = nil, nil
	c.mu.Unlock()
	// Never hold Controller.mu across the interpreter/040 release callbacks.
	finishImageRelease(c, finished)
	if !c.pool.adjust(nativeObservationCharge, 0) {
		return ErrInvalidPlan
	}
	return nil
}

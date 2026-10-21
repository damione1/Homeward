package automationruns

import (
	"context"

	"github.com/damione1/Homeward/internal/automationtriggers"
)

// programImage is the immutable causal program selected at admission. Its
// reference count is owned by Controller.mu: the active image, a preparation,
// a queued/replacement run and an executing run each own one reference.
type programImage struct {
	plan            Plan
	preparer        Preparer
	executor        Executor
	interpreter     *Interpreter
	generation      Generation
	triggerIdentity *automationtriggers.PlanIdentity
	charge          int
	refs            int
	lifecycle       programImageLifecycle
	lanes           map[laneKey]*lane
	ownKeys         map[laneKey]bool
}

type programImageLifecycle uint8

const (
	programImageInvalid programImageLifecycle = iota
	programImageCandidate
	programImagePublished
)

// ImageReservation is a one-use, controller-owned replacement. Admission and
// compilation precede SQL commit; the activation owner publishes it only
// after the committed generation is known and coherent.
type ImageReservation struct {
	controller *Controller
	image      *programImage
	state      imageReservationState
	admission  *imageAdmission
}

type imageReservationState uint8

const (
	imageReservationInvalid imageReservationState = iota
	imageReservationBuilding
	imageReservationPrepared
	imageReservationPreparingTriggers
	imageReservationPublishingTriggers
	imageReservationSchedulingTriggers
	imageReservationPublished
	imageReservationReleased
)

const (
	// Each entry covers its lane, the two key maps, and their bounded bucket
	// slack. The base covers map headers even for an empty B. These charges
	// remain owned by the image, including while an old run retains it.
	imageMapBaseCharge  = 4 << 10
	imageMapEntryCharge = 1 << 10
)

func imageMapCharge(keys int) (int, bool) {
	if keys < 0 || keys > (MaxMemoryBytes-imageMapBaseCharge)/imageMapEntryCharge {
		return 0, false
	}
	return imageMapBaseCharge + keys*imageMapEntryCharge, true
}

func (c *Controller) retainImageLocked(image *programImage) bool {
	if image == nil || image.refs <= 0 {
		return false
	}
	image.refs++
	return true
}

// releaseImageLocked removes the final 041 ownership. Its charge remains
// reserved until the returned interpreter and retained 040 image are freed.
func (c *Controller) releaseImageLocked(image *programImage) *programImage {
	if image == nil || image.refs <= 0 {
		return nil
	}
	image.refs--
	if image.refs != 0 {
		return nil
	}
	if image.lifecycle == programImagePublished {
		for key := range image.ownKeys {
			if l := c.lanes[key]; l != nil && l.imageOwners > 0 {
				l.imageOwners--
				c.pruneLaneLocked(key)
			}
		}
	}
	return image
}

func finishImageRelease(c *Controller, image *programImage) {
	if image != nil && image.interpreter != nil {
		image.interpreter.releaseImage()
	}
	if image != nil && image.charge != 0 {
		c.pool.adjust(image.charge, 0)
	}
}

func (c *Controller) releaseImage(image *programImage) {
	c.mu.Lock()
	finished := c.releaseImageLocked(image)
	c.mu.Unlock()
	finishImageRelease(c, finished)
}

func (c *Controller) ReserveImage(plan Plan, preparer Preparer, executor Executor) (*ImageReservation, error) {
	if c == nil || !plan.Valid() || nilPort(preparer) || nilPort(executor) ||
		(len(plan.items) != 0 && (nilPort(c.reader) || c.reader.Done() == nil)) {
		return nil, ErrInvalidPlan
	}
	if len(plan.scripts) != 0 {
		port, ok := executor.(ScriptExecutor)
		if !ok || nilPort(port) {
			return nil, ErrInvalidPlan
		}
	}
	// Acquire the one-candidate gate and its structural memory before any
	// candidate map, lane, compilation, or foreign service call is made.
	c.mu.Lock()
	if c.closed || c.active == nil || c.pending != nil {
		c.mu.Unlock()
		return nil, ErrInvalidPlan
	}
	structureCharge, valid := imageMapCharge(len(plan.items) + len(plan.scripts))
	if !valid || !c.pool.adjust(0, structureCharge) {
		c.mu.Unlock()
		return nil, ErrCapacity
	}
	r := &ImageReservation{controller: c, state: imageReservationBuilding, admission: c.admission}
	c.pending = r
	c.wg.Add(1)
	c.mu.Unlock()
	image := &programImage{plan: plan, preparer: preparer, executor: executor,
		generation: Generation(nextGeneration.Add(1)), refs: 1, charge: structureCharge, lifecycle: programImageCandidate,
		lanes:   make(map[laneKey]*lane, len(plan.items)+len(plan.scripts)),
		ownKeys: make(map[laneKey]bool, len(plan.items)+len(plan.scripts))}
	prepared := false
	defer func() {
		if !prepared {
			c.mu.Lock()
			if c.pending == r {
				c.pending = nil
			}
			r.state = imageReservationReleased
			finished := c.releaseImageLocked(image)
			c.mu.Unlock()
			finishImageRelease(c, finished)
		}
		c.wg.Done()
	}()
	for id := range plan.items {
		key := laneKey{executionAutomation, id}
		image.lanes[key], image.ownKeys[key] = &lane{active: make(map[RunID]*runEntry)}, true
	}
	for id := range plan.scripts {
		name, _ := id.Value()
		key := laneKey{executionScript, name}
		image.lanes[key], image.ownKeys[key] = &lane{active: make(map[RunID]*runEntry)}, true
	}
	if interpreter, ok := executor.(*Interpreter); ok {
		preparation, same := preparer.(*Interpreter)
		if !same || preparation != interpreter {
			return nil, ErrInvalidPlan
		}
		if err := interpreter.bindImage(c, image); err != nil {
			return nil, err
		}
		image.interpreter = interpreter
		image.charge += interpreter.charge
	}
	c.mu.Lock()
	if c.closed || c.active == nil || c.pending != r || r.state != imageReservationBuilding {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	stageCharge, valid := imageMapCharge(len(c.lanes) + len(image.lanes))
	if !valid || !c.pool.adjust(0, stageCharge) {
		c.mu.Unlock()
		return nil, ErrCapacity
	}
	image.charge += stageCharge
	staged := make(map[laneKey]*lane, len(c.lanes)+len(image.lanes))
	for key, current := range c.lanes {
		staged[key] = current
	}
	for key, fresh := range image.lanes {
		if staged[key] == nil {
			staged[key] = fresh
		}
	}
	image.lanes = staged
	r.image, r.state = image, imageReservationPrepared
	prepared = true
	c.mu.Unlock()
	return r, nil
}

func (r *ImageReservation) Release() error {
	if r == nil || r.controller == nil {
		return ErrInvalidPlan
	}
	c := r.controller
	c.mu.Lock()
	if r.state != imageReservationPrepared || r.image == nil || c.pending != r {
		c.mu.Unlock()
		return ErrInvalidPlan
	}
	r.state = imageReservationReleased
	if r.admission != nil {
		r.admission.phase = imageAdmissionResolved
		c.signalProgramAdmissionsLocked()
		c.releaseAdmissionLocked(r.admission)
		if c.admission == r.admission {
			c.admission = nil
		}
	}
	c.pending = nil
	finished := c.releaseImageLocked(r.image)
	if finished != nil {
		c.wg.Add(1)
	}
	c.mu.Unlock()
	finishImageRelease(c, finished)
	if finished != nil {
		c.wg.Done()
	}
	return nil
}

func (r *ImageReservation) Publish() error {
	if r == nil || r.controller == nil {
		return ErrInvalidPlan
	}
	c := r.controller
	c.mu.Lock()
	if r.state != imageReservationPrepared || r.image == nil || c.closed || c.pending != r {
		c.mu.Unlock()
		return ErrInvalidPlan
	}
	image := r.image
	var waitPlan *automationtriggers.PlanRetention
	if image.interpreter != nil {
		waitPlan = image.interpreter.waitPlan
	}
	if waitPlan != nil {
		if !c.retainImageLocked(image) {
			c.mu.Unlock()
			return ErrInvalidPlan
		}
		c.wg.Add(1)
	}
	c.mu.Unlock()
	ready := true
	if waitPlan != nil {
		ready = waitPlan.ReadyToSelect()
		c.releaseImage(image)
		c.wg.Done()
	}
	c.mu.Lock()
	if !ready || r.state != imageReservationPrepared || r.image != image || c.closed || c.pending != r {
		c.mu.Unlock()
		return ErrInvalidPlan
	}
	old := c.active
	c.lanes = image.lanes
	c.active, c.plan, c.generation = image, image.plan, image.generation
	image.lifecycle = programImagePublished
	for key := range image.ownKeys {
		c.lanes[key].imageOwners++
	}
	for key := range c.lanes {
		c.pruneLaneLocked(key)
	}
	r.state = imageReservationPublished
	if r.admission == nil || r.admission.phase != imageAdmissionPaused {
		c.pending = nil
	}
	finished := c.releaseImageLocked(old)
	if finished != nil {
		c.wg.Add(1)
	}
	c.mu.Unlock()
	finishImageRelease(c, finished)
	if finished != nil {
		c.wg.Done()
	}
	return nil
}

func (r *ImageReservation) withPreparedTriggerPlan(operation imageReservationState, call func(*automationtriggers.PlanRetention) error) error {
	if r == nil || r.controller == nil {
		return ErrInvalidPlan
	}
	if operation != imageReservationPreparingTriggers && operation != imageReservationPublishingTriggers && operation != imageReservationSchedulingTriggers {
		return ErrInvalidPlan
	}
	c := r.controller
	c.mu.Lock()
	if r.state != imageReservationPrepared || c.closed || c.pending != r || r.image == nil ||
		r.image.interpreter == nil || r.image.interpreter.waitPlan == nil {
		c.mu.Unlock()
		return ErrInvalidPlan
	}
	image := r.image
	waitPlan := image.interpreter.waitPlan
	if !c.retainImageLocked(image) {
		c.mu.Unlock()
		return ErrInvalidPlan
	}
	r.state = operation
	c.wg.Add(1)
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if r.state == operation && c.pending == r {
			r.state = imageReservationPrepared
		}
		c.mu.Unlock()
		c.releaseImage(image)
		c.wg.Done()
	}()
	err := call(waitPlan)
	c.mu.Lock()
	valid := r.state == operation && !c.closed && c.pending == r && r.image == image
	if valid {
		r.state = imageReservationPrepared
	}
	c.mu.Unlock()
	if !valid {
		return ErrInvalidPlan
	}
	return err
}

func (r *ImageReservation) PrepareTriggerPublication() error {
	return r.withPreparedTriggerPlan(imageReservationPreparingTriggers, func(waitPlan *automationtriggers.PlanRetention) error {
		return waitPlan.PreparePublication()
	})
}

// PublishTriggers makes the retained 040 image current in the same existing
// service. The owner still controls the enclosing common publication barrier.
func (r *ImageReservation) PublishTriggers() error {
	return r.withPreparedTriggerPlan(imageReservationPublishingTriggers, func(waitPlan *automationtriggers.PlanRetention) error {
		return waitPlan.Publish()
	})
}

// Prepare the real permanent schedules before the final 041 memory swap.
func (r *ImageReservation) ScheduleTriggerPublication(ctx context.Context) error {
	return r.withPreparedTriggerPlan(imageReservationSchedulingTriggers, func(waitPlan *automationtriggers.PlanRetention) error {
		return waitPlan.SchedulePublication(ctx)
	})
}

func (r *ImageReservation) DiscardTriggerPublication(ctx context.Context) error {
	if r == nil || r.controller == nil || nilPort(ctx) {
		return ErrInvalidPlan
	}
	c := r.controller
	c.mu.Lock()
	if c.pending != r || r.image == nil || r.image.interpreter == nil || (r.state != imageReservationPrepared && r.state != imageReservationPublished) {
		c.mu.Unlock()
		return ErrInvalidPlan
	}
	waits := r.image.interpreter.waitPlan
	if waits == nil {
		c.mu.Unlock()
		return nil
	}
	c.retainImageLocked(r.image)
	c.wg.Add(1)
	c.mu.Unlock()
	defer c.wg.Done()
	defer c.releaseImage(r.image)
	return waits.DiscardPublication(ctx)
}

// SchedulePublishedTriggers is deliberately outside the short publication
// lock: a SunSource can block and must be joined without a Controller mutex.
func (r *ImageReservation) SchedulePublishedTriggers() error {
	if r == nil || r.controller == nil {
		return ErrInvalidPlan
	}
	c := r.controller
	c.mu.Lock()
	if r.state != imageReservationPublished || c.active != r.image || c.closed ||
		r.image == nil || r.image.interpreter == nil || r.image.interpreter.waitPlan == nil ||
		!c.retainImageLocked(r.image) {
		c.mu.Unlock()
		return ErrInvalidPlan
	}
	waitPlan := r.image.interpreter.waitPlan
	c.wg.Add(1)
	c.mu.Unlock()
	defer func() {
		c.releaseImage(r.image)
		c.wg.Done()
	}()
	return waitPlan.SchedulePublished()
}

func (c *Controller) pruneLaneLocked(key laneKey) {
	l := c.lanes[key]
	if l != nil && l.imageOwners == 0 && len(l.active) == 0 && len(l.queue) == 0 && l.replacement == nil {
		delete(c.lanes, key)
	}
}

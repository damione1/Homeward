package automationruns

import (
	"context"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/domain"
)

type imageAdmissionPhase uint8

const (
	imageAdmissionInvalid imageAdmissionPhase = iota
	imageAdmissionPaused
	imageAdmissionResolved
)

// A replacement holds admission separately from Controller's terminal close.
// Its waiters consume the existing run/preparation domain, never a new queue.
type imageAdmission struct {
	phase   imageAdmissionPhase
	changed chan struct{}
	timeout time.Duration
	waiters int
	charge  int
}

// Header/channels plus the admitted maximum of caller-owned timer waits.
// This conservative bound stays in the same 16 MiB pool until the last
// waiter has observed resolution; supersession transfers it without a gap.
const imageAdmissionCharge = 1024 + (MaxLiveRuns+MaxPreparing)*512

func (c *Controller) CloneTriggerPlan() (automationtriggers.Plan, error) {
	if c == nil {
		return automationtriggers.Plan{}, ErrInvalidPlan
	}
	c.mu.Lock()
	image := c.active
	if c.closed || image == nil || image.interpreter == nil || !c.retainImageLocked(image) {
		c.mu.Unlock()
		return automationtriggers.Plan{}, ErrInvalidPlan
	}
	c.wg.Add(1)
	c.mu.Unlock()
	defer c.wg.Done()
	defer c.releaseImage(image)
	image.interpreter.mu.Lock()
	waits := image.interpreter.waitPlan
	image.interpreter.mu.Unlock()
	if waits == nil {
		return automationtriggers.Plan{}, nil
	}
	return waits.ClonePlan()
}

func (c *Controller) releaseAdmissionLocked(a *imageAdmission) {
	if a != nil && a.phase == imageAdmissionResolved && a.waiters == 0 && a.charge != 0 {
		c.pool.adjust(a.charge, 0)
		a.charge = 0
	}
}

// ReserveProgram reuses the actual admitted interpreter ports. It neither
// creates another engine nor reuses a previously bound Interpreter.
func (c *Controller) ReserveProgram(plan Plan, triggers automationtriggers.Plan) (*ImageReservation, error) {
	if c == nil {
		return nil, ErrInvalidPlan
	}
	c.mu.Lock()
	if c.closed || c.active == nil || c.active.interpreter == nil {
		c.mu.Unlock()
		return nil, ErrInvalidPlan
	}
	input := c.active.interpreter.input
	if !plan.Valid() {
		plan = c.active.plan
	}
	c.mu.Unlock()
	input.Plan = plan
	if triggers.Valid() {
		input.TriggerPlan = triggers
	} else if input.TriggerWaits != nil {
		var err error
		input.TriggerPlan, err = c.CloneTriggerPlan()
		if err != nil {
			return nil, err
		}
	}
	interpreter, err := NewInterpreter(input)
	if err != nil {
		return nil, err
	}
	return c.ReserveImage(plan, interpreter, interpreter)
}

func (c *Controller) NodeTraces() ([]NodeTrace, uint64) {
	if c == nil {
		return nil, 0
	}
	c.mu.Lock()
	image := c.active
	if c.closed || image == nil || image.interpreter == nil || !c.retainImageLocked(image) {
		c.mu.Unlock()
		return nil, 0
	}
	c.wg.Add(1)
	c.mu.Unlock()
	defer c.wg.Done()
	defer c.releaseImage(image)
	return image.interpreter.NodeTraces()
}

func (c *Controller) signalProgramAdmissionsLocked() {
	if c.admission != nil {
		a := c.admission
		close(a.changed)
		if a.phase == imageAdmissionPaused {
			a.changed = make(chan struct{})
		} else {
			a.changed = nil
		}
	}
}

// lockProgramAdmission returns with c.mu held on success. Waiting owns no
// mutex, is bounded/cancellable, and participates in Controller.Done.
func (c *Controller) lockProgramAdmission(ctx context.Context) error {
	if c == nil || nilPort(ctx) {
		return ErrInvalidPlan
	}
	var deadline time.Time
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return ErrClosed
		}
		a := c.admission
		if a == nil || a.phase == imageAdmissionResolved {
			return nil
		}
		if a.phase != imageAdmissionPaused || a.waiters == MaxLiveRuns+MaxPreparing {
			c.mu.Unlock()
			return ErrCapacity
		}
		if deadline.IsZero() {
			deadline = time.Now().Add(a.timeout)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			c.mu.Unlock()
			return context.DeadlineExceeded
		}
		changed := a.changed
		a.waiters++
		c.wg.Add(1)
		c.mu.Unlock()
		timer := time.NewTimer(remaining)
		var err error
		select {
		case <-changed:
		case <-ctx.Done():
			err = ctx.Err()
		case <-timer.C:
			err = context.DeadlineExceeded
		}
		timer.Stop()
		c.mu.Lock()
		a.waiters--
		c.releaseAdmissionLocked(a)
		c.mu.Unlock()
		c.wg.Done()
		if err != nil {
			return err
		}
	}
}

// PauseAdmissions closes only new program preparations. Runs, waits and their
// causal images remain alive. The activation owner calls this before SQL.
func (r *ImageReservation) PauseAdmissions(ctx context.Context, timeout time.Duration) error {
	if r == nil || r.controller == nil || nilPort(ctx) || timeout <= 0 || timeout > time.Minute {
		return ErrInvalidPlan
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := r.controller
	c.mu.Lock()
	if c.closed || c.pending != r || r.state != imageReservationPrepared {
		c.mu.Unlock()
		return ErrInvalidPlan
	}
	if r.admission != nil && r.admission == c.admission && r.admission.phase == imageAdmissionPaused && c.preparing == 0 {
		c.mu.Unlock()
		return nil
	}
	if r.admission != nil || c.admission != nil {
		c.mu.Unlock()
		return ErrInvalidPlan
	}
	if !c.pool.adjust(0, imageAdmissionCharge) {
		c.mu.Unlock()
		return ErrCapacity
	}
	a := &imageAdmission{phase: imageAdmissionPaused, changed: make(chan struct{}), timeout: timeout, charge: imageAdmissionCharge}
	r.admission = a
	c.admission = a
	c.mu.Unlock()
	for {
		c.mu.Lock()
		if c.closed || c.pending != r || r.admission != a || a.phase != imageAdmissionPaused {
			c.mu.Unlock()
			return ErrClosed
		}
		if c.preparing == 0 {
			c.mu.Unlock()
			return nil
		}
		changed := a.changed
		c.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			_ = r.ResumeAdmissions()
			return ctx.Err()
		}
	}
}

// ResumeAdmissions consumes the temporary gate only after the owner has
// proved old authority or coherent new publication. It never decides SQL.
func (r *ImageReservation) ResumeAdmissions() error {
	if r == nil || r.controller == nil {
		return ErrInvalidPlan
	}
	c := r.controller
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.pending != r || r.admission == nil || r.admission.phase != imageAdmissionPaused ||
		(r.state != imageReservationPrepared && r.state != imageReservationPublished) {
		return ErrInvalidPlan
	}
	r.admission.phase = imageAdmissionResolved
	c.signalProgramAdmissionsLocked()
	c.releaseAdmissionLocked(r.admission)
	c.admission = nil
	if r.state == imageReservationPublished {
		c.pending = nil
	}
	return nil
}

// ReleaseForReplacement returns B's real charge while the Controller retains
// the same paused admission. Supersession can reserve C without an A window.
func (r *ImageReservation) ReleaseForReplacement() error {
	if r == nil || r.controller == nil {
		return ErrInvalidPlan
	}
	c := r.controller
	c.mu.Lock()
	if c.closed || c.pending != r || (r.state != imageReservationPrepared && r.state != imageReservationPublished) || r.image == nil ||
		r.admission == nil || c.admission != r.admission || r.admission.phase != imageAdmissionPaused {
		c.mu.Unlock()
		return ErrInvalidPlan
	}
	published := r.state == imageReservationPublished
	r.state, c.pending = imageReservationReleased, nil
	var finished *programImage
	if !published {
		finished = c.releaseImageLocked(r.image)
	}
	c.wg.Add(1)
	c.mu.Unlock()
	finishImageRelease(c, finished)
	c.wg.Done()
	return nil
}

// CancelChanged is the post-COMMIT transfer. It compares each live causal
// image (including retained predecessors) with B, then removes affected queue
// entries atomically before cancelling active work. Intact runs keep A.
// A timeout retains the gate and can be retried without another SQL write.
func (r *ImageReservation) CancelChanged(ctx context.Context, aliases []domain.HAAlias) error {
	if r == nil || r.controller == nil || nilPort(ctx) || len(aliases) > 4096 {
		return ErrInvalidPlan
	}
	touched := make(map[domain.HAAlias]bool, len(aliases))
	for _, alias := range aliases {
		if _, err := alias.Value(); err != nil {
			return ErrInvalidPlan
		}
		touched[alias] = true
	}
	c := r.controller
	c.mu.Lock()
	if c.closed || c.pending != r || r.state != imageReservationPrepared || r.admission == nil ||
		r.admission.phase != imageAdmissionPaused || c.preparing != 0 {
		c.mu.Unlock()
		return ErrInvalidPlan
	}
	images := make(map[*programImage]automationtriggers.EnvironmentChanges)
	entries := make(map[*runEntry]bool)
	c.wg.Add(1)
	for _, lane := range c.lanes {
		capture := func(entry *runEntry) {
			if entry == nil {
				return
			}
			entries[entry] = false
			if _, retained := images[entry.image]; !retained && c.retainImageLocked(entry.image) {
				images[entry.image] = automationtriggers.EnvironmentChanges{}
			}
		}
		for _, entry := range lane.active {
			capture(entry)
		}
		for _, entry := range lane.queue {
			capture(entry)
		}
		capture(lane.replacement)
	}
	c.mu.Unlock()
	defer c.wg.Done()
	defer func() {
		for image := range images {
			c.releaseImage(image)
		}
	}()
	for image := range images {
		var environment automationtriggers.EnvironmentChanges
		if image.interpreter.waitPlan != nil || r.image.interpreter.waitPlan != nil {
			var err error
			environment, err = image.interpreter.waitPlan.EnvironmentChanges(r.image.interpreter.waitPlan)
			if err != nil {
				return err
			}
		}
		images[image] = environment
		if source := image.interpreter.input.TemplateWaits; source != nil {
			for _, alias := range source.ChangedGroupAliases(touched) {
				touched[alias] = true
			}
		}
	}
	for entry := range entries {
		// The arena lock observes every live branch together. No environment
		// escapes or is copied outside its admitted owner/charge.
		arena := entry.arena
		arena.mu.Lock()
		if !arena.closed {
			lookup := func(name string) (string, bool) {
				value, found := "", false
				for _, environment := range arena.environments {
					if environment.closed {
						continue
					}
					binding, present := environment.data[name]
					if !present {
						continue
					}
					text, valid := binding.StringValue()
					if !valid || found && text != value {
						return "", false
					}
					value, found = text, true
				}
				return value, found
			}
			entries[entry] = changedProgram(entry.image.plan, r.image.plan, entry.run.trigger.laneKey(),
				touched, images[entry.image], make(map[laneKey]bool), lookup)
		}
		arena.mu.Unlock()
	}
	c.mu.Lock()
	if c.closed || c.pending != r || r.state != imageReservationPrepared {
		c.mu.Unlock()
		return ErrClosed
	}
	active := make([]*runEntry, 0, MaxLiveRuns)
	detached := make([]*runEntry, 0, MaxLiveRuns)
	for key, lane := range c.lanes {
		affected := func(entry *runEntry) bool { return entry != nil && entries[entry] }
		for _, entry := range lane.active {
			if affected(entry) {
				active = append(active, entry)
			}
		}
		keep := lane.queue[:0]
		for _, entry := range lane.queue {
			if affected(entry) {
				detached = append(detached, entry)
			} else {
				keep = append(keep, entry)
			}
		}
		clear(lane.queue[len(keep):])
		lane.queue = keep
		if affected(lane.replacement) {
			detached = append(detached, lane.replacement)
			lane.replacement = nil
		}
		c.pruneLaneLocked(key)
	}
	for _, entry := range detached {
		c.observeTerminalLocked(entry, DecisionCancelled)
		c.live--
		c.recordLocked(Trace{Decision: DecisionCancelled, Automation: entry.run.trigger.automation,
			Script: entry.run.trigger.script, RunID: entry.run.id, ParentID: entry.run.parent,
			Generation: entry.run.generation, Provenance: entry.run.provenance})
	}
	c.mu.Unlock()
	for _, entry := range active {
		entry.cancel()
	}
	for _, entry := range detached {
		entry.cancel()
		entry.err = context.Canceled
		entry.arena.close()
		entry.acknowledgeStartup(StartupCancelledBeforeExecution, context.Canceled)
		close(entry.done)
		c.releaseImage(entry.image)
	}
	for _, entry := range active {
		select {
		case <-entry.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return ctx.Err()
}

func changedProgram(old, next Plan, key laneKey, aliases map[domain.HAAlias]bool, environment automationtriggers.EnvironmentChanges, seen map[laneKey]bool, bindings func(string) (string, bool)) bool {
	// Script call parameters are admitted literals. Revisit local expressions
	// for each caller's bindings, while each fixed descendant edge needs only
	// one traversal (including a recursive edge back to an ancestor).
	revisited := seen[key]
	seen[key] = true
	var actions []ir.Action
	var variables []ir.Variable
	var conditions []ir.Condition
	if key.kind == executionAutomation {
		a, exists := old.items[key.identity]
		b, present := next.items[key.identity]
		if !exists || !present || !a.value.SameBehavior(b.value) {
			return true
		}
		for _, trigger := range a.value.Triggers() {
			if aliases[trigger.EntityAlias()] || changedTriggerEnvironment(trigger, environment) {
				return true
			}
		}
		conditions, variables, actions = a.value.Conditions(), a.value.Variables(), a.value.Actions()
	} else {
		id, err := ir.ParseScriptID(key.identity)
		if err != nil {
			return true
		}
		a, exists := old.scripts[id]
		b, present := next.scripts[id]
		if !exists || !present || !a.script.SameBehavior(b.script) {
			return true
		}
		variables, actions = a.script.Variables(), a.script.Actions()
	}
	lookup := stableProgramAliasBindings(bindings, variables, actions)
	if changedConditions(conditions, aliases, lookup) {
		return true
	}
	var changedActions func([]ir.Action) bool
	changedActions = func(values []ir.Action) bool {
		for _, action := range values {
			if changedConditions(action.Conditions(), aliases, lookup) || changedTemplate(action.WaitProgram(), aliases, lookup) {
				return true
			}
			for _, trigger := range action.Triggers() {
				if aliases[trigger.EntityAlias()] || changedTriggerEnvironment(trigger, environment) {
					return true
				}
			}
			call := action.Call()
			for _, target := range call.Targets() {
				if aliases[target.Alias()] {
					return true
				}
			}
			for _, script := range call.Scripts() {
				if revisited {
					continue
				}
				name, _ := script.Value()
				if changedProgram(old, next, laneKey{executionScript, name}, aliases, environment, seen, stableProgramAliasBindings(nil, call.ScriptVariables(), nil)) {
					return true
				}
			}
			if changedActions(action.Children()) || changedActions(action.Otherwise()) {
				return true
			}
			for _, branch := range action.Branches() {
				if changedConditions(branch.Conditions(), aliases, lookup) || changedActions(branch.Actions()) {
					return true
				}
			}
		}
		return false
	}
	return changedActions(actions)
}

func changedTriggerEnvironment(trigger ir.Trigger, changes automationtriggers.EnvironmentChanges) bool {
	switch trigger.Kind() {
	case ir.TriggerSunset:
		return changes.Sun()
	case ir.TriggerTime, ir.TriggerTimePattern:
		return changes.Civil()
	case ir.TriggerMQTT, ir.TriggerDeviceMQTT:
		return changes.MQTT()
	}
	return false
}

func changedConditions(values []ir.Condition, aliases map[domain.HAAlias]bool, bindings func(string) (string, bool)) bool {
	for _, condition := range values {
		if aliases[condition.EntityAlias()] || changedTemplate(condition.Program(), aliases, bindings) {
			return true
		}
	}
	return false
}
func changedTemplate(program template.Program, aliases map[domain.HAAlias]bool, bindings func(string) (string, bool)) bool {
	return program.ReadsChangedAliases(aliases, bindings)
}

// A known run binding is stable only when every literal assignment in its
// admitted program agrees. Branches/repeats are all visited; unresolved or
// conflicting values retain an explicit conservative dependency.
func stableProgramAliasBindings(current func(string) (string, bool), defaults []ir.Variable, actions []ir.Action) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, found := "", false
		if current != nil {
			value, found = current(name)
		}
		if !found {
			for _, variable := range defaults {
				if variable.Name() == name && variable.Value().Kind() == ir.ValueString {
					value, found = variable.Value().Text(), true
				}
			}
		}
		valid := true
		var visit func([]ir.Action)
		visit = func(values []ir.Action) {
			for _, action := range values {
				for _, variable := range action.Variables() {
					if variable.Name() != name {
						continue
					}
					if variable.Value().Kind() != ir.ValueString || found && value != variable.Value().Text() {
						valid = false
					} else {
						value, found = variable.Value().Text(), true
					}
				}
				visit(action.Children())
				visit(action.Otherwise())
				for _, branch := range action.Branches() {
					visit(branch.Actions())
				}
			}
		}
		visit(actions)
		return value, found && valid
	}
}

package server

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/reactivetemplate"
	"github.com/damione1/Homeward/internal/storage"
)

// The barrier comes from the real 040 solar preparation callback after the
// durable configuration commit. It is not an injected Owner publication hook.
type nha029C5ProjectionSun struct{ child *nha029C5Child }

func (s nha029C5ProjectionSun) NextSunset(ctx context.Context, _ time.Time, _ string, _ automationtriggers.SolarPosition) (automationtriggers.SunResult, error) {
	if err := s.child.wire.send(nha029C5Message{kind: nha029C5Projection}); err != nil {
		return automationtriggers.SunResult{}, err
	}
	<-ctx.Done()
	return automationtriggers.SunResult{}, ctx.Err()
}

func (c *nha029C5Child) projectionRequest() (configactivation.Request, error) {
	automations, scripts, err := nha042ServerPrograms(nha029C5ParentBody, nha029OwnerChild, false)
	if err != nil {
		return configactivation.Request{}, err
	}
	sun, err := ir.NewEntity("sun.sun", ir.ReferencePresent, ir.SupportObserveState)
	if err != nil {
		return configactivation.Request{}, err
	}
	catalogue, err := ir.NewCatalogue([]ir.Entity{sun}, nil, nil)
	if err != nil {
		return configactivation.Request{}, err
	}
	file, err := parser.NewSourceFile("nha029-c5-sun.yaml", []byte("id: nha029_c5_sun\ntriggers:\n- trigger: sun\n  event: sunset\nactions:\n- stop: synthetic_projection_barrier\n"))
	if err != nil {
		return configactivation.Request{}, err
	}
	compiled, err := ir.CompileSources("automation.nha029_c5_sun", file.Path(), []parser.SourceFile{file}, catalogue)
	if err != nil || len(compiled.Accepted()) != 1 || len(compiled.Rejected()) != 0 {
		return configactivation.Request{}, errors.Join(err, errors.New("C5 solar fixture compilation refused"))
	}
	automations = append(automations, compiled.Accepted()...)
	runs, err := automationruns.NewPlanWithScripts(automations, scripts)
	if err != nil {
		return configactivation.Request{}, err
	}
	c.pilot.mu.Lock()
	binding := c.pilot.binding
	c.pilot.mu.Unlock()
	_, target, err := binding.IDs()
	if err != nil {
		return configactivation.Request{}, err
	}
	alias, err := domain.ParseHAAlias(nha042ServerAlias)
	if err != nil {
		return configactivation.Request{}, err
	}
	position, err := automationtriggers.NewSolarPosition(43, -79)
	if err != nil {
		return configactivation.Request{}, err
	}
	triggers, err := automationtriggers.NewPlan(automationtriggers.PlanInput{Automations: automations, Scripts: scripts, Resolver: nha042ServerResolver{entities: map[domain.HAAlias]domain.EntityID{alias: target}}, Zone: "UTC", SunSource: nha029C5ProjectionSun{child: c}, SolarPosition: position})
	if err != nil {
		return configactivation.Request{}, err
	}
	templates, err := (reactivetemplate.Plan{}).WithScriptWaits(time.UTC)
	if err != nil {
		return configactivation.Request{}, err
	}
	image, err := configactivation.NewProgramImage(runs, triggers, templates, nil)
	if err != nil {
		return configactivation.Request{}, err
	}
	author, err := storage.NewConfigAuthor("nha029:c5-trusted-solar-fixture")
	if err != nil {
		return configactivation.Request{}, err
	}
	source, err := storage.NewConfigSource("nha029:c5-physical-workspace")
	if err != nil {
		return configactivation.Request{}, err
	}
	return configactivation.NewWorkspaceRequest(configactivation.RetainDefinitions(), author, source, nil, &image, nil)
}

func TestNHA029C5CrashDuringActual040Projection(t *testing.T) {
	f := nha029C5NewFixture(t)
	child := f.start(t, false)
	child.ready(t, nil)
	a := f.active(t)
	child.fireWaits(t)
	if len(f.effects) != 1 {
		t.Fatal("projection precondition lacks the terminal simulated effect")
	}
	f.edit(t, "B real solar projection")
	child.send(t, nha029C5Message{kind: nha029C5ActivateProjection})
	child.await(t, nha029C5Projection)
	b := f.active(t)
	if b.Revision().Number() <= a.Revision().Number() || b.ManifestSHA256() == a.ManifestSHA256() {
		t.Fatal("solar callback preceded the actual durable B commit")
	}
	child.status(t, false)
	counts := f.counts(t)
	child.kill(t)
	if err := os.Remove(f.path); err != nil {
		t.Fatal(err)
	}
	// Configured restart must reopen SQL B, not select the prior A workspace or
	// pretend the transient Logic1 solar fixture was a persisted definition.
	restarted := f.start(t, true)
	restarted.ready(t, &b)
	restarted.stop(t)
	if f.counts(t) != counts || len(f.effects) != 1 {
		t.Fatal("projection crash restart rewrote configuration or replayed a command")
	}
}

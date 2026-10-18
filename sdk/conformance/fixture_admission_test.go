package conformance

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
)

var errAdmissionProbe = errors.New("admission probe completed")

type admissionProbe struct {
	t        *testing.T
	retained FixtureAdmission
}

type nilAdmissionContext struct{ context.Context }

func (*admissionProbe) Completion() CompletionSemantics { return CompletionAcknowledged }
func (*admissionProbe) Spec(IntegrationInstanceID) (PilotSpec, error) {
	panic("admission probe must stop before Supervisor.Start")
}
func (*admissionProbe) Ready(context.Context) error                { panic("unexpected Ready") }
func (*admissionProbe) Delivered(context.Context, CommandID) error { panic("unexpected Delivered") }
func (*admissionProbe) Apply(context.Context, CommandID, Stimulus) error {
	panic("unexpected Apply")
}
func (*admissionProbe) Crash(context.Context) error { panic("unexpected Crash") }
func (*admissionProbe) Deliveries() int             { panic("unexpected Deliveries") }
func (*admissionProbe) Close() error                { panic("unexpected Close") }

func (p *admissionProbe) PrepareFixture(ctx context.Context, admission FixtureAdmission) error {
	p.retained = admission
	assertRejected := func(label string, err error) {
		p.t.Helper()
		if !errors.Is(err, integrations.ErrInvalidInput) {
			p.t.Errorf("%s: got %v, want invalid admission", label, err)
		}
	}
	assertRejected("zero sources", admission.AddSources(ctx, nil))
	assertRejected("zero binding", admission.AddSources(ctx, []SupplementalBinding{{}}))
	first, err := NewSupplementalBinding("extra-device-0", "extra-entity-0", "switch.extra_0")
	if err != nil {
		p.t.Fatal(err)
	}
	var typedNil *nilAdmissionContext
	assertRejected("typed nil context", admission.AddSources(typedNil,
		[]SupplementalBinding{first}))
	primaryKey, _ := NewSupplementalBinding(SwitchSource().Device,
		SwitchSource().Entity, "switch.extra_primary_key")
	assertRejected("primary key replacement", admission.AddSources(ctx, []SupplementalBinding{primaryKey}))
	primaryAlias, _ := NewSupplementalBinding("extra-device-alias", "extra-entity-alias",
		"switch.nha095_relay_1")
	assertRejected("primary alias replacement", admission.AddSources(ctx, []SupplementalBinding{primaryAlias}))
	if err := admission.AddSources(ctx, []SupplementalBinding{first}); err != nil {
		p.t.Fatalf("first supplemental source: %v", err)
	}
	assertRejected("duplicate binding", admission.AddSources(ctx, []SupplementalBinding{first}))
	duplicateAlias, _ := NewSupplementalBinding("extra-device-new", "extra-entity-new", "switch.extra_0")
	assertRejected("duplicate alias", admission.AddSources(ctx, []SupplementalBinding{duplicateAlias}))
	for i := 1; i < 64; i++ {
		binding, err := NewSupplementalBinding(fmt.Sprintf("extra-device-%d", i),
			fmt.Sprintf("extra-entity-%d", i), fmt.Sprintf("switch.extra_%d", i))
		if err != nil {
			p.t.Fatal(err)
		}
		if err := admission.AddSources(ctx, []SupplementalBinding{binding}); err != nil {
			p.t.Fatalf("source %d within cap: %v", i, err)
		}
	}
	beyond, _ := NewSupplementalBinding("extra-device-64", "extra-entity-64", "switch.extra_64")
	assertRejected("source cap", admission.AddSources(ctx, []SupplementalBinding{beyond}))
	assertRejected("nil secrets", admission.AttachSecrets(nil))
	return errAdmissionProbe
}

func TestNHA077FixtureAdmissionIsBoundedAndClosedAfterCallback(t *testing.T) {
	probe := &admissionProbe{t: t}
	_, err := newRig(t.Context(), probe, CaseCore, DefaultFixtureProfile())
	if !errors.Is(err, errAdmissionProbe) {
		t.Fatalf("setup did not run the probe: %v", err)
	}
	binding, err := NewSupplementalBinding("late-device", "late-entity", "switch.late")
	if err != nil {
		t.Fatal(err)
	}
	if err := probe.retained.AddSources(t.Context(), []SupplementalBinding{binding}); !errors.Is(err, integrations.ErrInvalidInput) {
		t.Fatalf("retained AddSources admitted after callback: %v", err)
	}
	if err := probe.retained.AttachSecrets(new(storage.SecretRepository)); !errors.Is(err, integrations.ErrInvalidInput) {
		t.Fatalf("retained AttachSecrets admitted after callback: %v", err)
	}
}

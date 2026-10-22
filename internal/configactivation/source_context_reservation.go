package configactivation

import (
	"context"
	"errors"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
)

type sourceContextReservations struct {
	registry     *registry.ActivationGate
	pause        *integrations.ActivationPause
	declarations *integrations.ConfigurationDeclarationsLease
	taken        bool
}

func (o *Owner) reserveSourceContext(ctx context.Context, request Request, current AdmittedGeneration) (Request, error) {
	groups, err := replaceSourceGroup(current.extraGroups, request.sourceReplacement)
	if err != nil || len(groups) == 0 {
		return request, err
	}
	if request.sourceReservations != nil {
		return Request{}, ErrInvalidStartup
	}
	if o.runtime.Commands == nil || o.runtime.Host == nil {
		return Request{}, ErrProgramCompositionRequired
	}
	reserved := &sourceContextReservations{}
	reserved.registry, err = o.input.Registry.BeginActivation(ctx)
	if err == nil {
		reserved.pause, err = o.runtime.Commands.PauseAndDrain(ctx)
	}
	if err == nil {
		reserved.declarations, err = o.runtime.Host.ReserveConfigurationDeclarations(ctx)
	}
	if err != nil {
		return Request{}, errors.Join(err, o.releaseSourceContextReservations(reserved))
	}
	request.sourceReservations = reserved
	return request, nil
}

func (o *Owner) releaseSourceContextReservations(reserved *sourceContextReservations) error {
	if reserved == nil || reserved.taken {
		return nil
	}
	var err error
	if reserved.declarations != nil {
		err = reserved.declarations.Release()
	}
	if reserved.registry != nil {
		err = errors.Join(err, reserved.registry.Release())
	}
	if reserved.pause != nil {
		if err == nil {
			err = o.runtime.Commands.ResumeOld(reserved.pause)
		} else {
			_ = o.runtime.Commands.Quarantine(reserved.pause)
		}
	}
	return err
}

func (o *Owner) adoptSourceContextReservations(t *activationTransfer, request Request) {
	if reserved := request.sourceReservations; reserved != nil {
		t.registry, t.pause, t.declarations = reserved.registry, reserved.pause, reserved.declarations
		reserved.taken = true
	}
}
func (o *Owner) releaseSourceDeclarations(t *activationTransfer) error {
	if t.declarations == nil {
		return nil
	}
	if err := t.declarations.Release(); err != nil {
		return err
	}
	t.declarations = nil
	return nil
}

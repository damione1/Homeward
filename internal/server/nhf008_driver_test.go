package server

import (
	"context"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/integrations"
	"google.golang.org/protobuf/proto"
)

// Captures only typed fixture arguments, never authentication headers/tokens.
type nhf008Driver struct {
	nhf006Driver
	arguments []*actions.ActionArgs
}

func (d *nhf008Driver) Dispatch(ctx context.Context, command integrations.AdmittedCommand) error {
	request, err := command.Request()
	if err != nil {
		return err
	}
	if err := d.nhf006Driver.Dispatch(ctx, command); err != nil {
		return err
	}
	d.mu.Lock()
	d.arguments = append(d.arguments, proto.CloneOf(request.Arguments))
	d.mu.Unlock()
	return nil
}

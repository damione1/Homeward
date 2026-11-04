package cli

import (
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/apitypes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func (r *remote) integrations() apiv2connect.IntegrationServiceClient {
	return apiv2connect.NewIntegrationServiceClient(r.client, r.base)
}

type IntegrationPageFlags struct {
	Home      string `arg:"" help:"Canonical home resource name."`
	PageSize  int32  `help:"Requested page size; zero selects the server default."`
	PageToken string `help:"Opaque continuation returned by the same listing."`
}
type integrationCatalogue struct {
	IntegrationPageFlags `embed:""`
}

func (c *integrationCatalogue) Run(v *invocation) error {
	if _, err := apitypes.ParseHomeName(c.Home); err != nil {
		return fail("invalid_home_name", ExitUsage)
	}
	r, err := v.authenticated()
	if err != nil {
		return err
	}
	defer r.close()
	response, err := r.integrations().ListIntegrationTypes(v.ctx, bearer(&api.ListIntegrationTypesRequest{Parent: c.Home, PageSize: c.PageSize, PageToken: c.PageToken}, r.credential.Access))
	if err != nil {
		return err
	}
	return v.emit(response.Msg)
}

type integrationSchema struct {
	Type string `arg:"" help:"Canonical integration type resource name."`
}

func (c *integrationSchema) Run(v *invocation) error {
	if _, err := apitypes.ParseIntegrationTypeName(c.Type); err != nil {
		return fail("invalid_integration_type_name", ExitUsage)
	}
	r, err := v.authenticated()
	if err != nil {
		return err
	}
	defer r.close()
	response, err := r.integrations().GetIntegrationType(v.ctx, bearer(&api.GetIntegrationTypeRequest{Name: c.Type}, r.credential.Access))
	if err != nil {
		return err
	}
	return v.emit(response.Msg)
}

type integrationList struct {
	IntegrationPageFlags `embed:""`
}

func (c *integrationList) Run(v *invocation) error {
	if _, err := apitypes.ParseHomeName(c.Home); err != nil {
		return fail("invalid_home_name", ExitUsage)
	}
	r, err := v.authenticated()
	if err != nil {
		return err
	}
	defer r.close()
	response, err := r.integrations().ListIntegrationInstances(v.ctx, bearer(&api.ListIntegrationInstancesRequest{Parent: c.Home, PageSize: c.PageSize, PageToken: c.PageToken}, r.credential.Access))
	if err != nil {
		return err
	}
	return v.emit(response.Msg)
}

type integrationGet struct {
	Name string `arg:"" help:"Canonical integration instance resource name."`
}

func (c *integrationGet) Run(v *invocation) error {
	if _, err := apitypes.ParseIntegrationInstanceName(c.Name); err != nil {
		return fail("invalid_integration_instance_name", ExitUsage)
	}
	r, err := v.authenticated()
	if err != nil {
		return err
	}
	defer r.close()
	response, err := r.integrations().GetIntegrationInstance(v.ctx, bearer(&api.GetIntegrationInstanceRequest{Name: c.Name}, r.credential.Access))
	if err != nil {
		return err
	}
	return v.emit(response.Msg)
}

type integrationHealth struct {
	Name string `arg:"" help:"Canonical integration instance resource name."`
}

func (c *integrationHealth) Run(v *invocation) error {
	if _, err := apitypes.ParseIntegrationInstanceName(c.Name); err != nil {
		return fail("invalid_integration_instance_name", ExitUsage)
	}
	r, err := v.authenticated()
	if err != nil {
		return err
	}
	defer r.close()
	response, err := r.integrations().GetIntegrationInstance(v.ctx, bearer(&api.GetIntegrationInstanceRequest{Name: c.Name}, r.credential.Access))
	if err != nil {
		return err
	}
	value := response.Msg
	return v.emit(&api.IntegrationInstance{Name: value.Name, DesiredState: value.DesiredState, ObservedHealth: value.ObservedHealth, ObservedSupervision: value.ObservedSupervision})
}

// The user supplies the opaque etag they observed. No implicit reread, retry,
// source revision decoding, filesystem digest or synthetic client UUID exists.
type IntegrationRequestFlags struct {
	RequestID string `name:"request-id" help:"Optional UUIDv4 for a retained original operation."`
}
type IntegrationMutationFlags struct {
	Etag                    string `required:"" help:"Opaque etag from the integration instance."`
	IntegrationRequestFlags `embed:""`
}

func (flags IntegrationRequestFlags) validate() error {
	if flags.RequestID != "" {
		if _, err := apitypes.ParseRequestID(flags.RequestID); err != nil {
			return fail("invalid_request_id", ExitUsage)
		}
	}
	return nil
}
func readConfiguration(path string) (*api.IntegrationConfiguration, error) {
	data, err := readRegularInput(path)
	if err != nil {
		return nil, err
	}
	defer clear(data)
	value := new(api.IntegrationConfiguration)
	if (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(data, value) != nil || value.SchemaVersion == "" {
		return nil, fail("invalid_integration_configuration_json", ExitUsage)
	}
	return value, nil
}

type integrationCreate struct {
	Type                    string `arg:"" help:"Canonical integration type resource name."`
	ID                      string `arg:"" help:"Native instance key, immutable after creation."`
	Config                  string `required:"" help:"IntegrationConfiguration protobuf JSON file; secret references only."`
	Enabled                 bool   `help:"Initial desired state; defaults to disabled."`
	IntegrationRequestFlags `embed:""`
}

func (c *integrationCreate) Run(v *invocation) error {
	typ, err := apitypes.ParseIntegrationTypeName(c.Type)
	if err != nil {
		return fail("invalid_integration_type_name", ExitUsage)
	}
	if err := c.validate(); err != nil {
		return err
	}
	configuration, err := readConfiguration(c.Config)
	if err != nil {
		return err
	}
	if contracts.AdmitIntegrationConfiguration(configuration, c.ID) != nil {
		return fail("invalid_integration_configuration", ExitUsage)
	}
	r, err := v.authenticated()
	if err != nil {
		return err
	}
	defer r.close()
	state := api.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_DISABLED
	if c.Enabled {
		state = api.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_ENABLED
	}
	response, err := r.integrations().CreateIntegrationInstance(v.ctx, bearer(&api.CreateIntegrationInstanceRequest{Parent: typ.Home().String(), RequestId: c.RequestID, IntegrationInstance: &api.IntegrationInstance{InstanceKey: c.ID, IntegrationType: typ.String(), DesiredState: state, Configuration: &api.IntegrationInstance_Editable{Editable: configuration}}}, r.credential.Access))
	if err != nil {
		return err
	}
	return v.emitIntegrationOperation(response.Msg)
}

type integrationUpdate struct {
	Name                     string `arg:"" help:"Canonical integration instance resource name."`
	Config                   string `required:"" help:"IntegrationConfiguration protobuf JSON file."`
	IntegrationMutationFlags `embed:""`
}

func (c *integrationUpdate) Run(v *invocation) error {
	if _, err := apitypes.ParseIntegrationInstanceName(c.Name); err != nil {
		return fail("invalid_integration_instance_name", ExitUsage)
	}
	if err := c.validate(); err != nil {
		return err
	}
	configuration, err := readConfiguration(c.Config)
	if err != nil {
		return err
	}
	r, err := v.authenticated()
	if err != nil {
		return err
	}
	defer r.close()
	response, err := r.integrations().UpdateIntegrationInstance(v.ctx, bearer(&api.UpdateIntegrationInstanceRequest{RequestId: c.RequestID, IntegrationInstance: &api.IntegrationInstance{Name: c.Name, Etag: c.Etag, Configuration: &api.IntegrationInstance_Editable{Editable: configuration}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"editable"}}}, r.credential.Access))
	if err != nil {
		return err
	}
	return v.emitIntegrationOperation(response.Msg)
}

type integrationDelete struct {
	Name                     string `arg:""`
	IntegrationMutationFlags `embed:""`
}

func (c *integrationDelete) Run(v *invocation) error {
	if _, err := apitypes.ParseIntegrationInstanceName(c.Name); err != nil {
		return fail("invalid_integration_instance_name", ExitUsage)
	}
	if err := c.validate(); err != nil {
		return err
	}
	r, err := v.authenticated()
	if err != nil {
		return err
	}
	defer r.close()
	response, err := r.integrations().DeleteIntegrationInstance(v.ctx, bearer(&api.DeleteIntegrationInstanceRequest{Name: c.Name, Etag: c.Etag, RequestId: c.RequestID}, r.credential.Access))
	if err != nil {
		return err
	}
	return v.emitIntegrationOperation(response.Msg)
}

type integrationEnable struct {
	Name                     string `arg:""`
	IntegrationMutationFlags `embed:""`
}

func (c *integrationEnable) Run(v *invocation) error {
	return setIntegrationDesiredState(v, c.Name, c.IntegrationMutationFlags, api.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_ENABLED)
}

type integrationDisable struct {
	Name                     string `arg:""`
	IntegrationMutationFlags `embed:""`
}

func (c *integrationDisable) Run(v *invocation) error {
	return setIntegrationDesiredState(v, c.Name, c.IntegrationMutationFlags, api.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_DISABLED)
}
func setIntegrationDesiredState(v *invocation, name string, flags IntegrationMutationFlags, state api.IntegrationDesiredState) error {
	if _, err := apitypes.ParseIntegrationInstanceName(name); err != nil {
		return fail("invalid_integration_instance_name", ExitUsage)
	}
	if err := flags.validate(); err != nil {
		return err
	}
	r, err := v.authenticated()
	if err != nil {
		return err
	}
	defer r.close()
	response, err := r.integrations().UpdateIntegrationInstance(v.ctx, bearer(&api.UpdateIntegrationInstanceRequest{RequestId: flags.RequestID, IntegrationInstance: &api.IntegrationInstance{Name: name, Etag: flags.Etag, DesiredState: state}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"desired_state"}}}, r.credential.Access))
	if err != nil {
		return err
	}
	return v.emitIntegrationOperation(response.Msg)
}

func (v *invocation) emitIntegrationOperation(operation *longrunningpb.Operation) error {
	if operation == nil {
		return fail("invalid_integration_operation", ExitLocal)
	}
	name, err := apitypes.ParseOperationName(operation.Name)
	if err != nil || contracts.AdmitIntegrationOperation(operation, name) != nil {
		return fail("invalid_integration_operation", ExitLocal)
	}
	if err := v.emit(operation); err != nil {
		return err
	}
	if !operation.Done {
		return fail("mutation_pending", ExitMutationPending)
	}
	if operation.GetError() != nil {
		return fail("mutation_failed", ExitMutationFailed)
	}
	return nil
}

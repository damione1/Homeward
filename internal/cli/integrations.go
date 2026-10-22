package cli

import (
	"encoding/hex"

	"github.com/damione1/Homeward/contracts"
	cfg "github.com/damione1/Homeward/gen/home/configuration/v1"
	wire "github.com/damione1/Homeward/gen/home/integrationadmin/v1"
	"github.com/damione1/Homeward/gen/home/integrationadmin/v1/integrationadminv1connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func (r *remote) integrations() integrationadminv1connect.IntegrationServiceClient {
	return integrationadminv1connect.NewIntegrationServiceClient(r.client, r.base)
}

type integrationCatalogue struct{}

func (*integrationCatalogue) Run(v *invocation) error {
	r, err := v.authenticated()
	if err != nil {
		return err
	}
	defer r.close()
	response, err := r.integrations().ListIntegrationTypes(v.ctx, bearer(&wire.ListIntegrationTypesRequest{}, r.credential.Access))
	if err != nil {
		return err
	}
	return v.emit(response.Msg)
}

type integrationSchema struct {
	Type string `arg:""`
}

func (c *integrationSchema) Run(v *invocation) error {
	r, err := v.authenticated()
	if err != nil {
		return err
	}
	defer r.close()
	response, err := r.integrations().GetIntegrationType(v.ctx, bearer(&wire.GetIntegrationTypeRequest{TypeId: c.Type}, r.credential.Access))
	if err != nil {
		return err
	}
	return v.emit(response.Msg)
}

type integrationList struct{}

func (*integrationList) Run(v *invocation) error {
	r, err := v.authenticated()
	if err != nil {
		return err
	}
	defer r.close()
	response, err := r.integrations().ListInstances(v.ctx, bearer(&wire.ListInstancesRequest{}, r.credential.Access))
	if err != nil {
		return err
	}
	return v.emit(response.Msg)
}

type integrationGet struct {
	ID string `arg:""`
}

func (c *integrationGet) Run(v *invocation) error {
	r, err := v.authenticated()
	if err != nil {
		return err
	}
	defer r.close()
	response, err := r.integrations().GetInstance(v.ctx, bearer(&wire.GetInstanceRequest{IntegrationInstanceId: c.ID}, r.credential.Access))
	if err != nil {
		return err
	}
	return v.emit(response.Msg)
}

type integrationHealth struct {
	ID string `arg:""`
}

func (c *integrationHealth) Run(v *invocation) error {
	r, err := v.authenticated()
	if err != nil {
		return err
	}
	defer r.close()
	response, err := r.integrations().GetInstance(v.ctx, bearer(&wire.GetInstanceRequest{IntegrationInstanceId: c.ID}, r.credential.Access))
	if err != nil {
		return err
	}
	if response.Msg.Instance == nil {
		return fail("invalid_integration_response", ExitLocal)
	}
	instance := response.Msg.Instance
	result := &wire.IntegrationInstance{TypeId: instance.TypeId, IntegrationInstanceId: instance.IntegrationInstanceId, DesiredState: instance.DesiredState, Report: instance.Report, Supervision: instance.Supervision}
	return v.emit(result)
}

// Every mutation uses the exact revision, source digest and physical workspace
// digest the operator observed. There is no implicit reread, retry or CAS bypass.
type MutationFlags struct {
	Revision        int64  `required:"" help:"Expected operational.main revision from integrations get/list."`
	SourceSHA256    string `name:"source-sha256" required:"" help:"Expected exact source SHA256 (64 lowercase hex digits)."`
	WorkspaceSHA256 string `name:"workspace-sha256" required:"" help:"Expected physical workspace SHA256 (64 lowercase hex digits)."`
}

func digest(raw string) ([]byte, error) {
	value, err := hex.DecodeString(raw)
	if err != nil || len(value) != 32 || hex.EncodeToString(value) != raw {
		return nil, fail("invalid_sha256", ExitUsage)
	}
	return value, nil
}
func (m MutationFlags) precondition() (*wire.MutationPrecondition, error) {
	if m.Revision <= 0 {
		return nil, fail("invalid_revision", ExitUsage)
	}
	source, err := digest(m.SourceSHA256)
	if err != nil {
		return nil, err
	}
	workspace, err := digest(m.WorkspaceSHA256)
	if err != nil {
		return nil, err
	}
	return &wire.MutationPrecondition{Expected: &cfg.DocumentVersion{Revision: m.Revision, SourceSha256: source}, ExpectedWorkspaceSha256: workspace}, nil
}
func readConfiguration(path string) (*wire.PublicConfiguration, error) {
	data, err := readRegularInput(path)
	if err != nil {
		return nil, err
	}
	defer clear(data)
	configuration := &wire.PublicConfiguration{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(data, configuration); err != nil {
		return nil, fail("invalid_public_configuration_json", ExitUsage)
	}
	if configuration.SchemaVersion == "" {
		return nil, fail("configuration_schema_version_required", ExitUsage)
	}
	return configuration, nil
}

type integrationCreate struct {
	Type          string `arg:""`
	ID            string `arg:""`
	Config        string `required:"" help:"PublicConfiguration protobuf JSON file; credential fields are secret references only."`
	Enabled       bool   `help:"Initial desired state; defaults to disabled."`
	MutationFlags `embed:""`
}

func (c *integrationCreate) Run(v *invocation) error {
	precondition, err := c.precondition()
	if err != nil {
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
	state := wire.DesiredState_DESIRED_STATE_DISABLED
	if c.Enabled {
		state = wire.DesiredState_DESIRED_STATE_ENABLED
	}
	response, err := r.integrations().CreateInstance(v.ctx, bearer(&wire.CreateInstanceRequest{TypeId: c.Type, IntegrationInstanceId: c.ID, DesiredState: state, Configuration: configuration, Precondition: precondition}, r.credential.Access))
	if err != nil {
		return err
	}
	return v.emitMutation(response.Msg, response.Msg.Activation)
}

type integrationUpdate struct {
	Type          string `arg:""`
	ID            string `arg:""`
	Config        string `required:"" help:"PublicConfiguration protobuf JSON file."`
	MutationFlags `embed:""`
}

func (c *integrationUpdate) Run(v *invocation) error {
	precondition, err := c.precondition()
	if err != nil {
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
	response, err := r.integrations().UpdateInstance(v.ctx, bearer(&wire.UpdateInstanceRequest{TypeId: c.Type, IntegrationInstanceId: c.ID, Configuration: configuration, Precondition: precondition}, r.credential.Access))
	if err != nil {
		return err
	}
	return v.emitMutation(response.Msg, response.Msg.Activation)
}

type integrationDelete struct {
	ID            string `arg:""`
	MutationFlags `embed:""`
}

func (c *integrationDelete) Run(v *invocation) error {
	precondition, err := c.precondition()
	if err != nil {
		return err
	}
	r, err := v.authenticated()
	if err != nil {
		return err
	}
	defer r.close()
	response, err := r.integrations().DeleteInstance(v.ctx, bearer(&wire.DeleteInstanceRequest{IntegrationInstanceId: c.ID, Precondition: precondition}, r.credential.Access))
	if err != nil {
		return err
	}
	return v.emitMutation(response.Msg, response.Msg.Activation)
}

type integrationEnable struct {
	ID            string `arg:""`
	MutationFlags `embed:""`
}

func (c *integrationEnable) Run(v *invocation) error {
	return setEnabled(v, c.ID, c.MutationFlags, wire.DesiredState_DESIRED_STATE_ENABLED)
}

type integrationDisable struct {
	ID            string `arg:""`
	MutationFlags `embed:""`
}

func (c *integrationDisable) Run(v *invocation) error {
	return setEnabled(v, c.ID, c.MutationFlags, wire.DesiredState_DESIRED_STATE_DISABLED)
}
func setEnabled(v *invocation, id string, flags MutationFlags, state wire.DesiredState) error {
	precondition, err := flags.precondition()
	if err != nil {
		return err
	}
	r, err := v.authenticated()
	if err != nil {
		return err
	}
	defer r.close()
	response, err := r.integrations().SetInstanceEnabled(v.ctx, bearer(&wire.SetInstanceEnabledRequest{IntegrationInstanceId: id, DesiredState: state, Precondition: precondition}, r.credential.Access))
	if err != nil {
		return err
	}
	return v.emitMutation(response.Msg, response.Msg.Activation)
}

// Preserve the server's typed result even when the mutation was not published.
// No outcome causes a retry. Admission is owned by the existing wire contract.
func (v *invocation) emitMutation(message proto.Message, activation *cfg.ActivationResult) error {
	if contracts.AdmitIntegrationAdminResponse(message) != nil {
		return fail("invalid_mutation_response", ExitLocal)
	}
	var outcomeError error
	// A published generation may belong to another SQL winner. The requesting
	// transaction's failure/ambiguity takes priority over publication state.
	switch activation.Transaction {
	case cfg.TransactionOutcome_TRANSACTION_OUTCOME_NOT_COMMITTED:
		outcomeError = fail("mutation_not_committed", ExitMutationFailed)
	case cfg.TransactionOutcome_TRANSACTION_OUTCOME_COMMIT_UNKNOWN:
		outcomeError = fail("mutation_commit_unknown_do_not_retry", ExitMutationUnknown)
	case cfg.TransactionOutcome_TRANSACTION_OUTCOME_COMMITTED, cfg.TransactionOutcome_TRANSACTION_OUTCOME_NO_TRANSACTION:
		switch activation.Outcome {
		case cfg.ActivationOutcome_ACTIVATION_OUTCOME_PUBLISHED:
		case cfg.ActivationOutcome_ACTIVATION_OUTCOME_REJECTED_BEFORE_COMMIT, cfg.ActivationOutcome_ACTIVATION_OUTCOME_NOT_COMMITTED:
			outcomeError = fail("mutation_not_committed", ExitMutationFailed)
		case cfg.ActivationOutcome_ACTIVATION_OUTCOME_COMMIT_UNKNOWN:
			outcomeError = fail("mutation_commit_unknown_do_not_retry", ExitMutationUnknown)
		case cfg.ActivationOutcome_ACTIVATION_OUTCOME_COMMITTED_RECONCILING:
			outcomeError = fail("mutation_committed_reconciling", ExitMutationPending)
		case cfg.ActivationOutcome_ACTIVATION_OUTCOME_RESTART_REQUIRED:
			outcomeError = fail("mutation_restart_required", ExitMutationPending)
		default:
			return fail("invalid_mutation_outcome", ExitLocal)
		}
	default:
		return fail("invalid_mutation_transaction", ExitLocal)
	}
	if err := v.emit(message); err != nil {
		return err
	}
	return outcomeError
}

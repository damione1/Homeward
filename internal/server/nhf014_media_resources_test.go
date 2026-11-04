package server

import (
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"context"
	"github.com/damione1/Homeward/gen/google/longrunning/longrunningpbconnect"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNHF014MediaRunSQLLROAndArtworkTLS(t *testing.T) {
	root, err := filepath.Abs("../../qualification/frontend/media")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOMEWARD_NHF014_FIXTURE", filepath.Join(t.TempDir(), "fixture"))
	t.Setenv("HOMEWARD_NHF014_SOURCES", root)
	t.Setenv("HOMEWARD_NHF014_SYNTHETIC_ONLY", "1")
	nhf014RunFixture(t, true)
}
func nhf014SmokeCore(t *testing.T, ctx context.Context, httpClient *http.Client, origin, token, entity string, source *nhf014MediaMock, store *storage.Store, host *integrations.Manager) {
	t.Helper()
	homes := apiv2connect.NewHomeServiceClient(httpClient, origin)
	commands := apiv2connect.NewCapabilityServiceClient(httpClient, origin)
	operations := longrunningpbconnect.NewOperationsClient(httpClient, origin)
	content := apiv2connect.NewMediaServiceClient(httpClient, origin)
	page, err := homes.ListCapabilities(ctx, nha162Bearer(&api.ListCapabilitiesRequest{Parent: entity}, token))
	if err != nil || len(page.Msg.Capabilities) != 1 {
		t.Fatal("real media capability", err)
	}
	capability := page.Msg.Capabilities[0]
	playback := capability.GetMediaPlayback()
	if playback == nil || playback.DataProvenance != api.MediaDataProvenance_MEDIA_DATA_PROVENANCE_SYNTHETIC || playback.Observation == nil || playback.Observation.Route == nil {
		t.Fatal("synthetic playback lacks provenance/route")
	}
	request := &api.ExecuteCapabilityRequest{Name: capability.Name, Etag: capability.Etag, RequestId: "cd44f01c-5bb2-4fc6-bd4c-c9b944b26462", ExpireTime: timestamppb.New(time.Now().Add(20 * time.Second)), Command: &api.ExecuteCapabilityRequest_Media{Media: &api.MediaCommand{Route: proto.CloneOf(playback.Observation.Route), Command: &api.MediaCommand_Operation{Operation: api.MediaOperation_MEDIA_OPERATION_PLAY}}}}
	submitted, err := commands.ExecuteCapability(ctx, nha162Bearer(request, token))
	if err != nil || submitted.Msg.Name == "" {
		t.Fatal("real Run ExecuteCapability", err)
	}
	duplicate, err := commands.ExecuteCapability(ctx, nha162Bearer(proto.CloneOf(request), token))
	if err != nil || duplicate.Msg.Name != submitted.Msg.Name {
		t.Fatal("real SQL original result dedup", err)
	}
	var nativeID domain.CommandID
	var record *nhf014MediaRecord
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		source.mu.Lock()
		if len(source.records) == 1 {
			for id, value := range source.records {
				nativeID, record = id, value
			}
		}
		source.mu.Unlock()
		if record != nil {
			break
		}
		progress, readErr := operations.GetOperation(ctx, nha162Bearer(&longrunningpb.GetOperationRequest{Name: submitted.Msg.Name}, token))
		if readErr != nil {
			t.Fatal("read admitted operation while waiting for driver", readErr)
		}
		if progress.Msg.Done {
			nhf014LogLedger(t, ctx, store, host, progress.Msg)
			t.Fatal("operation terminated before native synthetic effect", progress.Msg)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if record == nil {
		progress, readErr := operations.GetOperation(ctx, nha162Bearer(&longrunningpb.GetOperationRequest{Name: submitted.Msg.Name}, token))
		if readErr == nil {
			nhf014LogLedger(t, ctx, store, host, progress.Msg)
		}
		t.Fatal("native synthetic driver not reached", readErr)
	}
	// Read the standard LRO before confirming the native synthetic effect.
	current, err := operations.GetOperation(ctx, nha162Bearer(&longrunningpb.GetOperationRequest{Name: submitted.Msg.Name}, token))
	if err != nil || current.Msg.Name != submitted.Msg.Name {
		t.Fatal("real Operations read", err)
	}
	acknowledged := false
	for time.Now().Before(deadline) {
		metadata := new(api.CommandMetadata)
		if current.Msg.Metadata != nil && current.Msg.Metadata.UnmarshalTo(metadata) == nil && metadata.Phase == api.CommandPhase_COMMAND_PHASE_ACKNOWLEDGED {
			acknowledged = true
			break
		}
		time.Sleep(10 * time.Millisecond)
		current, err = operations.GetOperation(ctx, nha162Bearer(&longrunningpb.GetOperationRequest{Name: submitted.Msg.Name}, token))
		if err != nil {
			t.Fatal(err)
		}
	}
	if !acknowledged {
		t.Fatal("native tracker acknowledgement missing")
	}
	if err = record.command.ConfirmProtocol(nhf014SyntheticProof{nativeID, record.entity}); err != nil {
		t.Fatal(err)
	}
	for !current.Msg.Done && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		current, err = operations.GetOperation(ctx, nha162Bearer(&longrunningpb.GetOperationRequest{Name: submitted.Msg.Name}, token))
		if err != nil {
			t.Fatal(err)
		}
	}
	if !current.Msg.Done || current.Msg.GetError() != nil || current.Msg.GetResponse() == nil {
		t.Fatal("native confirmation did not finish LRO", current.Msg)
	}
	metadata := new(api.CommandMetadata)
	result := new(api.ExecuteCapabilityResponse)
	if err := current.Msg.GetMetadata().UnmarshalTo(metadata); err != nil {
		t.Fatal("terminal command metadata", err)
	}
	if err := current.Msg.GetResponse().UnmarshalTo(result); err != nil {
		t.Fatal("terminal command result", err)
	}
	if metadata.Phase != api.CommandPhase_COMMAND_PHASE_CONFIRMED || metadata.Capability != capability.Name || metadata.RequestId != request.RequestId || result.Capability != capability.Name || result.Outcome != api.CommandOutcome_COMMAND_OUTCOME_CONFIRMED {
		t.Fatal("native confirmation guarantee or admission identity lost", metadata, result)
	}
	source.mu.Lock()
	mutations, records := source.mutations, len(source.records)
	source.mu.Unlock()
	if mutations != 1 || records != 1 {
		t.Fatal("duplicate native effect", mutations, records)
	}
	artwork := playback.Observation.Artwork
	if artwork == nil || artwork.MediaSource == "" {
		t.Fatal("no exact synthetic artwork source")
	}
	home, err := homes.ListHomes(ctx, nha162Bearer(&api.ListHomesRequest{}, token))
	if err != nil || len(home.Msg.Homes) != 1 {
		t.Fatal(err)
	}
	acquired, err := content.CreateMediaSession(ctx, nha162Bearer(&api.CreateMediaSessionRequest{Parent: home.Msg.Homes[0].Name, MediaSession: &api.MediaSession{MediaSource: artwork.MediaSource, ContentKind: api.MediaContentKind_MEDIA_CONTENT_KIND_ARTWORK, ArtworkReference: artwork.Reference}, RequestId: "cd44f01c-5bb2-4fc6-bd4c-c9b944b26463"}, token))
	if err != nil {
		t.Fatal("sole SQL artwork owner", err)
	}
	image, err := content.ReadMediaSession(ctx, nha162Bearer(nha202SnapshotRequest(acquired.Msg.Name), token))
	if err != nil || len(image.Msg.GetImage().GetPng()) == 0 {
		t.Fatal("synthetic artwork read", err)
	}
	if _, err = content.DeleteMediaSession(ctx, nha162Bearer(&api.DeleteMediaSessionRequest{Name: acquired.Msg.Name, RequestId: "cd44f01c-5bb2-4fc6-bd4c-c9b944b26464"}, token)); err != nil {
		t.Fatal(err)
	}
}

// Failure diagnostics read the same Run-owned ledger. They never dispatch,
// retry, alter admission or expose a public diagnostic endpoint.
func nhf014LogLedger(t *testing.T, ctx context.Context, store *storage.Store, host *integrations.Manager, operation *longrunningpb.Operation) {
	t.Helper()
	metadata := new(api.CommandMetadata)
	if err := operation.GetMetadata().UnmarshalTo(metadata); err != nil {
		t.Log("operation metadata decode", err)
	} else {
		t.Logf("operation phase=%s live=%v done=%v error=%v", metadata.Phase, metadata.LiveProgressAvailable, operation.Done, operation.GetError())
	}
	name, err := apitypes.ParseOperationName(operation.Name)
	if err != nil {
		t.Log("operation name", err)
		return
	}
	id, err := domain.ParseCommandID(strings.TrimPrefix(name.ID(), "cmd-"))
	if err != nil {
		t.Log("command identity", err)
		return
	}
	row, found, err := store.CommandIntent(ctx, id)
	if err != nil || !found {
		t.Log("ledger row", found, err)
		return
	}
	response, terminal := new(actions.CommandResponse), new(actions.CommandResponse)
	responseErr := proto.Unmarshal(row.Response(), response)
	terminalErr := proto.Unmarshal(row.APITerminalResponse(), terminal)
	t.Logf("ledger state=%v error_kind=%v driver_entries=%d response=%v response_decode=%v terminal=%v terminal_decode=%v", row.State(), row.ErrorKind(), host.CommandDriverEntries(), response, responseErr, terminal, terminalErr)
}

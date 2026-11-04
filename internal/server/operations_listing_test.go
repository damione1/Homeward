package server

import (
	"context"
	"fmt"
	"google.golang.org/protobuf/proto"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type operationListFixture struct {
	names        []apitypes.OperationName
	hidden       map[string]bool
	cutoff       time.Time
	payloadBytes int
}

func (p *operationListFixture) GetOperation(_ context.Context, name apitypes.OperationName) (*longrunningpb.Operation, error) {
	metadata, _ := anypb.New(&api.CommandMetadata{Capability: name.Home().String() + "/entities/example/capabilities/power", Phase: api.CommandPhase_COMMAND_PHASE_ACCEPTED, CreateTime: timestamppb.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))})
	value := &longrunningpb.Operation{Name: name.String(), Metadata: metadata}
	if p.payloadBytes > 0 {
		// The listing layer accepts an owner-admitted opaque result envelope;
		// it must bound that payload independently of domain-specific codecs.
		value.Done = true
		value.Result = &longrunningpb.Operation_Response{Response: &anypb.Any{TypeUrl: "type.googleapis.com/synthetic.OwnerResult", Value: make([]byte, p.payloadBytes)}}
	}
	return value, nil
}
func (p *operationListFixture) ListOperationNames(_ context.Context, _ apitypes.HomeName, after *apitypes.OperationName, size uint32, cutoff time.Time) ([]apitypes.OperationName, *apitypes.OperationName, error) {
	p.cutoff = cutoff
	start := 0
	if after != nil {
		for start < len(p.names) && p.names[start].String() <= after.String() {
			start++
		}
	}
	end := min(start+int(size), len(p.names))
	var out []apitypes.OperationName
	for _, name := range p.names[start:end] {
		if !p.hidden[name.String()] {
			out = append(out, name)
		}
	}
	if end < len(p.names) {
		next := p.names[end-1]
		return out, &next, nil
	}
	return out, nil, nil
}
func (p *operationListFixture) FindOperationsByRequest(_ context.Context, _ apitypes.HomeName, _ apitypes.RequestID, cutoff time.Time) ([]apitypes.OperationName, error) {
	p.cutoff = cutoff
	return p.names, nil
}

func TestNHA201OperationUnionPaginatesEmptyAuthorizedPagesAndBindsRights(t *testing.T) {
	f := nha165GrantSetup(t)
	ctx := context.WithValue(f.ctx, actorContextKey{}, f.actor)
	name := func(id string) apitypes.OperationName {
		value, err := apitypes.ParseOperationName("homes/synthetic/operations/" + id)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	physical := &operationListFixture{names: []apitypes.OperationName{name("cmd-a"), name("cmd-b")}, hidden: map[string]bool{name("cmd-a").String(): true}}
	configuration := &operationListFixture{names: []apitypes.OperationName{name("config-a")}}
	handler, err := newOperationRPCHandler(physical, configuration, f.a.authority)
	if err != nil {
		t.Fatal(err)
	}
	first, err := handler.ListOperations(ctx, connect.NewRequest(&longrunningpb.ListOperationsRequest{Name: "homes/synthetic", PageSize: 1}))
	if err != nil || len(first.Msg.Operations) != 0 || first.Msg.NextPageToken == "" {
		t.Fatal("empty authorized page lost scanned continuation", first, err)
	}
	cutoff := physical.cutoff
	second, err := handler.ListOperations(ctx, connect.NewRequest(&longrunningpb.ListOperationsRequest{Name: "homes/synthetic", PageSize: 2, PageToken: first.Msg.NextPageToken}))
	if err != nil || len(second.Msg.Operations) != 2 || second.Msg.NextPageToken != "" {
		t.Fatal("union or variable page size", second, err)
	}
	if second.Msg.Operations[0].Name != name("cmd-b").String() || second.Msg.Operations[1].Name != name("config-a").String() || !configuration.cutoff.Equal(cutoff) || !physical.cutoff.Equal(cutoff) {
		t.Fatal("order or creation bound changed")
	}
	forged := first.Msg.NextPageToken[:len(first.Msg.NextPageToken)-2] + "xx"
	if _, err := handler.ListOperations(ctx, connect.NewRequest(&longrunningpb.ListOperationsRequest{Name: "homes/synthetic", PageToken: forged})); err == nil {
		t.Fatal("forged token accepted")
	}
	if _, err := handler.ListOperations(ctx, connect.NewRequest(&longrunningpb.ListOperationsRequest{Name: "homes/other", PageToken: first.Msg.NextPageToken})); err == nil {
		t.Fatal("transferred parent token accepted")
	}
	if err := f.a.authority.Publish(nha164AuthorityState(t, f.source, auth.AccountRole_ACCOUNT_ROLE_USER, 2)); err != nil {
		t.Fatal(err)
	}
	if _, err := handler.ListOperations(ctx, connect.NewRequest(&longrunningpb.ListOperationsRequest{Name: "homes/synthetic", PageToken: first.Msg.NextPageToken})); err == nil {
		t.Fatal("old rights token accepted")
	}
}

func TestNHA201OperationRequestFilterIsUUIDOnly(t *testing.T) {
	for _, text := range []string{`metadata.request_id = "12345678-1234-4123-8123-123456789ABC"`, `metadata.request_id="12345678-1234-4123-8123-123456789abc"`} {
		key, normalized, err := operationRequestFilter(text)
		if err != nil || key == nil || strings.Contains(normalized, "ABC") {
			t.Fatal(key, normalized, err)
		}
	}
	for _, text := range []string{`metadata.request_id="*"`, `metadata.request_id="12345678-1234-4123-8123-123456789abc" OR true`, `actor="other"`, `request_id: "12345678-1234-4123-8123-123456789abc"`} {
		if _, _, err := operationRequestFilter(text); err == nil {
			t.Fatal("unsupported filter accepted", text)
		}
	}
}

func TestNHA201OperationPagesBoundBytesWithoutSkippingCandidate(t *testing.T) {
	for _, payload := range []int{900 << 10, (1 << 20) - 512} {
		for _, filter := range []string{"", `metadata.request_id="12345678-1234-4123-8123-123456789abc"`} {
			t.Run(fmt.Sprintf("bytes%d/filter%t", payload, filter != ""), func(t *testing.T) {
				f := nha165GrantSetup(t)
				ctx := context.WithValue(f.ctx, actorContextKey{}, f.actor)
				physical := &operationListFixture{payloadBytes: payload}
				configuration := &operationListFixture{payloadBytes: payload}
				expected := map[string]bool{}
				for _, provider := range []*operationListFixture{physical, configuration} {
					prefix := "cmd"
					if provider == configuration {
						prefix = "config"
					}
					for i := 0; i < 5; i++ {
						name, err := apitypes.ParseOperationName(fmt.Sprintf("homes/synthetic/operations/%s-%d", prefix, i))
						if err != nil {
							t.Fatal(err)
						}
						provider.names = append(provider.names, name)
						expected[name.String()] = true
					}
				}
				handler, err := newOperationRPCHandler(physical, configuration, f.a.authority)
				if err != nil {
					t.Fatal(err)
				}
				token := ""
				seen := map[string]bool{}
				for page := 0; page < 20; page++ {
					size := int32(500)
					if page%2 == 1 {
						size = 1
					}
					response, err := handler.ListOperations(ctx, connect.NewRequest(&longrunningpb.ListOperationsRequest{Name: "homes/synthetic", Filter: filter, PageSize: size, PageToken: token}))
					if err != nil {
						t.Fatal(err)
					}
					if proto.Size(response.Msg) > maxOperationListBytes {
						t.Fatal("response exceeded transport byte budget")
					}
					for _, value := range response.Msg.Operations {
						if !expected[value.Name] || seen[value.Name] {
							t.Fatal("foreign or repeated operation", value.Name)
						}
						seen[value.Name] = true
					}
					token = response.Msg.NextPageToken
					if token == "" {
						if len(seen) != len(expected) {
							t.Fatalf("lost overflow candidate: got%d want%d", len(seen), len(expected))
						}
						return
					}
					if len(response.Msg.Operations) == 0 {
						t.Fatal("valid individual operation failed to fit")
					}
				}
				t.Fatal("byte-bound continuation failed to make progress")
			})
		}
	}
}

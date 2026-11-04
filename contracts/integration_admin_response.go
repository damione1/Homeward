package contracts

import (
	"strings"
	"unicode/utf8"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
	"google.golang.org/protobuf/proto"
)

func AdmitIntegrationAdminResponse(message proto.Message) error {
	if message == nil || !message.ProtoReflect().IsValid() || !configurationKnown(message.ProtoReflect()) || proto.Size(message) > 1<<20 {
		return ErrInvalidIntegrationAdminContract
	}
	valid := false
	switch m := message.(type) {
	case *api.ListIntegrationTypesResponse:
		valid = m != nil && len(m.IntegrationTypes) <= 64 && len(m.NextPageToken) <= 2048 && utf8.ValidString(m.NextPageToken) && !strings.ContainsAny(m.NextPageToken, "\x00\r\n")
		if valid {
			seenNames := map[string]bool{}
			seenKeys := map[string]bool{}
			var home apitypes.HomeName
			for i, t := range m.IntegrationTypes {
				if t == nil {
					valid = false
					break
				}
				name, err := apitypes.ParseIntegrationTypeName(t.Name)
				if err != nil || (i > 0 && name.Home() != home) || seenNames[t.Name] || seenKeys[t.TypeKey] || AdmitIntegrationType(t, name.Home()) != nil {
					valid = false
					break
				}
				home = name.Home()
				seenNames[t.Name] = true
				seenKeys[t.TypeKey] = true
			}
		}
	case *api.IntegrationType:
		if m != nil {
			if name, err := apitypes.ParseIntegrationTypeName(m.Name); err == nil {
				valid = AdmitIntegrationType(m, name.Home()) == nil
			}
		}
	case *api.ListIntegrationInstancesResponse:
		valid = m != nil && len(m.IntegrationInstances) <= 1024 && len(m.NextPageToken) <= 2048 && utf8.ValidString(m.NextPageToken) && !strings.ContainsAny(m.NextPageToken, "\x00\r\n")
		if valid {
			seenNames := map[string]bool{}
			seenKeys := map[string]bool{}
			var home apitypes.HomeName
			for i, inst := range m.IntegrationInstances {
				if inst == nil {
					valid = false
					break
				}
				name, err := apitypes.ParseIntegrationInstanceName(inst.Name)
				if err != nil || (i > 0 && name.Home() != home) || seenNames[inst.Name] || seenKeys[inst.InstanceKey] || AdmitIntegrationInstance(inst, name.Home()) != nil {
					valid = false
					break
				}
				home = name.Home()
				seenNames[inst.Name] = true
				seenKeys[inst.InstanceKey] = true
			}
		}
	case *api.IntegrationInstance:
		if m != nil {
			if name, err := apitypes.ParseIntegrationInstanceName(m.Name); err == nil {
				valid = AdmitIntegrationInstance(m, name.Home()) == nil
			}
		}
	case *longrunningpb.Operation:
		if m != nil {
			if name, err := apitypes.ParseOperationName(m.Name); err == nil {
				valid = AdmitIntegrationOperation(m, name) == nil
			}
		}
	}
	if !valid {
		return ErrInvalidIntegrationAdminContract
	}
	return nil
}

package contracts

import (
	"errors"
	"strings"
	"unicode/utf8"

	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

var ErrInvalidConfigurationContract = errors.New("invalid configuration contract")

func configurationKnown(m protoreflect.Message) bool {
	if !m.IsValid() || len(m.GetUnknown()) != 0 {
		return false
	}
	valid := true
	m.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.Message() == nil {
			return true
		}
		if field.IsList() {
			list := value.List()
			for i := 0; i < list.Len(); i++ {
				if !configurationKnown(list.Get(i).Message()) {
					valid = false
					return false
				}
			}
		} else if !configurationKnown(value.Message()) {
			valid = false
			return false
		}
		return true
	})
	return valid
}
func configurationHash(raw []byte) bool {
	if len(raw) != 32 {
		return false
	}
	var any byte
	for _, b := range raw {
		any |= b
	}
	return any != 0
}
func configurationID(id *wire.DocumentId) bool {
	return id != nil && len(id.Value) > 0 && len(id.Value) <= 255 && utf8.ValidString(id.Value) && strings.TrimSpace(id.Value) != "" && !strings.ContainsAny(id.Value, "\x00\r\n")
}
func configurationVersion(v *wire.DocumentVersion) bool {
	return v != nil && v.Revision > 0 && configurationHash(v.SourceSha256)
}
func configurationGeneration(g *wire.ActiveGeneration) bool {
	return g != nil && g.Revision > 0 && configurationHash(g.ManifestSha256)
}
func configurationPage(size int32, token string) bool { return size >= 0 && len(token) <= 1200 }

// AdmitConfigurationRequest is operational admission, not merely successful
// Protobuf decoding. Future enum/unknown fields and Go zero requests fail closed.
func AdmitConfigurationRequest(message proto.Message) error {
	if message == nil || !configurationKnown(message.ProtoReflect()) {
		return ErrInvalidConfigurationContract
	}
	valid := false
	switch value := message.(type) {
	case *wire.GetConfigurationRevisionRequest:
		if value != nil {
			_, err := apitypes.ParseConfigurationRevisionName(value.Name)
			valid = err == nil
		}
	case *wire.GetConfigurationGenerationRequest:
		if value != nil {
			_, err := apitypes.ParseConfigurationGenerationName(value.Name)
			valid = err == nil
		}
	case *wire.GetConfigurationSchemaRequest:
		if value != nil {
			_, err := apitypes.ParseConfigurationSchemaName(value.Name)
			valid = err == nil
		}
	case *wire.GetConfigurationDocumentRequest:
		_, err := ParseGetConfigurationDocument(value)
		valid = err == nil
	case *wire.ListConfigurationDocumentsRequest:
		valid = value != nil && configurationPage(value.PageSize, value.PageToken) && configurationHomeName(value.Parent)
	case *wire.ListConfigurationRevisionsRequest:
		valid = value != nil && configurationResourceName(value.Parent, "configurationDocuments") && configurationPage(value.PageSize, value.PageToken)
	case *wire.ListConfigurationGenerationsRequest:
		valid = value != nil && configurationPage(value.PageSize, value.PageToken) && configurationHomeName(value.Parent)
	case *wire.ListConfigurationSchemasRequest:
		valid = value != nil && configurationPage(value.PageSize, value.PageToken) && configurationHomeName(value.Parent)
	case *wire.PublishConfigurationDocumentRequest:
		valid = value != nil && configurationResourceName(value.Name, "configurationDocuments") && configurationVersion(value.Expected) && configurationHash(value.ExpectedWorkspaceSha256) && len(value.ExactYamlSource) >= 1 && len(value.ExactYamlSource) <= 1<<20
	case *wire.RollbackConfigurationDocumentRequest:
		valid = value != nil && configurationResourceName(value.Name, "configurationDocuments") && configurationVersion(value.Expected) && configurationHash(value.ExpectedWorkspaceSha256) && configurationGeneration(value.Historical)
	case *wire.WatchConfigurationRequest:
		valid = value != nil && configurationHomeName(value.Parent) && len(value.ResumeToken) == 118
	default:
		return ErrInvalidConfigurationContract
	}
	if !valid {
		return ErrInvalidConfigurationContract
	}
	return nil
}

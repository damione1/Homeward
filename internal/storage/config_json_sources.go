package storage

import "github.com/damione1/Homeward/compat/ha/parser"

// NewJSONConfigDocumentBinding is reserved for native JSON source owners. A
// binding is a logical identity and path, never a filesystem grant.
func NewJSONConfigDocumentBinding(id ConfigDocumentID, path string) (ConfigDocumentBinding, error) {
	if !id.Valid() {
		return ConfigDocumentBinding{}, &Error{kind: ConfigInvalid}
	}
	if _, err := parser.NewJSONSourceFile(path, nil); err != nil {
		return ConfigDocumentBinding{}, &Error{kind: ConfigInvalid}
	}
	return ConfigDocumentBinding{id: id, path: path}, nil
}

func configSourcePath(kind, path string) error {
	if kind == ConfigDocumentKindOwnershipSource.String() {
		_, err := parser.NewJSONSourceFile(path, nil)
		return err
	}
	_, err := parser.NewSourceFile(path, nil)
	return err
}

func editableSourceSnapshot(kind ConfigDocumentKind, path string, raw []byte) (parser.SourceFile, error) {
	if kind == ConfigDocumentKindOwnershipSource {
		return parser.NewJSONSourceFile(path, raw)
	}
	return parser.NewSourceFile(path, raw)
}

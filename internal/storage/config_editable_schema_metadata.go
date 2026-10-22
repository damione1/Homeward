package storage

// configPublicSchemaEntries expands an installed SQL schema label into the
// closed admitted source formats. Automation and script intentionally share
// ha-logic-1, while retaining distinct document kinds. No source descriptor,
// identity, capability or SQL enum value is inferred from client input.
func configPublicSchemaEntries(schema string) ([]ConfigSchemaMetadata, bool) {
	entries := make([]ConfigSchemaMetadata, 0, 2)
	for _, format := range []configSourceFormat{configSourceDashboard, configSourceAutomation, configSourceScript, configSourceScene, configSourceOwnership} {
		kind, admittedSchema, profile, err := editableFormatValues(format)
		if err != nil {
			return nil, false
		}
		if schema == admittedSchema {
			entries = append(entries, ConfigSchemaMetadata{kind: kind, schema: admittedSchema, profile: profile})
		}
	}
	if len(entries) > 0 {
		return entries, true
	}
	entry, known := configPublicSchemaMetadata(schema)
	if !known {
		return nil, false
	}
	return []ConfigSchemaMetadata{entry}, true
}

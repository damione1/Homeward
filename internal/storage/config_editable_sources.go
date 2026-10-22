package storage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"sort"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/jackc/pgx/v5"
)

// These extend the existing closed SQL kinds and source-format conversion.
// The numbers continue the delivered kinds/formats; no parallel string registry.
const (
	ConfigDocumentKindDashboardSource  ConfigDocumentKind = 6
	ConfigDocumentKindAutomationSource ConfigDocumentKind = 7
	ConfigDocumentKindScriptSource     ConfigDocumentKind = 8
	ConfigDocumentKindSceneSource      ConfigDocumentKind = 9
	ConfigDocumentKindOwnershipSource  ConfigDocumentKind = 10

	configSourceDashboard  configSourceFormat = 6
	configSourceAutomation configSourceFormat = 7
	configSourceScript     configSourceFormat = 8
	configSourceScene      configSourceFormat = 9
	configSourceOwnership  configSourceFormat = 10
)

func editableKindText(kind ConfigDocumentKind) string {
	switch kind {
	case ConfigDocumentKindDashboardSource:
		return "dashboard_source"
	case ConfigDocumentKindAutomationSource:
		return "automation_source"
	case ConfigDocumentKindScriptSource:
		return "script_source"
	case ConfigDocumentKindSceneSource:
		return "scene_source"
	case ConfigDocumentKindOwnershipSource:
		return "ownership_source"
	default:
		return ""
	}
}

func parseEditableKind(text string) (ConfigDocumentKind, error) {
	for _, kind := range []ConfigDocumentKind{ConfigDocumentKindDashboardSource, ConfigDocumentKindAutomationSource, ConfigDocumentKindScriptSource, ConfigDocumentKindSceneSource, ConfigDocumentKindOwnershipSource} {
		if text == editableKindText(kind) {
			return kind, nil
		}
	}
	return ConfigDocumentKindInvalid, &Error{kind: ConfigIntegrity}
}

func editableFormatValues(format configSourceFormat) (ConfigDocumentKind, string, string, error) {
	switch format {
	case configSourceDashboard:
		return ConfigDocumentKindDashboardSource, "homeward-dashboard-1", parser.LovelaceProfile, nil
	case configSourceAutomation:
		return ConfigDocumentKindAutomationSource, "ha-logic-1", parser.LogicProfile, nil
	case configSourceScript:
		return ConfigDocumentKindScriptSource, "ha-logic-1", parser.LogicProfile, nil
	case configSourceScene:
		return ConfigDocumentKindSceneSource, "ha-scene-1", parser.SceneProfile, nil
	case configSourceOwnership:
		return ConfigDocumentKindOwnershipSource, "homeward.heating-ownership/1", "homeward.heating-ownership-json/1", nil
	default:
		return ConfigDocumentKindInvalid, "", "", &Error{kind: ConfigInvalid}
	}
}

func editableSourceFormat(kind, schema, profile string) bool {
	for _, format := range []configSourceFormat{configSourceDashboard, configSourceAutomation, configSourceScript, configSourceScene, configSourceOwnership} {
		admittedKind, admittedSchema, admittedProfile, _ := editableFormatValues(format)
		if kind == editableKindText(admittedKind) && schema == admittedSchema && profile == admittedProfile {
			return true
		}
	}
	return false
}

// SourceDescriptor supplies the missing grouping/entry metadata for exact
// source files. It does not certify operational semantics; each configuration
// owner must compile the corresponding complete source set before publication.
type SourceDescriptor struct {
	format          configSourceFormat
	identity, entry string
}

func newSourceDescriptor(format configSourceFormat, identity, entry string) (SourceDescriptor, error) {
	if !configLabel(identity, 255) {
		return SourceDescriptor{}, &Error{kind: ConfigInvalid}
	}
	kind, _, _, formatErr := format.values()
	if formatErr != nil {
		return SourceDescriptor{}, &Error{kind: ConfigInvalid}
	}
	if err := configSourcePath(kind.String(), entry); err != nil {
		return SourceDescriptor{}, &Error{kind: ConfigInvalid}
	}
	switch format {
	case configSourceDashboard, configSourceAutomation, configSourceScript, configSourceScene, configSourceOwnership:
		return SourceDescriptor{format: format, identity: identity, entry: entry}, nil
	default:
		return SourceDescriptor{}, &Error{kind: ConfigInvalid}
	}
}

func DashboardSourceDescriptor(identity, entry string) (SourceDescriptor, error) {
	return newSourceDescriptor(configSourceDashboard, identity, entry)
}
func AutomationSourceDescriptor(identity, entry string) (SourceDescriptor, error) {
	return newSourceDescriptor(configSourceAutomation, identity, entry)
}
func ScriptSourceDescriptor(identity, entry string) (SourceDescriptor, error) {
	return newSourceDescriptor(configSourceScript, identity, entry)
}
func SceneSourceDescriptor(identity, entry string) (SourceDescriptor, error) {
	return newSourceDescriptor(configSourceScene, identity, entry)
}
func OwnershipSourceDescriptor(identity, entry string) (SourceDescriptor, error) {
	return newSourceDescriptor(configSourceOwnership, identity, entry)
}
func (d SourceDescriptor) Valid() bool {
	_, err := newSourceDescriptor(d.format, d.identity, d.entry)
	return err == nil
}
func (d SourceDescriptor) Identity() string { return d.identity }
func (d SourceDescriptor) Entry() string    { return d.entry }
func (d SourceDescriptor) Kind() ConfigDocumentKind {
	kind, _, _, err := d.format.values()
	if err != nil || !d.Valid() {
		return ConfigDocumentKindInvalid
	}
	return kind
}
func (d SourceDescriptor) Schema() string {
	_, schema, _, err := d.format.values()
	if err != nil || !d.Valid() {
		return ""
	}
	return schema
}

// SaveEditableSource adds a version through the existing immutable-source
// transaction. Its closed descriptor and exact source hash are storage
// admission, independently of the semantic compiler required by 029.
func (c *ConfigTx) SaveEditableSource(descriptor SourceDescriptor, binding ConfigDocumentBinding, file parser.SourceFile, metadata ConfigVersionMetadata) (ref ConfigVersionRef, err error) {
	defer func() { c.remember(err) }()
	if err := c.valid(); err != nil {
		return ConfigVersionRef{}, err
	}
	if !descriptor.Valid() || !binding.ID().Valid() || binding.Path() != file.Path() || len(file.Bytes()) < 1 || sha256.Sum256(file.Bytes()) != file.SHA256() {
		return ConfigVersionRef{}, &Error{kind: ConfigInvalid}
	}
	kind, _, _, err := descriptor.format.values()
	if err != nil {
		return ConfigVersionRef{}, err
	}
	if _, err := c.tx.Exec(c.ctx, `INSERT INTO public.config_documents(id,logical_path,kind,source_identity,source_entry)
		VALUES($1,$2,$3::public.config_document_kind,$4,$5) ON CONFLICT(id) DO NOTHING`, binding.ID().String(), binding.Path(), kind.String(), descriptor.identity, descriptor.entry); err != nil {
		return ConfigVersionRef{}, configWriteError(err)
	}
	var identity, entry *string
	if err := c.tx.QueryRow(c.ctx, `SELECT source_identity,source_entry FROM public.config_documents WHERE id=$1`, binding.ID().String()).Scan(&identity, &entry); err != nil {
		return ConfigVersionRef{}, err
	}
	if identity == nil || entry == nil || *identity != descriptor.identity || *entry != descriptor.entry {
		return ConfigVersionRef{}, &Error{kind: ConfigConflict}
	}
	return c.saveAdmittedSource(binding, descriptor.format, file.Bytes(), file.SHA256(), metadata)
}

// EditableSourceSnapshot remains redacted by default; source bytes are only
// available to trusted compilation/replay, never generic history responses.
type EditableSourceSnapshot struct {
	descriptor SourceDescriptor
	binding    ConfigDocumentBinding
	ref        ConfigVersionRef
	file       parser.SourceFile
}

func (s EditableSourceSnapshot) Descriptor() SourceDescriptor   { return s.descriptor }
func (s EditableSourceSnapshot) Binding() ConfigDocumentBinding { return s.binding }
func (s EditableSourceSnapshot) Ref() ConfigVersionRef          { return s.ref }
func (s EditableSourceSnapshot) File() parser.SourceFile        { return s.file }
func (s EditableSourceSnapshot) String() string                 { return "[REDACTED EDITABLE CONFIG SOURCE]" }
func (s EditableSourceSnapshot) GoString() string               { return s.String() }
func (s EditableSourceSnapshot) MarshalJSON() ([]byte, error)   { return json.Marshal(s.String()) }

func editableDescriptor(kind ConfigDocumentKind, identity, entry string) (SourceDescriptor, error) {
	switch kind {
	case ConfigDocumentKindDashboardSource:
		return DashboardSourceDescriptor(identity, entry)
	case ConfigDocumentKindAutomationSource:
		return AutomationSourceDescriptor(identity, entry)
	case ConfigDocumentKindScriptSource:
		return ScriptSourceDescriptor(identity, entry)
	case ConfigDocumentKindSceneSource:
		return SceneSourceDescriptor(identity, entry)
	case ConfigDocumentKindOwnershipSource:
		return OwnershipSourceDescriptor(identity, entry)
	default:
		return SourceDescriptor{}, &Error{kind: ConfigInvalid}
	}
}

// ReadEditableSources reads one complete sealed generation under the same
// repeatable SQL snapshot as its source bytes and grouping metadata.
func (s *Store) ReadEditableSources(ctx context.Context, revision ConfigGenerationRevision, manifest [32]byte) ([]EditableSourceSnapshot, error) {
	if err := bootstrapReadContextError(ctx); err != nil {
		return nil, err
	}
	if s == nil || s.pool == nil || !revision.Valid() || manifest == ([32]byte{}) {
		return nil, &Error{kind: ConfigInvalid}
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, &Error{kind: Unavailable}
	}
	defer rollback(tx)
	generation, err := readConfigGenerationTx(ctx, tx, revision)
	if err != nil {
		return nil, err
	}
	if generation.ManifestSHA256() != manifest {
		return nil, &Error{kind: ConfigConflict}
	}
	if generation.Availability() != ConfigGenerationComplete {
		return nil, &Error{kind: ConfigIncomplete}
	}
	rows, err := tx.Query(ctx, `SELECT d.id,d.logical_path,d.kind::text,d.source_identity,d.source_entry,
		v.revision,v.source_hash,v.yaml_source,v.schema_version::text,v.syntax_profile::text
		FROM public.config_generation_entries e JOIN public.config_documents d ON d.id=e.document_id
		JOIN public.config_versions v ON v.document_id=e.document_id AND v.revision=e.version_revision
		WHERE e.generation_revision=$1 AND d.kind::text IN ('dashboard_source','automation_source','script_source','scene_source','ownership_source') ORDER BY d.id`, revision.Number())
	if err != nil {
		return nil, &Error{kind: Unavailable}
	}
	var result []EditableSourceSnapshot
	total := 0
	for rows.Next() {
		var document, logical, kindText, schema, profile string
		var identity, entry *string
		var version int64
		var hash, raw []byte
		if err := rows.Scan(&document, &logical, &kindText, &identity, &entry, &version, &hash, &raw, &schema, &profile); err != nil {
			rows.Close()
			return nil, &Error{kind: ConfigIntegrity}
		}
		total += len(raw)
		if len(result) >= 256 || total > 4<<20 || len(raw) < 1 || len(raw) > 1<<20 || len(hash) != 32 || identity == nil || entry == nil {
			rows.Close()
			return nil, &Error{kind: ConfigIntegrity}
		}
		kind, err := parseConfigDocumentKind(kindText)
		if err != nil {
			rows.Close()
			return nil, err
		}
		descriptor, err := editableDescriptor(kind, *identity, *entry)
		_, expectedSchema, expectedProfile, formatErr := descriptor.format.values()
		if err != nil || formatErr != nil || schema != expectedSchema || profile != expectedProfile {
			rows.Close()
			return nil, &Error{kind: ConfigIntegrity}
		}
		id, err := NewConfigDocumentID(document)
		if err != nil {
			rows.Close()
			return nil, &Error{kind: ConfigIntegrity}
		}
		binding, err := NewConfigDocumentBinding(id, logical)
		if kind == ConfigDocumentKindOwnershipSource {
			binding, err = NewJSONConfigDocumentBinding(id, logical)
		}
		if err != nil {
			rows.Close()
			return nil, &Error{kind: ConfigIntegrity}
		}
		ref := ConfigVersionRef{document: id, revision: ConfigVersionRevision{version: version}}
		file, err := editableSourceSnapshot(kind, logical, raw)
		var expected [32]byte
		copy(expected[:], hash)
		if err != nil || !ref.Valid() || file.SHA256() != expected {
			rows.Close()
			return nil, &Error{kind: ConfigIntegrity}
		}
		result = append(result, EditableSourceSnapshot{descriptor: descriptor, binding: binding, ref: ref, file: file})
	}
	rows.Close()
	if rows.Err() != nil {
		return nil, &Error{kind: Unavailable}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, &Error{kind: Unavailable}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].binding.ID().String() < result[j].binding.ID().String() })
	return result, nil
}

package configdocuments

import (
	"context"
	"errors"
	"time"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/storage"
)

// ExpectedSource admits SQL version and physical observation independently.
// A retained SQL source may legitimately not have been materialized in the
// editable workspace yet. That is distinct from absence in SQL.
type ExpectedSource struct {
	binding   storage.ConfigDocumentBinding
	revision  storage.ConfigVersionRevision
	sql       MemberExpectation
	workspace MemberExpectation
}

func NewExpectedSource(binding storage.ConfigDocumentBinding, revision storage.ConfigVersionRevision, source [32]byte, absent bool, workspace MemberExpectation) (ExpectedSource, error) {
	if !binding.ID().Valid() || binding.Path() == "" || !workspace.valid() || absent && (revision.Valid() || source != ([32]byte{})) || !absent && (!revision.Valid() || source == ([32]byte{})) {
		return ExpectedSource{}, &Error{kind: FailureInvalid}
	}
	sql := ExpectAbsentMember()
	if !absent {
		sql, _ = ExpectExistingMember(source)
	}
	return ExpectedSource{binding: binding, revision: revision, sql: sql, workspace: workspace}, nil
}

func (e ExpectedSource) Binding() storage.ConfigDocumentBinding { return e.binding }

func (e ExpectedSource) confirmKind(ctx context.Context, store *storage.Store, kind storage.ConfigDocumentKind) error {
	if err := store.CheckSourceBinding(ctx, e.binding, kind, e.revision, e.sql.hash, e.sql.absent); err != nil {
		return &Error{kind: Conflict}
	}
	return nil
}

// WriteDefinitions composes the existing declarative compiler and 029
// ReplaceDefinitions with the same 149 writer/intent. All includes enter one
// generation; there is no per-file activation, second registry or SQL writer.
func (s *Service) WriteDefinitions(ctx context.Context, group storage.ConfigDocumentID, candidate bundle.Candidate, expectedOperational ExpectedDocument, expected []ExpectedSource, author storage.ConfigAuthor, basis WriteBasis) (configactivation.Result, error) {
	if !candidate.Valid() {
		return configactivation.Result{}, &Error{kind: FailureInvalid}
	}
	bindings := make([]storage.ConfigDocumentBinding, 0, len(expected))
	for _, version := range expected {
		bindings = append(bindings, version.binding)
	}
	choice, err := configactivation.ReplaceWorkspaceDefinitions(group, candidate, bindings)
	if err != nil {
		return configactivation.Result{}, err
	}
	return s.writeSourceSet(ctx, group, candidate.Sources(), choice, nil, configactivation.SourceCompilation{}, storage.ConfigDocumentKindYAMLSource, expectedOperational, expected, author, basis, nil)
}

func (s *Service) WriteSourceGroup(ctx context.Context, groupID storage.ConfigDocumentID, group configactivation.SourceGroup, expectedOperational ExpectedDocument, expected []ExpectedSource, author storage.ConfigAuthor, basis WriteBasis) (configactivation.Result, error) {
	expectedGroup, groupErr := SourceWorkspaceGroup(group.Descriptor())
	if s == nil || !s.admitted || s.owner == nil || groupErr != nil || groupID != expectedGroup {
		return configactivation.Result{}, &Error{kind: FailureInvalid}
	}
	compilation, err := s.owner.CompileEditableSourceGroup(ctx, group)
	if err != nil {
		return configactivation.Result{}, err
	}
	return s.writeSourceSet(ctx, groupID, group.Files(), configactivation.RetainDefinitions(), &group, compilation, group.Descriptor().Kind(), expectedOperational, expected, author, basis, nil)
}

func (s *Service) writeSourceSet(ctx context.Context, group storage.ConfigDocumentID, files []parser.SourceFile, choice configactivation.DefinitionChoice, editable *configactivation.SourceGroup, compilation configactivation.SourceCompilation, kind storage.ConfigDocumentKind, expectedOperational ExpectedDocument, expected []ExpectedSource, author storage.ConfigAuthor, basis WriteBasis, policies []configactivation.PolicyChange) (configactivation.Result, error) {
	if !group.Valid() || !expectedOperational.valid() || !author.Valid() || !basis.valid() || len(expected) != len(files) || len(expected) < 1 || len(expected) > 64 {
		return configactivation.Result{}, &Error{kind: FailureInvalid}
	}
	if kind == storage.ConfigDocumentKindYAMLSource && reservedSourceWorkspaceGroup(group) {
		return configactivation.Result{}, &Error{kind: FailureInvalid}
	}

	byPath := map[string]ExpectedSource{}
	for _, version := range expected {
		if _, duplicate := byPath[version.binding.Path()]; duplicate || !version.sql.valid() || !version.workspace.valid() {
			return configactivation.Result{}, &Error{kind: FailureInvalid}
		}
		byPath[version.binding.Path()] = version
	}
	members := make([]WorkspaceMember, 0, len(expected))
	for _, file := range files {
		version, present := byPath[file.Path()]
		if !present {
			return configactivation.Result{}, &Error{kind: FailureInvalid}
		}
		member, err := NewWorkspaceMember(group, version.binding, version.workspace, version.sql, file)
		if err != nil {
			return configactivation.Result{}, err
		}
		members = append(members, member)
	}
	release, err := s.acquireMutation(ctx)
	if err != nil {
		return configactivation.Result{}, err
	}
	defer release()
	current, err := s.Read(ctx, operationalID())
	if err != nil {
		return configactivation.Result{}, err
	}
	if err := basis.confirm(current); err != nil {
		return configactivation.Result{}, err
	}
	if current.document.Ref().Revision() != expectedOperational.revision || current.document.SourceSHA256() != expectedOperational.source || current.observation.hash != expectedOperational.workspace {
		return configactivation.Result{}, &Error{kind: Conflict}
	}
	if current.Dirty() != Clean {
		return configactivation.Result{}, &Error{kind: Conflict}
	}
	active, present, err := s.store.ReadActiveConfig(ctx)
	if err != nil || !present || active.Revision() != current.generation || active.ManifestSHA256() != current.manifest || active.Availability() != storage.ConfigGenerationComplete {
		return configactivation.Result{}, &Error{kind: Conflict}
	}
	// The immutable-version CAS and the active-generation authority are
	// independent. A retained inactive version is not the admitted SQL base.
	authorities, err := s.selectedSourceAuthorities(ctx, active, expected)
	if err != nil {
		return configactivation.Result{}, err
	}
	for index := range members {
		members[index].sqlBase = authorities[members[index].binding.ID()]
	}

	source, _ := storage.NewConfigSource("api.configdocuments.sources")
	request, err := configactivation.NewWorkspaceRequest(choice, author, source, policies, nil, nil)
	if err != nil {
		return configactivation.Result{}, err
	}
	if editable != nil {
		request, err = request.WithEditableSourceGroup(*editable, compilation)
		if err != nil {
			return configactivation.Result{}, err
		}
	}
	precondition, err := configactivation.NewWorkspacePrecondition(current.generation, current.manifest, current.document.SourceSHA256())
	if err != nil {
		return configactivation.Result{}, err
	}
	request, err = request.WithWorkspacePrecondition(precondition)
	if err != nil {
		return configactivation.Result{}, err
	}
	var edit *SetEdit
	request, err = request.WithWorkspaceSourceConfirmation(func() error {
		if edit == nil {
			return &Error{kind: ReconciliationRequired}
		}
		return edit.Confirm()
	}, func(tx *storage.ConfigTx) error {
		for _, version := range expected {
			var err error
			if editable != nil {
				err = tx.ConfirmEditableSourceVersion(editable.Descriptor(), version.binding, version.revision, version.sql.hash, version.sql.absent)
			} else {
				err = tx.ConfirmSourceVersion(version.binding, kind, version.revision, version.sql.hash, version.sql.absent)
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return configactivation.Result{}, err
	}
	result, activationErr := s.owner.ActivateWorkspaceEdit(ctx, request, func(stageCtx context.Context) error {
		confirmed, err := s.Read(stageCtx, operationalID())
		if err != nil {
			return err
		}
		if err := basis.confirm(confirmed); err != nil {
			return err
		}
		for _, version := range expected {
			var err error
			if editable != nil {
				err = s.store.CheckEditableSourceBinding(stageCtx, editable.Descriptor(), version.binding, version.revision, version.sql.hash, version.sql.absent)
			} else {
				err = version.confirmKind(stageCtx, s.store, kind)
			}
			if err != nil {
				return &Error{kind: Conflict}
			}
		}
		if err := stageCtx.Err(); err != nil {
			return err
		}
		if err := basis.confirmDeadline(); err != nil {
			return err
		}
		edit, err = s.workspace.beginSetEdit(members, func() error {
			if err := stageCtx.Err(); err != nil {
				return err
			}
			return basis.confirmDeadline()
		})
		return err
	})
	if edit == nil {
		return result, activationErr
	}
	known := result.Outcome() == configactivation.Published || result.Outcome() == configactivation.RejectedBeforeCommit || result.Outcome() == configactivation.NotCommitted || result.Outcome() == configactivation.RestartRequired
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	active, present, readErr := s.store.ReadActiveConfig(readCtx)
	admitted := map[storage.ConfigDocumentID]MemberExpectation{}
	if readErr == nil && present {
		admitted, readErr = s.selectedSourceAuthorities(readCtx, active, expected)
	}
	cancel()

	finishErr := edit.Finish(admitted, known && readErr == nil && present)
	if finishErr != nil {
		return result, errors.Join(activationErr, finishErr)
	}
	refreshCtx, refreshCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	_, _ = s.Refresh(refreshCtx)
	refreshCancel()
	return result, activationErr
}

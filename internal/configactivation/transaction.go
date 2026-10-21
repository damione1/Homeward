package configactivation

import (
	"context"
	"crypto/sha256"
	"errors"
	"sort"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
)

// DefinitionChoice is a closed preparation decision. Retention reuses exact
// active SQL references; replacement carries a compiler-admitted candidate
// and its complete path/document bindings. Zero is never retention.
type definitionChoiceKind uint8

const (
	definitionChoiceInvalid definitionChoiceKind = iota
	definitionChoiceRetain
	definitionChoiceReplace
)

type DefinitionChoice struct {
	kind      definitionChoiceKind
	candidate bundle.Candidate
	bindings  []storage.ConfigDocumentBinding
}

func RetainDefinitions() DefinitionChoice { return DefinitionChoice{kind: definitionChoiceRetain} }

func ReplaceDefinitions(candidate bundle.Candidate, bindings []storage.ConfigDocumentBinding) (DefinitionChoice, error) {
	if !candidate.Valid() || len(candidate.Sources()) == 0 || len(candidate.Sources()) != len(bindings) {
		return DefinitionChoice{}, ErrInvalidStartup
	}
	seenPath, seenID := make(map[string]bool, len(bindings)), make(map[storage.ConfigDocumentID]bool, len(bindings))
	for _, binding := range bindings {
		if !binding.ID().Valid() || binding.Path() == "" || seenPath[binding.Path()] || seenID[binding.ID()] {
			return DefinitionChoice{}, ErrInvalidStartup
		}
		seenPath[binding.Path()] = true
		seenID[binding.ID()] = true
	}
	for _, source := range candidate.Sources() {
		if !seenPath[source.Path()] {
			return DefinitionChoice{}, ErrInvalidStartup
		}
	}
	return DefinitionChoice{kind: definitionChoiceReplace, candidate: candidate,
		bindings: append([]storage.ConfigDocumentBinding(nil), bindings...)}, nil
}

type preparedActivation struct {
	startup      config.StartupInputs
	source       config.ExactWorkspaceSource
	expected     storage.ExpectedConfigActive
	attempt      storage.ActivationAttemptID
	metadata     storage.ConfigVersionMetadata
	definitions  DefinitionChoice
	legacy       bundle.Candidate
	retainedRefs []storage.ConfigVersionRef
	compiled     [sha256.Size]byte
	entry        string
	policies     []PolicyChange
}

// PolicyChange is a human/API configuration mutation, never an automation
// runtime authority. The account is derived from the admitted source; each
// touched account's policy admission gate is held by the activation owner.
type PolicyChange struct {
	account  contracts.AccountID
	expected authorization.ExpectedVersion
	source   authorization.PolicySource
}

func NewPolicyChange(source authorization.PolicySource, expected authorization.ExpectedVersion) (PolicyChange, error) {
	account, err := source.Account()
	if err != nil {
		return PolicyChange{}, ErrInvalidStartup
	}
	if _, _, err := expected.Values(); err != nil {
		return PolicyChange{}, ErrInvalidStartup
	}
	raw, err := source.Bytes()
	if err != nil {
		return PolicyChange{}, ErrInvalidStartup
	}
	owned, err := authorization.AdmitPolicySource(account, raw)
	if err != nil {
		return PolicyChange{}, ErrInvalidStartup
	}
	return PolicyChange{account: account, expected: expected, source: owned}, nil
}

// prepareWorkspaceActivation reads only the current SQL authority and exact
// workspace bytes. It does not publish programs, connect pilots or claim any
// durable command ID. The activation owner reserves run/engine capacity and
// policy admission tickets before calling writePreparedActivation.
func prepareWorkspaceActivation(ctx context.Context, store *storage.Store, startup config.StartupInputs,
	current AdmittedGeneration, definitions DefinitionChoice, author storage.ConfigAuthor,
	auditSource storage.ConfigSource, policies []PolicyChange) (preparedActivation, error) {
	if !usableContext(ctx) || store == nil || !startup.Valid() || !current.Valid() ||
		(definitions.kind != definitionChoiceRetain && definitions.kind != definitionChoiceReplace) {
		return preparedActivation{}, ErrInvalidStartup
	}
	metadata, err := storage.NewConfigVersionMetadata(author, auditSource)
	if err != nil {
		return preparedActivation{}, ErrInvalidStartup
	}
	active, present, err := store.ReadActiveConfig(ctx)
	if err != nil || !present || active.ManifestFormat() != storage.ConfigManifestCompiledV2 ||
		active.Availability() != storage.ConfigGenerationComplete || active.Revision() != current.Revision() ||
		active.ManifestSHA256() != current.ManifestSHA256() {
		return preparedActivation{}, errors.Join(ErrStartupConflict, err)
	}
	source, err := startup.CaptureWorkspace()
	if err != nil || !source.Valid() {
		return preparedActivation{}, errors.Join(ErrInvalidStartup, err)
	}
	expected, err := storage.ExpectConfigActive(current.Revision(), current.ManifestSHA256())
	if err != nil {
		return preparedActivation{}, err
	}
	attempt, err := storage.RandomActivationAttemptID()
	if err != nil {
		return preparedActivation{}, err
	}
	legacy, hasLegacy := current.LegacyCandidate()
	prepared := preparedActivation{startup: startup, source: source, expected: expected, attempt: attempt,
		metadata: metadata, definitions: definitions}
	seenAccounts := make(map[contracts.AccountID]struct{}, len(policies))
	for _, change := range policies {
		if _, err := change.account.Value(); err != nil {
			return preparedActivation{}, ErrInvalidStartup
		}
		account, err := change.source.Account()
		if err != nil || account != change.account {
			return preparedActivation{}, ErrInvalidStartup
		}
		if _, _, err := change.expected.Values(); err != nil {
			return preparedActivation{}, ErrInvalidStartup
		}
		if _, duplicate := seenAccounts[account]; duplicate {
			return preparedActivation{}, ErrInvalidStartup
		}
		seenAccounts[account] = struct{}{}
		prepared.policies = append(prepared.policies, change)
	}
	sort.Slice(prepared.policies, func(i, j int) bool {
		left, _ := prepared.policies[i].account.Value()
		right, _ := prepared.policies[j].account.Value()
		return left < right
	})
	if definitions.kind == definitionChoiceRetain {
		if hasLegacy {
			prepared.legacy = legacy
		}
		for _, version := range active.Versions() {
			kind, present := version.Kind()
			if !present {
				return preparedActivation{}, ErrStartupIntegrity
			}
			if kind == storage.ConfigDocumentKindYAMLSource {
				prepared.retainedRefs = append(prepared.retainedRefs, version.Ref())
			}
		}
		if len(prepared.retainedRefs) != len(prepared.legacy.Sources()) {
			return preparedActivation{}, ErrStartupIntegrity
		}
	} else {
		prepared.legacy = definitions.candidate
	}
	if 1+len(prepared.legacy.Sources()) > 256 {
		return preparedActivation{}, ErrInvalidStartup
	}
	total := len(source.Bytes())
	for _, file := range prepared.legacy.Sources() {
		total += len(file.Bytes())
	}
	if total > 4<<20 {
		return preparedActivation{}, ErrInvalidStartup
	}
	prepared.compiled = compiledOperationalHash(source.SHA256(), prepared.legacy)
	prepared.entry = source.LogicalPath()
	if prepared.legacy.Valid() {
		prepared.entry = prepared.legacy.Entry()
	}
	return prepared, nil
}

// writePreparedActivation is intentionally package-private until the owner
// can retain the registry gate, DCS pause and all in-memory projection images
// through COMMIT/reconciliation. Its callback is one ConfigTx, never nested.
func writePreparedActivation(ctx context.Context, store *storage.Store, replay *storage.ConfigReplayRepository,
	gate *registry.ActivationGate, prepared preparedActivation,
	mutateRegistry func(*registry.ConfigWriter) error) (storage.ConfigGenerationRevision, storage.TransactionOutcome, error) {
	if !usableContext(ctx) || store == nil || replay == nil || gate == nil || !prepared.source.Valid() ||
		!prepared.attempt.Valid() || prepared.compiled == ([sha256.Size]byte{}) || prepared.entry == "" ||
		(prepared.definitions.kind != definitionChoiceRetain && prepared.definitions.kind != definitionChoiceReplace) {
		return storage.ConfigGenerationRevision{}, storage.TransactionNotCommitted, ErrInvalidStartup
	}
	if mutateRegistry == nil {
		mutateRegistry = func(*registry.ConfigWriter) error { return nil }
	}
	binding, err := sourceBinding(operationalDocumentID, prepared.source.LogicalPath())
	if err != nil {
		return storage.ConfigGenerationRevision{}, storage.TransactionNotCommitted, err
	}
	var target storage.ConfigGenerationRevision
	outcome, err := store.InConfigTransactionOutcome(ctx, func(tx *storage.ConfigTx) error {
		if _, physical := prepared.source.PhysicalPath(); physical {
			if err := prepared.source.ConfirmPhysicalSource(); err != nil {
				return err
			}
		}
		operationalRef, err := tx.SaveProtectedOperationalSource(replay, binding,
			prepared.source.Bytes(), prepared.source.SHA256(), prepared.metadata)
		if err != nil {
			return err
		}
		refs := append([]storage.ConfigVersionRef(nil), prepared.retainedRefs...)
		if prepared.definitions.kind == definitionChoiceReplace {
			replaced, err := tx.SaveCandidate(prepared.legacy, prepared.definitions.bindings, prepared.metadata)
			if err != nil {
				return err
			}
			refs = replaced
		}
		refs = append(refs, operationalRef)
		target, err = tx.RegisterCompiledGeneration(prepared.entry, prepared.compiled, refs)
		if err != nil {
			return err
		}
		if err := gate.ApplyInConfigTx(tx, mutateRegistry); err != nil {
			return err
		}
		policyRefs := make([]storage.ConfigVersionRef, 0, len(prepared.policies))
		for _, change := range prepared.policies {
			ref, err := tx.SaveAuthorizationPolicy(change.source, prepared.metadata)
			if err != nil {
				return err
			}
			if _, err := tx.ActivateAuthorizationPolicy(change.account, change.expected, ref); err != nil {
				return err
			}
			policyRefs = append(policyRefs, ref)
		}
		// File authority is checked again at the actual CAS, after callbacks
		// and policy writes. An edit while SQL was in flight rolls all back.
		if _, physical := prepared.source.PhysicalPath(); physical {
			if err := prepared.source.ConfirmPhysicalSource(); err != nil {
				return err
			}
		}
		if err := prepared.startup.ConfirmBootstrapSource(); err != nil {
			return err
		}
		if err := tx.ActivateGeneration(prepared.expected, target); err != nil {
			return err
		}
		if err := tx.RecordActivationAttempt(prepared.attempt, prepared.metadata); err != nil {
			return err
		}
		for i, change := range prepared.policies {
			if err := tx.RecordActivationPolicyChange(prepared.attempt, change.account, change.expected, policyRefs[i]); err != nil {
				return err
			}
		}
		return nil
	})
	return target, outcome, err
}

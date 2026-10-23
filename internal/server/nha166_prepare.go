package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"sort"
	"strings"
	"unicode/utf8"

	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/accounts"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
)

var ErrMQTTObservationPreparationInvalid = errors.New("observation preparation: invalid private input or source")
var ErrMQTTObservationPreparationConflict = errors.New("observation preparation: existing input, account, policy or runtime differs")
var ErrMQTTObservationPreparationUnavailable = errors.New("observation preparation: storage or credential verification unavailable")

const observationPreparationSecret = "observation-setup-binding"

// MQTTReadOnlyObservationMapping is an explicit operator assertion about one
// discovery identity. No wire discovery or alias inference supplies authority.
type MQTTReadOnlyObservationMapping struct {
	key   integrations.SourceKey
	alias registry.HAAlias
}

func (MQTTReadOnlyObservationMapping) String() string     { return "[REDACTED OBSERVATION MAPPING]" }
func (m MQTTReadOnlyObservationMapping) GoString() string { return m.String() }
func (m MQTTReadOnlyObservationMapping) MarshalJSON() ([]byte, error) {
	return json.Marshal(m.String())
}
func NewMQTTReadOnlyObservationMapping(deviceIdentifier, uniqueID, alias string) (MQTTReadOnlyObservationMapping, error) {
	key, err := integrations.NewSourceKey(deviceIdentifier, uniqueID)
	if err != nil || strings.ContainsRune(deviceIdentifier, 0) || strings.ContainsRune(uniqueID, 0) {
		return MQTTReadOnlyObservationMapping{}, ErrMQTTObservationPreparationInvalid
	}
	parsed, err := registry.ParseHAAlias(alias)
	if err != nil {
		return MQTTReadOnlyObservationMapping{}, ErrMQTTObservationPreparationInvalid
	}
	return MQTTReadOnlyObservationMapping{key: key, alias: parsed}, nil
}
func observationAdmitMappings(mappings []MQTTReadOnlyObservationMapping) ([]MQTTReadOnlyObservationMapping, error) {
	if len(mappings) < 1 || len(mappings) > 256 {
		return nil, ErrMQTTObservationPreparationInvalid
	}
	result := append([]MQTTReadOnlyObservationMapping(nil), mappings...)
	keys := make(map[integrations.SourceKey]bool, len(result))
	aliases := make(map[registry.HAAlias]bool, len(result))
	uniqueIDs := make(map[string]bool, len(result))
	for _, mapping := range result {
		device, unique, err := mapping.key.Components()
		if err != nil || strings.ContainsRune(device, 0) || strings.ContainsRune(unique, 0) {
			return nil, ErrMQTTObservationPreparationInvalid
		}
		if _, err := mapping.alias.Value(); err != nil || keys[mapping.key] || aliases[mapping.alias] || uniqueIDs[unique] {
			return nil, ErrMQTTObservationPreparationInvalid
		}
		keys[mapping.key] = true
		aliases[mapping.alias] = true
		uniqueIDs[unique] = true
	}
	sort.Slice(result, func(i, j int) bool {
		di, ui, _ := result[i].key.Components()
		dj, uj, _ := result[j].key.Components()
		if di != dj {
			return di < dj
		}
		if ui != uj {
			return ui < uj
		}
		ai, _ := result[i].alias.Value()
		aj, _ := result[j].alias.Value()
		return ai < aj
	})
	return result, nil
}

type MQTTReadOnlyPreparationInput struct {
	username, password storage.SecretValue
	login              accounts.LoginName
	display            accounts.DisplayName
	adminPassword      string
	mappings           []MQTTReadOnlyObservationMapping
	valid              bool
}

func (MQTTReadOnlyPreparationInput) String() string {
	return "[REDACTED OBSERVATION PREPARATION INPUT]"
}
func (i MQTTReadOnlyPreparationInput) GoString() string             { return i.String() }
func (i MQTTReadOnlyPreparationInput) MarshalJSON() ([]byte, error) { return json.Marshal(i.String()) }
func NewMQTTReadOnlyPreparationInput(username, password, login, display, adminPassword string, mappings ...MQTTReadOnlyObservationMapping) (MQTTReadOnlyPreparationInput, error) {
	if len(username) == 0 || len(username) > 128 || !utf8.ValidString(username) || strings.ContainsRune(username, 0) || len(password) == 0 || len(password) > 65535 {
		return MQTTReadOnlyPreparationInput{}, ErrMQTTObservationPreparationInvalid
	}
	user, err := storage.NewSecretValue([]byte(username))
	if err != nil {
		return MQTTReadOnlyPreparationInput{}, ErrMQTTObservationPreparationInvalid
	}
	pass, err := storage.NewSecretValue([]byte(password))
	if err != nil {
		return MQTTReadOnlyPreparationInput{}, ErrMQTTObservationPreparationInvalid
	}
	name, err := accounts.ParseLoginName(login)
	if err != nil {
		return MQTTReadOnlyPreparationInput{}, ErrMQTTObservationPreparationInvalid
	}
	shown, err := accounts.ParseDisplayName(display)
	if err != nil {
		return MQTTReadOnlyPreparationInput{}, ErrMQTTObservationPreparationInvalid
	}
	if _, err := accounts.AdmitNewPassword(adminPassword); err != nil {
		return MQTTReadOnlyPreparationInput{}, ErrMQTTObservationPreparationInvalid
	}
	admittedMappings, err := observationAdmitMappings(mappings)
	if err != nil {
		return MQTTReadOnlyPreparationInput{}, err
	}
	return MQTTReadOnlyPreparationInput{username: user, password: pass, login: name, display: shown, adminPassword: adminPassword, mappings: admittedMappings, valid: true}, nil
}

// PrepareMQTTReadOnlyObservation is an offline operator port. It opens one
// normal Store, never a provider, runtime plan, pilot, listener or connection to
// MQTT/HA. The existing command-runtime guard excludes a serving Run and other
// preparations. Only the existing secret/account/policy/registry APIs write.
func PrepareMQTTReadOnlyObservation(ctx context.Context, startup config.StartupInputs, input MQTTReadOnlyPreparationInput) (returnedErr error) {
	if !validRunContext(ctx) || !input.valid || !startup.Valid() || !startup.MQTTReadOnlyObservation() {
		return ErrMQTTObservationPreparationInvalid
	}
	if _, physical := startup.BootstrapPath(); !physical {
		return ErrMQTTObservationPreparationInvalid
	}
	source, err := startup.CaptureWorkspace()
	if err != nil {
		return ErrMQTTObservationPreparationInvalid
	}
	operational, err := source.OperationalConfig()
	if err != nil {
		return ErrMQTTObservationPreparationInvalid
	}
	launch, err := startup.LaunchConfig()
	if err != nil {
		return ErrMQTTObservationPreparationInvalid
	}
	if validateMQTTReadOnlyObservation(operational, launch, configactivation.AdmittedGeneration{}, nil) != nil {
		return ErrMQTTObservationPreparationInvalid
	}
	// Validate the whole private mapping set against Run's selected manifest
	// before any Store, account, secret, config or binding write.
	mappings, err := observationAdmitMappings(input.mappings)
	if err != nil {
		return err
	}
	input.mappings = mappings
	scopeRevision, err := launch.ScopeRevision()
	if err != nil {
		return ErrMQTTObservationPreparationInvalid
	}
	manifest, err := registry.LoadManifest(scopeRevision)
	if err != nil {
		return ErrMQTTObservationPreparationInvalid
	}
	for _, mapping := range mappings {
		if _, present := manifest.SourceFor(mapping.alias); !present {
			return ErrMQTTObservationPreparationInvalid
		}
	}
	physicalMQTT, _ := operational.MQTTSettings()
	physicalInstance, err := physicalMQTT.Instance()
	if err != nil {
		return ErrMQTTObservationPreparationInvalid
	}
	settings, err := launch.PostgresSettings()
	if err != nil {
		return ErrMQTTObservationPreparationInvalid
	}
	store, err := storage.Open(ctx, settings)
	if err != nil {
		return ErrMQTTObservationPreparationUnavailable
	}
	defer store.Close()
	if _, err := store.Migrate(ctx); err != nil {
		return ErrMQTTObservationPreparationUnavailable
	}
	if _, err := store.BootstrapMFAPolicy(ctx); err != nil {
		return ErrMQTTObservationPreparationUnavailable
	}
	runtime, err := store.AcquireCommandRuntime(ctx)
	if err != nil {
		var stored *storage.Error
		if errors.As(err, &stored) && stored.Kind() == storage.CommandRuntimeBusy {
			return ErrMQTTObservationPreparationConflict
		}
		return ErrMQTTObservationPreparationUnavailable
	}
	defer func() {
		if runtime.Close() != nil {
			returnedErr = ErrMQTTObservationPreparationUnavailable
		}
	}()
	repository, err := registry.NewRepository(store, manifest)
	if err != nil {
		return ErrMQTTObservationPreparationUnavailable
	}
	if err := observationCheckBindings(ctx, repository, physicalInstance, input.mappings, false); err != nil {
		return err
	}
	// An established SQL generation may replay a different physical file.
	// Refuse drift before Reconcile can update even the bootstrap pointer.
	if err := observationCheckActiveSource(ctx, store, source.SHA256()); err != nil {
		return err
	}
	admitted, _, err := configactivation.ReconcileStartup(ctx, store, startup)
	if err != nil {
		return ErrMQTTObservationPreparationUnavailable
	}
	// This source digest authenticates the exact operational bytes, unlike
	// ManifestSHA256 which legitimately changes with account/policy writes.
	if admitted.SourceSHA256() != source.SHA256() {
		return ErrMQTTObservationPreparationConflict
	}
	operational, err = admitted.OperationalConfig()
	if err != nil || validateMQTTReadOnlyObservation(operational, launch, admitted, nil) != nil {
		return ErrMQTTObservationPreparationInvalid
	}
	mqtt, _ := operational.MQTTSettings()
	username, password, err := mqtt.CredentialHandles()
	if err != nil {
		return ErrMQTTObservationPreparationInvalid
	}
	instance, err := mqtt.Instance()
	if err != nil || instance != physicalInstance {
		return ErrMQTTObservationPreparationInvalid
	}
	name, _ := storage.ParseSecretName(observationPreparationSecret)
	binding, err := storage.NewSecretHandle(instance, name)
	if err != nil || binding == username || binding == password {
		return ErrMQTTObservationPreparationInvalid
	}
	secrets, err := admitted.Secrets()
	if err != nil {
		return ErrMQTTObservationPreparationUnavailable
	}
	// Compare all existing facts before initializing any missing credential.
	if err := observationSecretMatches(ctx, secrets, username, input.username, true); err != nil {
		return err
	}
	if err := observationSecretMatches(ctx, secrets, password, input.password, true); err != nil {
		return err
	}
	record, err := store.CredentialByLogin(ctx, input.login)
	existing := err == nil
	if err != nil && !errors.Is(err, storage.ErrAccountNotFound) {
		return ErrMQTTObservationPreparationUnavailable
	}
	if existing {
		if err := observationAccountMatches(ctx, record, input); err != nil {
			return err
		}
		if _, err := observationReadPolicy(ctx, store, record, false); err != nil {
			return err
		}
	} else {
		// A different already bootstrapped administrator must refuse before even
		// binding this preparation intent. This existing startup reader has no
		// actor/privilege bypass exposed to a public request.
		err := store.ForEachAuthorizationPolicy(ctx, func(authorization.AccountState) error { return ErrMQTTObservationPreparationConflict })
		if err != nil {
			if errors.Is(err, ErrMQTTObservationPreparationConflict) {
				return err
			}
			return ErrMQTTObservationPreparationUnavailable
		}
	}
	intent, err := observationPreparationIntent(ctx, source.SHA256(), username, password, input)
	if err != nil {
		return err
	}
	if _, err := secrets.InitializeCredentialIfAbsent(ctx, binding, intent); err != nil {
		return ErrMQTTObservationPreparationUnavailable
	}
	if err := observationIntentMatches(ctx, secrets, binding, source.SHA256(), username, password, input); err != nil {
		return err
	}
	if !existing {
		verifier, err := accounts.HashNewPassword(ctx, input.adminPassword)
		if err != nil {
			return ErrMQTTObservationPreparationUnavailable
		}
		id, err := accounts.NewAccountID()
		if err != nil {
			return ErrMQTTObservationPreparationUnavailable
		}
		record, err = store.BootstrapAdmin(ctx, storage.NewAccount{ID: id, Login: input.login, Display: input.display, Role: auth.AccountRole_ACCOUNT_ROLE_ADMIN, Verifier: verifier})
		if err != nil {
			record, err = store.CredentialByLogin(ctx, input.login)
			if err != nil {
				return ErrMQTTObservationPreparationConflict
			}
		}
	}
	if err := observationAccountMatches(ctx, record, input); err != nil {
		return err
	}
	if _, err := observationReadPolicy(ctx, store, record, true); err != nil {
		return err
	}
	for _, entry := range []struct {
		handle storage.SecretHandle
		value  storage.SecretValue
	}{{username, input.username}, {password, input.password}} {
		if _, err := secrets.InitializeCredentialIfAbsent(ctx, entry.handle, entry.value); err != nil {
			return ErrMQTTObservationPreparationUnavailable
		}
		if err := observationSecretMatches(ctx, secrets, entry.handle, entry.value, false); err != nil {
			return err
		}
	}
	for _, mapping := range input.mappings {
		alias, _ := mapping.alias.Value()
		if _, err := repository.ConfigureBinding(ctx, instance, mapping.key, alias); err != nil {
			return observationRegistryError(err)
		}
	}
	if err := observationCheckBindings(ctx, repository, instance, input.mappings, true); err != nil {
		return err
	}
	latest, err := startup.CaptureWorkspace()
	if err != nil || latest.SHA256() != source.SHA256() || startup.ConfirmBootstrapSource() != nil {
		return ErrMQTTObservationPreparationConflict
	}
	return nil
}
func observationSecretMatches(ctx context.Context, secrets *storage.SecretRepository, handle storage.SecretHandle, want storage.SecretValue, absentOK bool) error {
	found, err := secrets.Read(ctx, handle)
	if err != nil {
		var stored *storage.Error
		if absentOK && errors.As(err, &stored) && stored.Kind() == storage.SecretNotFound {
			return nil
		}
		return ErrMQTTObservationPreparationUnavailable
	}
	actual, err := found.Bytes()
	if err != nil {
		return ErrMQTTObservationPreparationUnavailable
	}
	defer clear(actual)
	expected, err := want.Bytes()
	if err != nil {
		return ErrMQTTObservationPreparationInvalid
	}
	defer clear(expected)
	if subtle.ConstantTimeCompare(actual, expected) != 1 {
		return ErrMQTTObservationPreparationConflict
	}
	return nil
}
func observationAccountMatches(ctx context.Context, record storage.AccountRecord, input MQTTReadOnlyPreparationInput) error {
	login, err := record.Login()
	if err != nil || login != input.login {
		return ErrMQTTObservationPreparationConflict
	}
	display, err := record.Display()
	if err != nil || display != input.display {
		return ErrMQTTObservationPreparationConflict
	}
	role, err := record.Role()
	if err != nil || role != auth.AccountRole_ACCOUNT_ROLE_ADMIN {
		return ErrMQTTObservationPreparationConflict
	}
	status, err := record.Status()
	if err != nil || status != auth.AccountStatus_ACCOUNT_STATUS_ACTIVE {
		return ErrMQTTObservationPreparationConflict
	}
	factor, err := record.SecondFactorEnabled()
	if err != nil || factor {
		return ErrMQTTObservationPreparationConflict
	}
	verifier, err := record.Verifier()
	if err != nil {
		return ErrMQTTObservationPreparationUnavailable
	}
	same, err := accounts.VerifyPassword(ctx, input.adminPassword, verifier)
	if err != nil {
		return ErrMQTTObservationPreparationUnavailable
	}
	if !same {
		return ErrMQTTObservationPreparationConflict
	}
	return nil
}
func observationReadPolicy(ctx context.Context, store *storage.Store, record storage.AccountRecord, write bool) (storage.AuthorizationPolicySnapshot, error) {
	id, err := record.ID()
	if err != nil {
		return storage.AuthorizationPolicySnapshot{}, ErrMQTTObservationPreparationUnavailable
	}
	snapshot, err := store.AuthorizationPolicy(ctx, id)
	if err != nil {
		return snapshot, ErrMQTTObservationPreparationUnavailable
	}
	current, err := snapshot.Source()
	if err != nil {
		return snapshot, ErrMQTTObservationPreparationUnavailable
	}
	text, _ := id.Value()
	wanted, err := authorization.AdmitPolicySource(id, []byte(fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants:\n  - scope: {family: state_registry, operation: read}\n    objects: {entities: {all: {capabilities: {all: {}}}}}\n", text)))
	if err != nil {
		return snapshot, ErrMQTTObservationPreparationUnavailable
	}
	actualHash, _ := current.SemanticSHA256()
	wantedHash, _ := wanted.SemanticSHA256()
	if subtle.ConstantTimeCompare(actualHash[:], wantedHash[:]) == 1 {
		return snapshot, nil
	}
	empty, _ := authorization.EmptySource(id)
	emptyHash, _ := empty.SemanticSHA256()
	if subtle.ConstantTimeCompare(actualHash[:], emptyHash[:]) != 1 {
		return snapshot, ErrMQTTObservationPreparationConflict
	}
	if !write {
		return snapshot, nil
	}
	version, err := snapshot.Version()
	if err != nil {
		return snapshot, ErrMQTTObservationPreparationUnavailable
	}
	expected, err := authorization.ExpectVersion(version)
	if err != nil {
		return snapshot, ErrMQTTObservationPreparationUnavailable
	}
	result, err := store.ReplaceAuthorizationPolicy(ctx, id, id, wanted, expected)
	if err != nil {
		return snapshot, ErrMQTTObservationPreparationUnavailable
	}
	replaced, _, err := result.Result()
	if err != nil {
		return snapshot, ErrMQTTObservationPreparationUnavailable
	}
	return replaced, nil
}
func observationPreparationTranscript(source [32]byte, username, password storage.SecretHandle, input MQTTReadOnlyPreparationInput) (string, error) {
	digest := sha256.New()
	observationTranscript(digest, []byte("homeward.observation.prepare/2"))
	observationTranscript(digest, source[:])
	for _, handle := range []storage.SecretHandle{username, password} {
		instance, name, err := handle.Components()
		if err != nil {
			return "", ErrMQTTObservationPreparationInvalid
		}
		i, _ := instance.Value()
		n, _ := name.Value()
		observationTranscript(digest, []byte(i))
		observationTranscript(digest, []byte(n))
	}
	for _, value := range []storage.SecretValue{input.username, input.password} {
		raw, err := value.Bytes()
		if err != nil {
			return "", ErrMQTTObservationPreparationInvalid
		}
		observationTranscript(digest, raw)
		clear(raw)
	}
	shown, key, _ := input.login.Values()
	display, _ := input.display.Value()
	for _, value := range []string{shown, key, display, input.adminPassword} {
		observationTranscript(digest, []byte(value))
	}
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], uint64(len(input.mappings)))
	observationTranscript(digest, count[:])
	for _, mapping := range input.mappings {
		device, unique, err := mapping.key.Components()
		if err != nil {
			return "", ErrMQTTObservationPreparationInvalid
		}
		alias, err := mapping.alias.Value()
		if err != nil {
			return "", ErrMQTTObservationPreparationInvalid
		}
		for _, field := range []string{device, unique, alias} {
			observationTranscript(digest, []byte(field))
		}
	}
	return "homeward-observation-intent-v2:" + hex.EncodeToString(digest.Sum(nil)), nil
}
func observationTranscript(digest hash.Hash, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = digest.Write(length[:])
	_, _ = digest.Write(value)
}

// The encrypted marker carries only the normal account Argon2id verifier.
// Decrypting it never yields a fast offline oracle for the native password.
func observationPreparationIntent(ctx context.Context, source [32]byte, username, password storage.SecretHandle, input MQTTReadOnlyPreparationInput) (storage.SecretValue, error) {
	transcript, err := observationPreparationTranscript(source, username, password, input)
	if err != nil {
		return storage.SecretValue{}, err
	}
	verifier, err := accounts.HashNewPassword(ctx, transcript)
	if err != nil {
		return storage.SecretValue{}, ErrMQTTObservationPreparationUnavailable
	}
	profile, salt, digest, err := verifier.StorageValues()
	if err != nil {
		return storage.SecretValue{}, ErrMQTTObservationPreparationUnavailable
	}
	encoded := make([]byte, 58)
	copy(encoded, "N166KDF1")
	binary.BigEndian.PutUint16(encoded[8:10], uint16(profile))
	copy(encoded[10:26], salt[:])
	copy(encoded[26:58], digest[:])
	defer clear(encoded)
	value, err := storage.NewSecretValue(encoded)
	if err != nil {
		return storage.SecretValue{}, ErrMQTTObservationPreparationUnavailable
	}
	return value, nil
}
func observationIntentMatches(ctx context.Context, secrets *storage.SecretRepository, binding storage.SecretHandle, source [32]byte, username, password storage.SecretHandle, input MQTTReadOnlyPreparationInput) error {
	value, err := secrets.Read(ctx, binding)
	if err != nil {
		return ErrMQTTObservationPreparationUnavailable
	}
	encoded, err := value.Bytes()
	if err != nil {
		return ErrMQTTObservationPreparationUnavailable
	}
	defer clear(encoded)
	if len(encoded) != 58 || string(encoded[:8]) != "N166KDF1" {
		return ErrMQTTObservationPreparationConflict
	}
	verifier, err := accounts.ParseVerifier(int16(binary.BigEndian.Uint16(encoded[8:10])), encoded[10:26], encoded[26:58])
	if err != nil {
		return ErrMQTTObservationPreparationConflict
	}
	transcript, err := observationPreparationTranscript(source, username, password, input)
	if err != nil {
		return err
	}
	match, err := accounts.VerifyPassword(ctx, transcript, verifier)
	if err != nil {
		return ErrMQTTObservationPreparationUnavailable
	}
	if !match {
		return ErrMQTTObservationPreparationConflict
	}
	return nil
}

func observationRegistryError(err error) error {
	for _, conflict := range []error{registry.ErrInvalidInput, registry.ErrOutsideManifest, registry.ErrUnconfigured, registry.ErrTombstoned, registry.ErrAliasConflict, registry.ErrBindingConflict} {
		if errors.Is(err, conflict) {
			return ErrMQTTObservationPreparationConflict
		}
	}
	return ErrMQTTObservationPreparationUnavailable
}

// A current instance may contain only an exact, unchanged subset of the full
// intent before preparation. The final pass requires the entire set. The
// existing batch reader validates all aliases/collisions before the first
// ConfigureBinding transaction, without committing its prospective IDs.
func observationCheckBindings(ctx context.Context, repository *registry.Repository, instance domain.IntegrationInstanceID, mappings []MQTTReadOnlyObservationMapping, complete bool) error {
	wanted := make(map[integrations.SourceKey]registry.HAAlias, len(mappings))
	uniqueIDs := make(map[string]integrations.SourceKey, len(mappings))
	requests := make([]registry.BindingRequest, 0, len(mappings))
	for _, mapping := range mappings {
		wanted[mapping.key] = mapping.alias
		_, unique, _ := mapping.key.Components()
		uniqueIDs[unique] = mapping.key
		alias, _ := mapping.alias.Value()
		request, err := registry.NewBindingRequest(instance, mapping.key, alias)
		if err != nil {
			return ErrMQTTObservationPreparationInvalid
		}
		requests = append(requests, request)
	}
	records, err := repository.List(ctx)
	if err != nil {
		return observationRegistryError(err)
	}
	matched := 0
	for _, record := range records {
		currentInstance, key, err := record.Source()
		if err != nil {
			return ErrMQTTObservationPreparationUnavailable
		}
		if currentInstance != instance {
			continue
		}
		original, current, _, err := record.Aliases()
		if err != nil {
			return ErrMQTTObservationPreparationUnavailable
		}
		alias, present := wanted[key]
		if !present || record.Retired() || original != alias || current != alias {
			return ErrMQTTObservationPreparationConflict
		}
		_, unique, err := key.Components()
		if err != nil {
			return ErrMQTTObservationPreparationUnavailable
		}
		if selected, present := uniqueIDs[unique]; present && selected != key {
			return ErrMQTTObservationPreparationConflict
		}
		matched++
	}
	if complete && matched != len(mappings) {
		return ErrMQTTObservationPreparationConflict
	}
	if _, err := repository.PrepareBindings(ctx, requests); err != nil {
		return observationRegistryError(err)
	}
	return nil
}

// ReadActiveConfig already verifies one bounded generation snapshot. Its
// protected-source SHA metadata is sufficient for this pre-write comparison;
// decrypting/initializing the key repository is unnecessary. A legacy upgrade
// admits the physical source through ReconcileStartup and the backstop above.
func observationCheckActiveSource(ctx context.Context, store *storage.Store, physical [32]byte) error {
	active, present, err := store.ReadActiveConfig(ctx)
	if err != nil {
		return ErrMQTTObservationPreparationUnavailable
	}
	if !present || active.ManifestFormat() == storage.ConfigManifestLegacyV1 {
		return nil
	}
	if active.ManifestFormat() != storage.ConfigManifestCompiledV2 {
		return ErrMQTTObservationPreparationConflict
	}
	found := false
	for _, version := range active.Versions() {
		kind, present := version.Kind()
		if !present {
			return ErrMQTTObservationPreparationUnavailable
		}
		if kind != storage.ConfigDocumentKindOperationalYAML {
			continue
		}
		if found || version.SHA256() != physical {
			return ErrMQTTObservationPreparationConflict
		}
		found = true
	}
	if !found {
		return ErrMQTTObservationPreparationConflict
	}
	return nil
}

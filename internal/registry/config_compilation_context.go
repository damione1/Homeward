package registry

import (
	"strconv"

	"github.com/damione1/Homeward/internal/configcompilation"
	"github.com/damione1/Homeward/internal/storage"
)

// ConfirmCompilationIdentities checks the compilation input against the SQL
// image after the activation callback's mutations, in the same ConfigTx and
// behind its existing registry locks. It grants no live capability support.
// Unrelated SQL identities do not enter or broaden the compiled context.
func (w *ConfigWriter) ConfirmCompilationIdentities(compilation configcompilation.Context) (err error) {
	defer func() { w.remember(err) }()
	if err := w.valid(); err != nil {
		return err
	}
	if !compilation.Valid() {
		return ErrInvalidInput
	}
	revision, valid := w.repo.manifest.Revision()
	if !valid {
		return ErrInvalidManifest
	}
	revisionText, err := revision.Value()
	digest, valid := w.repo.manifest.Digest()
	if err != nil || !valid || revisionText != compilation.ManifestRevision() || digest != compilation.ManifestSHA256() {
		return ErrBindingConflict
	}
	for _, identity := range compilation.Identities() {
		device, deviceErr := identity.Device.Value()
		entity, entityErr := identity.Entity.Value()
		instance, instanceErr := identity.Instance.Value()
		original, originalErr := identity.Original.Value()
		if deviceErr != nil || entityErr != nil || instanceErr != nil || originalErr != nil || !w.repo.manifest.permits(identity.Original) {
			return ErrInvalidInput
		}
		var actualDevice, actualEntity, actualOriginal string
		var retired bool
		if err := w.sql.QueryRow(storage.RegistrySourceBinding(), instance, identity.SourceDevice, identity.SourceEntity).
			Scan(&actualDevice, &actualEntity, &actualOriginal, &retired); err != nil {
			return err
		}
		if actualDevice != device || actualEntity != entity || actualOriginal != original || retired != identity.Retired {
			return ErrBindingConflict
		}
		var sourceDevice string
		if err := w.sql.QueryRow(storage.RegistryDeviceSource(), instance, identity.SourceDevice).Scan(&sourceDevice); err != nil {
			return err
		}
		if sourceDevice != device {
			return ErrBindingConflict
		}
		var aliasCount string
		if err := w.sql.QueryRow(storage.RegistryEntityAliasCount(), entity).Scan(&aliasCount); err != nil {
			return err
		}
		if aliasCount != strconv.Itoa(len(identity.Aliases)) {
			return ErrBindingConflict
		}
		for _, alias := range identity.Aliases {
			label, err := alias.Value()
			if err != nil {
				return ErrInvalidInput
			}
			var owner string
			var current bool
			if err := w.sql.QueryRow(storage.RegistryAliasOwner(), label).Scan(&owner, &current); err != nil {
				return err
			}
			if owner != entity || current != (alias == identity.Current) {
				return ErrBindingConflict
			}
		}
	}
	return nil
}

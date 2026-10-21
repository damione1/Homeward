package config

import backuparchive "github.com/damione1/Homeward/internal/backups/archive"

// BackupRecovery is public launch material, never a private recovery identity.
func (c Config) BackupRecovery() (backuparchive.RecoveryPublic, bool, error) {
	if !c.backupProviders.Valid() {
		return backuparchive.RecoveryPublic{}, false, ErrUninitialized
	}
	if c.backupRecovery == nil {
		return backuparchive.RecoveryPublic{}, false, nil
	}
	return *c.backupRecovery, true, nil
}

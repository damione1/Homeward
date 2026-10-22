package configactivation

import "github.com/damione1/Homeward/internal/storage"

// WithWorkspaceSourceConfirmation binds a multi-file edit to the actual SQL
// transaction. These callbacks come from the single trusted workspace owner;
// no transport can provide them. The filesystem callback does not acquire the
// workspace mutex again: the staged edit already owns it until Finish.
func (r Request) WithWorkspaceSourceConfirmation(files func() error, versions func(*storage.ConfigTx) error) (Request, error) {
	if !r.admitted || files == nil || versions == nil || r.confirmSourceFiles != nil || r.confirmSourceVersions != nil {
		return Request{}, ErrInvalidStartup
	}
	r.confirmSourceFiles, r.confirmSourceVersions = files, versions
	return r, nil
}

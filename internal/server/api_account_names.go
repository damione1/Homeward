package server

import (
	"context"
	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/storage"
)

// authResourceNames reads only identities allocated by the native transaction.
// Callers authenticate and authorize before invoking any resolver. Parent names
// are checked against native ownership, never accepted as proof of ownership.
type authResourceNames struct{ administrationNames }

func (n authResourceNames) account(ctx context.Context, id contracts.AccountID) (apitypes.AccountName, error) {
	key, err := id.Value()
	if err != nil {
		return apitypes.AccountName{}, err
	}
	value, err := n.name(ctx, storage.APIAccounts, key)
	if err != nil {
		return apitypes.AccountName{}, err
	}
	return apitypes.ParseAccountName(value)
}

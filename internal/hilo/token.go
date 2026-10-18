package hilo

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
)

const subscriptionKey = "20eeaedcb86945afa3fe792cea89b8bf"
const tokenRenewalMargin = 30 * time.Second // pinned SDK v0.1.3 auth.go:29

func decodeCredential(value storage.SecretValue) (credentialJSON, error) {
	raw, err := value.Bytes()
	if err != nil {
		return credentialJSON{}, ErrReauthorizationRequired
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var credential credentialJSON
	if decoder.Decode(&credential) != nil {
		return credentialJSON{}, ErrReauthorizationRequired
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return credentialJSON{}, ErrReauthorizationRequired
	}
	if _, err := credential.secretValue(); err != nil {
		return credentialJSON{}, ErrReauthorizationRequired
	}
	return credential, nil
}

// liveCredential only refreshes an existing revision through NHA-096. Absence,
// invalid_grant, lost response or ambiguous commit never trigger a B2C login.
func liveCredential(ctx context.Context, client *integrations.CloudClient, handle storage.SecretHandle) (credentialJSON, error) {
	observed, err := client.ReadCredential(ctx, handle)
	if err != nil {
		return credentialJSON{}, ErrReauthorizationRequired
	}
	value, err := observed.Value()
	if err != nil {
		return credentialJSON{}, ErrReauthorizationRequired
	}
	current, err := decodeCredential(value)
	if err != nil {
		return credentialJSON{}, err
	}
	if time.Now().UTC().Before(current.ExpiresAt.Add(-tokenRenewalMargin)) {
		return current, nil
	}
	renewal, err := client.BeginRenewal(ctx, observed)
	if err != nil {
		return credentialJSON{}, ErrReauthorizationRequired
	}
	value, err = renewal.Value()
	if err != nil {
		return credentialJSON{}, ErrReauthorizationRequired
	}
	latest, err := decodeCredential(value)
	if err != nil {
		return credentialJSON{}, err
	}
	if renewal.Kind() == integrations.CloudRenewalJoined {
		if !time.Now().UTC().Before(latest.ExpiresAt.Add(-tokenRenewalMargin)) {
			return credentialJSON{}, ErrReauthorizationRequired
		}
		return latest, nil
	}
	lease, err := renewal.Lease()
	if err != nil {
		return credentialJSON{}, ErrReauthorizationRequired
	}
	defer lease.Close()
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {latest.Refresh}, "scope": {clientScope}}
	request, err := integrations.NewCloudRequest(http.MethodPost, b2cOrigin+tokenPath+"?p="+b2cPolicy, http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, []byte(form.Encode()))
	if err != nil {
		return credentialJSON{}, ErrReauthorizationRequired
	}
	response, err := lease.Exchange(request)
	if err != nil {
		return credentialJSON{}, ErrReauthorizationRequired
	}
	updated, err := decodeTokenResponse(response.Bytes(), latest.Refresh, time.Now().UTC())
	if err != nil {
		return credentialJSON{}, err
	}
	secret, err := updated.secretValue()
	if err != nil {
		return credentialJSON{}, ErrReauthorizationRequired
	}
	if err := lease.Commit(secret); err != nil {
		return credentialJSON{}, ErrReauthorizationRequired
	}
	return updated, nil
}

func bearerHeaders(token credentialJSON, subscription bool) http.Header {
	header := http.Header{"Authorization": {"Bearer " + token.Access}}
	if subscription {
		header.Set("Ocp-Apim-Subscription-Key", subscriptionKey)
	}
	return header
}

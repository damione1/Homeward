package registry

import (
	"context"
	"errors"
	"unicode/utf8"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/jackc/pgx/v5"
)

// MatchMQTTSource selects the one configured, active key whose source device
// is present in the wire identifiers. Wire order and names carry no authority.
func (r *Repository) MatchMQTTSource(ctx context.Context, instance domain.IntegrationInstanceID, identifiers []string, uniqueID string) (integrations.SourceKey, error) {
	if r == nil || ctx == nil || len(identifiers) == 0 || len(identifiers) > 16 || uniqueID == "" || len(uniqueID) > 256 || !utf8.ValidString(uniqueID) {
		return integrations.SourceKey{}, ErrInvalidInput
	}
	instanceText, err := instance.Value()
	if err != nil {
		return integrations.SourceKey{}, ErrInvalidInput
	}
	allowed := make(map[string]struct{}, len(identifiers))
	for _, identifier := range identifiers {
		if _, err := integrations.NewSourceKey(identifier, uniqueID); err != nil {
			return integrations.SourceKey{}, ErrInvalidInput
		}
		if _, duplicate := allowed[identifier]; duplicate {
			return integrations.SourceKey{}, ErrInvalidInput
		}
		allowed[identifier] = struct{}{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.projectionDirty {
		return integrations.SourceKey{}, ErrProjectionUnavailable
	}
	var selected integrations.SourceKey
	retired := false
	err = r.transaction(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT b.source_device,e.manifest_alias,e.tombstoned
			FROM public.registry_source_bindings b JOIN public.registry_entities e ON e.id=b.entity_id
			WHERE b.instance_id=$1 AND b.source_entity=$2`, instanceText, uniqueID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var device, aliasText string
			var isRetired bool
			if err := rows.Scan(&device, &aliasText, &isRetired); err != nil {
				return err
			}
			if _, found := allowed[device]; !found {
				continue
			}
			alias, err := ParseHAAlias(aliasText)
			if err != nil || !r.manifest.permits(alias) {
				return ErrOutsideManifest
			}
			if selected != (integrations.SourceKey{}) {
				return ErrBindingConflict
			}
			selected, err = integrations.NewSourceKey(device, uniqueID)
			if err != nil {
				return ErrStorage
			}
			retired = isRetired
		}
		return rows.Err()
	})
	if err != nil {
		return integrations.SourceKey{}, err
	}
	if selected == (integrations.SourceKey{}) {
		return integrations.SourceKey{}, ErrUnconfigured
	}
	if retired {
		return integrations.SourceKey{}, ErrTombstoned
	}
	return selected, nil
}

func validDiscoveryTopic(topic string) bool {
	return topic != "" && len(topic) <= 256 && utf8.ValidString(topic)
}

// RememberMQTTTopic records provenance after the key was explicitly configured.
// It is idempotent; a source cannot silently migrate to another topic.
func (r *Repository) RememberMQTTTopic(ctx context.Context, instance domain.IntegrationInstanceID, topic string, key integrations.SourceKey) error {
	return r.RememberMQTTTopics(ctx, instance, topic, []integrations.SourceKey{key})
}

// RememberMQTTTopics commits one fully validated discovery document's source
// provenance atomically; an invalid neighbor cannot leave a partial mapping.
func (r *Repository) RememberMQTTTopics(ctx context.Context, instance domain.IntegrationInstanceID, topic string, keys []integrations.SourceKey) error {
	if r == nil || ctx == nil || !validDiscoveryTopic(topic) {
		return ErrInvalidInput
	}
	if len(keys) == 0 || len(keys) > 64 {
		return ErrInvalidInput
	}
	instanceText, err := instance.Value()
	if err != nil {
		return ErrInvalidInput
	}
	seen := make(map[integrations.SourceKey]struct{}, len(keys))
	for _, key := range keys {
		if _, _, err := key.Components(); err != nil {
			return ErrInvalidInput
		}
		if _, duplicate := seen[key]; duplicate {
			return ErrInvalidInput
		}
		seen[key] = struct{}{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.projectionDirty {
		return ErrProjectionUnavailable
	}
	return r.transaction(ctx, func(tx pgx.Tx) error {
		for _, key := range keys {
			device, entity, _ := key.Components()
			var retired bool
			err := tx.QueryRow(ctx, `SELECT e.tombstoned FROM public.registry_source_bindings b
			JOIN public.registry_entities e ON e.id=b.entity_id
			WHERE b.instance_id=$1 AND b.source_device=$2 AND b.source_entity=$3`, instanceText, device, entity).Scan(&retired)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrUnconfigured
			}
			if err != nil {
				return err
			}
			if retired {
				return ErrTombstoned
			}
			result, err := tx.Exec(ctx, `INSERT INTO public.registry_mqtt_discovery_topics(instance_id,discovery_topic,source_device,source_entity)
			VALUES ($1,$2,$3,$4) ON CONFLICT (instance_id,source_device,source_entity) DO UPDATE
			SET discovery_topic=EXCLUDED.discovery_topic
			WHERE public.registry_mqtt_discovery_topics.discovery_topic=EXCLUDED.discovery_topic`, instanceText, topic, device, entity)
			if err != nil {
				return err
			}
			if result.RowsAffected() == 0 {
				return ErrBindingConflict
			}
		}
		return nil
	})
}

// MQTTTopicSources returns the durable, active keys previously seen at a topic.
// A retained empty payload can therefore retire them after a cold restart.
func (r *Repository) MQTTTopicSources(ctx context.Context, instance domain.IntegrationInstanceID, topic string) ([]integrations.SourceKey, error) {
	if r == nil || ctx == nil || !validDiscoveryTopic(topic) {
		return nil, ErrInvalidInput
	}
	instanceText, err := instance.Value()
	if err != nil {
		return nil, ErrInvalidInput
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.projectionDirty {
		return nil, ErrProjectionUnavailable
	}
	var keys []integrations.SourceKey
	err = r.transaction(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT t.source_device,t.source_entity FROM public.registry_mqtt_discovery_topics t
			JOIN public.registry_source_bindings b USING (instance_id,source_device,source_entity)
			JOIN public.registry_entities e ON e.id=b.entity_id
			WHERE t.instance_id=$1 AND t.discovery_topic=$2 AND NOT e.tombstoned
			ORDER BY t.source_device,t.source_entity`, instanceText, topic)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var device, entity string
			if err := rows.Scan(&device, &entity); err != nil {
				return err
			}
			key, err := integrations.NewSourceKey(device, entity)
			if err != nil {
				return ErrStorage
			}
			keys = append(keys, key)
		}
		return rows.Err()
	})
	return keys, err
}

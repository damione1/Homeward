package storage

// helperValuesSchema is registered by append-only migration 13. Numeric
// tags are converted explicitly by helpers.Kind; SQL never stores free-form JSON.
const helperValuesSchema = `
CREATE TABLE public.helper_values (
    entity_id text PRIMARY KEY REFERENCES public.registry_entities(id),
    kind smallint NOT NULL,
    boolean_value boolean,
    number_value double precision,
    counter_value bigint,
    revision bigint NOT NULL CHECK (revision > 0),
    updated_at timestamptz NOT NULL,
    CONSTRAINT helper_values_payload CHECK (
        (kind = 1 AND boolean_value IS NOT NULL AND number_value IS NULL AND counter_value IS NULL) OR
        (kind = 2 AND boolean_value IS NULL AND number_value IS NOT NULL AND counter_value IS NULL
            AND number_value > '-Infinity'::double precision AND number_value < 'Infinity'::double precision) OR
        (kind = 3 AND boolean_value IS NULL AND number_value IS NULL AND counter_value IS NOT NULL)
    )
);
`

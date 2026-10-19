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

// Migration 16 is additive. The v13 table and its original rows remain intact.
const helperValuesV16 = `
ALTER TYPE public.config_schema_version ADD VALUE 'ha-declarative-2';
ALTER TYPE public.config_syntax_profile ADD VALUE 'ha-2026.10/declarative-2';
ALTER TABLE public.helper_values
    ADD COLUMN button_pressed_at timestamptz,
    ADD COLUMN date_value date,
    ADD COLUMN time_value time without time zone,
    ADD COLUMN datetime_value timestamp without time zone,
    ADD COLUMN select_value text,
    ADD COLUMN text_value text;
ALTER TABLE public.helper_values DROP CONSTRAINT helper_values_payload;
ALTER TABLE public.helper_values ADD CONSTRAINT helper_values_payload CHECK (
    num_nonnulls(boolean_value, number_value, counter_value, button_pressed_at,
                 date_value, time_value, datetime_value, select_value, text_value) = 1
    AND ((kind = 1 AND boolean_value IS NOT NULL)
      OR (kind = 2 AND number_value IS NOT NULL AND number_value > '-Infinity'::double precision
          AND number_value < 'Infinity'::double precision)
      OR (kind = 3 AND counter_value IS NOT NULL)
      OR (kind = 4 AND button_pressed_at IS NOT NULL)
      OR (kind = 5 AND date_value IS NOT NULL)
      OR (kind = 6 AND time_value IS NOT NULL)
      OR (kind = 7 AND datetime_value IS NOT NULL)
      OR (kind = 8 AND select_value IS NOT NULL AND select_value <> '')
      OR (kind = 9 AND text_value IS NOT NULL))
);
`

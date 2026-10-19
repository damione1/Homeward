package storage

// Append-only migration17. NULL remains an observation without a value;
// Solar and SolarEvent are results, never numeric epoch measurements.
const recorderSolarSchemaSQL = `
ALTER TABLE public.state_history DROP CONSTRAINT state_history_value_kind_check;
ALTER TABLE public.state_history ADD CONSTRAINT state_history_value_kind_check
    CHECK (value_kind IS NULL OR value_kind IN (1,2,3,4,5,6,7,8));
`

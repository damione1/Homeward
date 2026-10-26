package storage

// Typed controls remain non-numeric recorder values; their deterministic wire
// payload carries the independent observations. Historical migrations are frozen.
const recorderControlsSchemaSQL = `
ALTER TABLE public.state_history DROP CONSTRAINT state_history_value_kind_check;
ALTER TABLE public.state_history ADD CONSTRAINT state_history_value_kind_check
    CHECK (value_kind IS NULL OR value_kind IN (1,2,3,4,5,6,7,8,9,10,11));
`

package storage

// Weather and presence payloads remain non-numeric. This is additive to the
// frozen SQL27 prefix; older readers are excluded by the new release window.
const recorderEnvironmentSchemaSQL = `
ALTER TABLE public.state_history DROP CONSTRAINT state_history_value_kind_check;
ALTER TABLE public.state_history ADD CONSTRAINT state_history_value_kind_check
    CHECK (value_kind IS NULL OR value_kind IN (1,2,3,4,5,6,7,8,9,10,11,12,13,14));
`

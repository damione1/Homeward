package storage

// ValueHelper is domain.StateValue's single new tag. NULL remains an absent
// observation. Later observation families require their own migration.
const recorderHelperSchemaSQL = `
ALTER TABLE public.state_history DROP CONSTRAINT state_history_value_kind_check;
ALTER TABLE public.state_history ADD CONSTRAINT state_history_value_kind_check
    CHECK (value_kind IS NULL OR value_kind IN (1,2,3,4,5,6));
`

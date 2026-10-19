package storage

// localListsSchemaSQL is migration 19. List and item tombstones, their HA
// provenance, and immutable import fingerprints survive ordinary CRUD.
const localListsSchemaSQL = `
CREATE TYPE public.homeward_list_origin AS ENUM ('legacy_local_todo', 'legacy_shopping_list', 'native');
CREATE TABLE public.homeward_lists_catalog (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    revision bigint NOT NULL CHECK (revision > 0)
);
INSERT INTO public.homeward_lists_catalog(singleton,revision) VALUES (true,1);
CREATE TABLE public.homeward_lists (
    list_id text PRIMARY KEY CHECK (list_id <> '' AND octet_length(list_id) <= 255),
    origin public.homeward_list_origin NOT NULL,
    legacy_alias text UNIQUE CHECK (legacy_alias IS NULL OR (legacy_alias <> '' AND octet_length(legacy_alias) <= 512)),
    name text NOT NULL CHECK (name <> '' AND octet_length(name) <= 512),
    lifecycle smallint NOT NULL CHECK (lifecycle IN (1,2)),
    revision bigint NOT NULL CHECK (revision > 0),
    updated_at timestamptz NOT NULL DEFAULT now(),
    last_actor_id text CHECK (last_actor_id IS NULL OR (last_actor_id <> '' AND octet_length(last_actor_id) <= 128)),
    UNIQUE(list_id,origin),
    CHECK ((origin = 'native' AND legacy_alias IS NULL) OR (origin <> 'native' AND legacy_alias IS NOT NULL))
);
CREATE INDEX homeward_lists_active_by_id ON public.homeward_lists(list_id) WHERE lifecycle=1;
CREATE TABLE public.homeward_list_items (
    list_id text NOT NULL,
    item_uid text NOT NULL CHECK (item_uid <> '' AND octet_length(item_uid) <= 255),
    origin public.homeward_list_origin NOT NULL,
    summary text NOT NULL CHECK (summary <> '' AND octet_length(summary) <= 512),
    completed boolean NOT NULL,
    description text CHECK (description IS NULL OR octet_length(description) <= 4096),
    due_date date,
    due_at timestamptz,
    due_zone text CHECK (due_zone IS NULL OR (due_zone <> '' AND octet_length(due_zone) <= 128)),
    completed_at timestamptz,
    position bigint NOT NULL CHECK (position >= 0),
    lifecycle smallint NOT NULL CHECK (lifecycle IN (1,2)),
    revision bigint NOT NULL CHECK (revision > 0),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(list_id,item_uid),
    FOREIGN KEY(list_id,origin) REFERENCES public.homeward_lists(list_id,origin),
    CHECK ((due_date IS NULL AND due_at IS NULL AND due_zone IS NULL) OR
           (due_date IS NOT NULL AND due_at IS NULL AND due_zone IS NULL) OR
           (due_date IS NULL AND due_at IS NOT NULL AND due_zone IS NOT NULL)),
    CHECK (origin <> 'legacy_shopping_list' OR (description IS NULL AND due_date IS NULL AND due_at IS NULL AND due_zone IS NULL AND completed_at IS NULL))
);
CREATE INDEX homeward_list_items_active_order ON public.homeward_list_items(list_id,position,item_uid) WHERE lifecycle=1;
CREATE TABLE public.homeward_legacy_import_ledger (
    legacy_alias text NOT NULL CHECK (legacy_alias <> '' AND octet_length(legacy_alias) <= 512),
    item_uid text NOT NULL CHECK (octet_length(item_uid) <= 255),
    source_sha256 bytea NOT NULL CHECK (octet_length(source_sha256)=32),
    first_revision bigint NOT NULL CHECK (first_revision > 0),
    imported_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(legacy_alias,item_uid),
    FOREIGN KEY(legacy_alias) REFERENCES public.homeward_lists(legacy_alias)
);
`

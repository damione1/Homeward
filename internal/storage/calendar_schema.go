package storage

// calendarSchemaSQL is migration 15 in the authoritative append-only list.
// Tags are explicitly encoded/decoded by calendars; SQL text is not a domain.
const calendarSchemaSQL = `
CREATE TABLE public.local_calendars (
    entity_id text PRIMARY KEY REFERENCES public.registry_entities(id),
    origin smallint NOT NULL CHECK (origin = 1),
    name text NOT NULL CHECK (octet_length(name) BETWEEN 1 AND 512),
    lifecycle smallint NOT NULL CHECK (lifecycle IN (1,2)),
    revision bigint NOT NULL CHECK (revision > 0),
    updated_at timestamptz NOT NULL
);
CREATE TABLE public.calendar_events (
    calendar_id text NOT NULL REFERENCES public.local_calendars(entity_id),
    uid text NOT NULL CHECK (octet_length(uid) BETWEEN 1 AND 255),
    lifecycle smallint NOT NULL CHECK (lifecycle IN (1,2)),
    revision bigint NOT NULL CHECK (revision > 0),
    updated_at timestamptz NOT NULL,
    summary text CHECK (octet_length(summary) BETWEEN 1 AND 512),
    description text CHECK (octet_length(description) <= 4096),
    location text CHECK (octet_length(location) <= 512),
    temporal_kind smallint,
    start_date date,
    end_date date,
    anchor_local timestamp without time zone,
    start_at timestamptz,
    end_at timestamptz,
    timezone text CHECK (octet_length(timezone) BETWEEN 1 AND 128),
    timezone_data text CHECK (octet_length(timezone_data) BETWEEN 1 AND 128),
    recurrence_profile text CHECK (octet_length(recurrence_profile) BETWEEN 1 AND 128),
    recurrence_rule text CHECK (octet_length(recurrence_rule) BETWEEN 1 AND 1024),
    PRIMARY KEY(calendar_id,uid),
    UNIQUE(calendar_id,uid,temporal_kind),
    CHECK ((recurrence_profile IS NULL) = (recurrence_rule IS NULL)),
    CHECK (lifecycle=2 OR (summary IS NOT NULL AND description IS NOT NULL AND location IS NOT NULL AND temporal_kind IS NOT NULL)),
    CHECK (lifecycle=1 OR (summary IS NULL AND description IS NULL AND location IS NULL AND temporal_kind IS NULL
        AND start_date IS NULL AND end_date IS NULL AND anchor_local IS NULL AND start_at IS NULL AND end_at IS NULL
        AND timezone IS NULL AND timezone_data IS NULL AND recurrence_profile IS NULL AND recurrence_rule IS NULL)),
    CHECK (lifecycle=2 OR
        (temporal_kind=1 AND start_date IS NOT NULL AND end_date IS NOT NULL
            AND start_date >= DATE '0001-01-01' AND end_date <= DATE '9999-12-31'
            AND end_date > start_date AND end_date-start_date <= 366
            AND anchor_local IS NULL AND start_at IS NULL AND end_at IS NULL AND timezone IS NULL AND timezone_data IS NULL) OR
        (temporal_kind=2 AND start_date IS NULL AND end_date IS NULL
            AND anchor_local IS NOT NULL AND start_at IS NOT NULL AND end_at IS NOT NULL AND timezone IS NOT NULL AND timezone_data IS NOT NULL
            AND anchor_local >= TIMESTAMP '0001-01-01' AND anchor_local < TIMESTAMP '10000-01-01'
            AND start_at >= TIMESTAMPTZ '0001-01-01 00:00:00+00' AND end_at < TIMESTAMPTZ '10000-01-01 00:00:00+00'
            AND date_trunc('second',anchor_local)=anchor_local
            AND date_trunc('second',start_at)=start_at AND date_trunc('second',end_at)=end_at
            AND end_at > start_at AND end_at-start_at <= INTERVAL '366 days')
    )
);
CREATE TABLE public.calendar_dates (
    calendar_id text NOT NULL,
    uid text NOT NULL,
    date_kind smallint NOT NULL CHECK (date_kind IN (1,2)),
    temporal_kind smallint NOT NULL CHECK (temporal_kind IN (1,2)),
    point_key text NOT NULL CHECK (octet_length(point_key) IN (10,19)),
    date_value date,
    local_value timestamp without time zone,
    PRIMARY KEY(calendar_id,uid,date_kind,point_key),
    FOREIGN KEY(calendar_id,uid,temporal_kind) REFERENCES public.calendar_events(calendar_id,uid,temporal_kind),
    CHECK (
        (temporal_kind=1 AND date_value IS NOT NULL AND local_value IS NULL
            AND date_value >= DATE '0001-01-01' AND date_value <= DATE '9999-12-31') OR
        (temporal_kind=2 AND date_value IS NULL AND local_value IS NOT NULL
            AND local_value >= TIMESTAMP '0001-01-01' AND local_value < TIMESTAMP '10000-01-01'
            AND date_trunc('second',local_value)=local_value)
    )
);
CREATE TABLE public.calendar_exceptions (
    calendar_id text NOT NULL,
    uid text NOT NULL,
    temporal_kind smallint NOT NULL CHECK (temporal_kind IN (1,2)),
    point_key text NOT NULL CHECK (octet_length(point_key) IN (10,19)),
    date_value date,
    local_value timestamp without time zone,
    action smallint NOT NULL CHECK (action IN (1,2)),
    start_date date,
    end_date date,
    anchor_local timestamp without time zone,
    start_at timestamptz,
    end_at timestamptz,
    timezone text CHECK (octet_length(timezone) BETWEEN 1 AND 128),
    timezone_data text CHECK (octet_length(timezone_data) BETWEEN 1 AND 128),
    PRIMARY KEY(calendar_id,uid,point_key),
    FOREIGN KEY(calendar_id,uid,temporal_kind) REFERENCES public.calendar_events(calendar_id,uid,temporal_kind),
    CHECK (
        (temporal_kind=1 AND date_value IS NOT NULL AND local_value IS NULL
            AND date_value >= DATE '0001-01-01' AND date_value <= DATE '9999-12-31') OR
        (temporal_kind=2 AND date_value IS NULL AND local_value IS NOT NULL
            AND local_value >= TIMESTAMP '0001-01-01' AND local_value < TIMESTAMP '10000-01-01'
            AND date_trunc('second',local_value)=local_value)
    ),
    CHECK (
        (action=1 AND start_date IS NULL AND end_date IS NULL AND anchor_local IS NULL
            AND start_at IS NULL AND end_at IS NULL AND timezone IS NULL AND timezone_data IS NULL) OR
        (action=2 AND temporal_kind=1 AND start_date IS NOT NULL AND end_date IS NOT NULL
            AND start_date >= DATE '0001-01-01' AND end_date <= DATE '9999-12-31'
            AND end_date > start_date AND end_date-start_date <= 366
            AND anchor_local IS NULL AND start_at IS NULL AND end_at IS NULL AND timezone IS NULL AND timezone_data IS NULL) OR
        (action=2 AND temporal_kind=2 AND start_date IS NULL AND end_date IS NULL
            AND anchor_local IS NOT NULL AND start_at IS NOT NULL AND end_at IS NOT NULL AND timezone IS NOT NULL AND timezone_data IS NOT NULL
            AND anchor_local >= TIMESTAMP '0001-01-01' AND anchor_local < TIMESTAMP '10000-01-01'
            AND start_at >= TIMESTAMPTZ '0001-01-01 00:00:00+00' AND end_at < TIMESTAMPTZ '10000-01-01 00:00:00+00'
            AND date_trunc('second',anchor_local)=anchor_local
            AND date_trunc('second',start_at)=start_at AND date_trunc('second',end_at)=end_at
            AND end_at > start_at AND end_at-start_at <= INTERVAL '366 days')
    )
);
`

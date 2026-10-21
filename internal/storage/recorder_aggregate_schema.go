package storage

// Registered as migration 21. Retention authority remains in the admitted UTC
// cutoff and logical cursor, independent of later migration deliveries.
const recorderAggregateSchemaSQL = `
CREATE TABLE public.recorder_maintenance_head (
 singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
 raw_cutoff timestamptz NOT NULL DEFAULT '0001-01-01T00:00:00Z',
 analytic_run bigint NOT NULL DEFAULT 0 CHECK(analytic_run>=0),
 analytic_sequence numeric(20,0) NOT NULL DEFAULT 0 CHECK(analytic_sequence BETWEEN 0 AND 18446744073709551615),
 revision bigint NOT NULL DEFAULT 0 CHECK(revision>=0)
 ,tail_entity_digest bytea CHECK(tail_entity_digest IS NULL OR octet_length(tail_entity_digest)=32)
);
INSERT INTO public.recorder_maintenance_head(singleton) VALUES(true);
CREATE TABLE public.recorder_retention_ranges (
 epoch bytea NOT NULL REFERENCES public.recorder_epochs(epoch),
 from_sequence numeric(20,0) NOT NULL CHECK(from_sequence BETWEEN 0 AND 18446744073709551615),
 through_sequence numeric(20,0) NOT NULL CHECK(through_sequence BETWEEN 0 AND 18446744073709551615),
 snapshot boolean NOT NULL,
 cutoff timestamptz NOT NULL,
 CHECK(from_sequence<=through_sequence),
 CHECK(snapshot OR from_sequence>0),
 PRIMARY KEY(epoch,from_sequence,through_sequence,snapshot)
);
CREATE TABLE public.recorder_numeric_continuations (
 entity_digest bytea PRIMARY KEY CHECK(octet_length(entity_digest)=32),
 entity_id text CHECK(entity_id IS NULL OR octet_length(entity_id) BETWEEN 1 AND 1024),
 analytical_refusal boolean NOT NULL DEFAULT false,
 point jsonb NOT NULL CHECK(octet_length(point::text)<=4096),
 baseline_pending boolean NOT NULL,
 high_water_seconds bigint NOT NULL,
 high_water_nanos integer NOT NULL CHECK(high_water_nanos BETWEEN 0 AND 999999999),
 covered_through timestamptz NOT NULL,
 last_observation_at timestamptz NOT NULL,
 CHECK((analytical_refusal AND entity_id IS NULL AND NOT baseline_pending) OR (NOT analytical_refusal AND entity_id IS NOT NULL))
);
CREATE INDEX recorder_continuations_retention ON public.recorder_numeric_continuations(last_observation_at,entity_digest);
CREATE TABLE public.recorder_analytical_breaks (
 entity_digest bytea NOT NULL CHECK(octet_length(entity_digest)=32),
 start_at timestamptz NOT NULL,
 end_at timestamptz NOT NULL,
 reasons integer NOT NULL CHECK(reasons BETWEEN 1 AND 4095),
 CHECK(start_at<=end_at),
 PRIMARY KEY(entity_digest,start_at,end_at)
);
CREATE INDEX recorder_analytical_breaks_retention ON public.recorder_analytical_breaks(end_at,entity_digest,start_at);
CREATE TABLE public.recorder_hourly_aggregates (
 entity_digest bytea NOT NULL CHECK(octet_length(entity_digest)=32),
 entity_id text NOT NULL CHECK(octet_length(entity_id) BETWEEN 1 AND 1024),
 bucket_start timestamptz NOT NULL,
 bucket_end timestamptz NOT NULL,
 group_digest bytea NOT NULL CHECK(octet_length(group_digest)=32),
 descriptor jsonb NOT NULL CHECK(octet_length(descriptor::text)<=2048),
 availability smallint NOT NULL CHECK(availability BETWEEN 1 AND 3),
 freshness smallint NOT NULL CHECK(freshness BETWEEN 1 AND 2),
 provenance smallint NOT NULL CHECK(provenance BETWEEN 1 AND 3),
 duration_ns bigint NOT NULL CHECK(duration_ns BETWEEN 0 AND 3600000000000),
 weighted double precision NOT NULL CHECK(weighted NOT IN ('NaN'::float8,'Infinity'::float8,'-Infinity'::float8)),
 minimum double precision,
 maximum double precision,
 contribution double precision,
 reset_kind smallint NOT NULL CHECK(reset_kind BETWEEN 1 AND 4),
 reasons integer NOT NULL CHECK(reasons BETWEEN 0 AND 4095),
 from_epoch bytea NOT NULL REFERENCES public.recorder_epochs(epoch),
 from_sequence numeric(20,0) NOT NULL CHECK(from_sequence BETWEEN 0 AND 18446744073709551615),
 through_epoch bytea NOT NULL REFERENCES public.recorder_epochs(epoch),
 through_sequence numeric(20,0) NOT NULL CHECK(through_sequence BETWEEN 1 AND 18446744073709551615),
 CHECK(bucket_end=bucket_start+interval '1 hour'),
 CHECK((minimum IS NULL)=(maximum IS NULL)),
 CHECK(minimum IS NULL OR minimum<=maximum),
 CHECK(duration_ns>0 OR weighted=0),
 CHECK(duration_ns=0 OR reset_kind=1),
 CHECK(reset_kind<>1 OR contribution IS NULL),
 CHECK(reset_kind<>4 OR contribution IS NOT NULL),
 CHECK((minimum IS NULL)=(duration_ns=0)),
 CHECK(minimum IS NULL OR minimum NOT IN ('NaN'::float8,'Infinity'::float8,'-Infinity'::float8)),
 CHECK(maximum IS NULL OR maximum NOT IN ('NaN'::float8,'Infinity'::float8,'-Infinity'::float8)),
 CHECK(contribution IS NULL OR contribution NOT IN ('NaN'::float8,'Infinity'::float8,'-Infinity'::float8)),
 PRIMARY KEY(entity_digest,bucket_start,group_digest)
);
CREATE INDEX recorder_aggregates_retention ON public.recorder_hourly_aggregates(bucket_end,entity_digest,bucket_start,group_digest);
CREATE INDEX recorder_snapshots_retention ON public.recorder_snapshots(received_at,epoch,boundary_sequence,entity_digest);
CREATE INDEX secret_access_audit_retention ON public.secret_access_audit(recorded_at,id);
`

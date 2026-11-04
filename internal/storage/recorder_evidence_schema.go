package storage

// This fragment is composed into the undelivered migration 29.
// NULL means no admission proof: preserve such data but never reconstruct its
// historical permissions from the current capability catalogue.
const recorderEvidenceSchemaSQL = `
ALTER TABLE public.recorder_epochs ADD COLUMN evidence_version smallint CHECK(evidence_version IS NULL OR evidence_version=1);
CREATE INDEX recorder_epochs_provenance ON public.recorder_epochs(started_at) WHERE evidence_version=1;
ALTER TABLE public.state_history ADD COLUMN admission_evidence bytea CHECK(admission_evidence IS NULL OR octet_length(admission_evidence) BETWEEN 1 AND 40960);
ALTER TABLE public.recorder_numeric_continuations ADD COLUMN admission_evidence bytea CHECK(admission_evidence IS NULL OR octet_length(admission_evidence) BETWEEN 1 AND 40960);
ALTER TABLE public.recorder_hourly_aggregates ADD COLUMN admission_evidence bytea CHECK(admission_evidence IS NULL OR octet_length(admission_evidence) BETWEEN 1 AND 40960);
CREATE INDEX state_history_provenance ON public.state_history(entity_digest,epoch,sequence) WHERE admission_evidence IS NOT NULL;
CREATE INDEX recorder_aggregates_provenance ON public.recorder_hourly_aggregates(entity_digest,bucket_start,group_digest) WHERE admission_evidence IS NOT NULL;
`

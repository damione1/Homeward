package storage

// SQL30 adds the durable calendar delivery ledger. SQL29 is authenticated for
// migration only; it cannot read/write an active SQL30 installation.
const schemaWindowBaseline uint32 = 29
const schemaWindowSuccessor uint32 = 30
const schemaWindowBaselineRelease = "homeward-window-29-30-baseline"
const schemaWindowSuccessorRelease = "homeward-window-29-30-successor"
const compiledSchemaReleaseID = schemaWindowSuccessorRelease
const compiledMinimumReaderID = schemaWindowSuccessorRelease
const compiledMinimumWriterID = schemaWindowSuccessorRelease

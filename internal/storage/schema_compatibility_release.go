package storage

// SQL31 converts stored fan observations. SQL30 is authenticated for migration
// only; its closed enum reader cannot access the new live state contract.
const schemaWindowBaseline uint32 = 30
const schemaWindowSuccessor uint32 = 31
const schemaWindowBaselineRelease = "homeward-window-30-31-baseline"
const schemaWindowSuccessorRelease = "homeward-window-30-31-successor"
const compiledSchemaReleaseID = schemaWindowSuccessorRelease
const compiledMinimumReaderID = schemaWindowSuccessorRelease
const compiledMinimumWriterID = schemaWindowSuccessorRelease

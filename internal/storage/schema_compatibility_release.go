package storage

// SQL32 persists response methods for native configuration operations without
// request IDs. SQL31 is authenticated for migration, not admitted as a writer.
const schemaWindowBaseline uint32 = 31
const schemaWindowSuccessor uint32 = 32
const schemaWindowBaselineRelease = "homeward-window-31-32-baseline"
const schemaWindowSuccessorRelease = "homeward-window-31-32-successor"
const compiledSchemaReleaseID = schemaWindowSuccessorRelease
const compiledMinimumReaderID = schemaWindowSuccessorRelease
const compiledMinimumWriterID = schemaWindowSuccessorRelease

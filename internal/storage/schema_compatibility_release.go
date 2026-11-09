package storage

// SQL33 upgrades persisted compiler inputs to the current fan wire contract.
// SQL32 is authenticated only for migration, never admitted as a writer.
const schemaWindowBaseline uint32 = 32
const schemaWindowSuccessor uint32 = 33
const schemaWindowBaselineRelease = "homeward-window-32-33-baseline"
const schemaWindowSuccessorRelease = "homeward-window-32-33-successor"
const compiledSchemaReleaseID = schemaWindowSuccessorRelease
const compiledMinimumReaderID = schemaWindowSuccessorRelease
const compiledMinimumWriterID = schemaWindowSuccessorRelease

package storage

// SQL29 is the only active reader/writer profile. Historical windows are
// authenticated solely during data migration; they confer no runtime access.
const schemaWindowBaseline uint32 = 29
const schemaWindowSuccessor uint32 = 30
const schemaWindowBaselineRelease = "homeward-window-29-30-baseline"
const schemaWindowSuccessorRelease = "homeward-window-29-30-successor"
const compiledSchemaReleaseID = schemaWindowBaselineRelease
const compiledMinimumReaderID = schemaWindowBaselineRelease
const compiledMinimumWriterID = schemaWindowBaselineRelease

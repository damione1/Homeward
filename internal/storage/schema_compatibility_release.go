package storage

// This is the complete, explicit compiled pair. SQL ordinals and release IDs
// are independent. A later real release replaces these literals deliberately;
// no process can select a profile or acquire older-reader compatibility.
const schemaWindowBaseline uint32 = 28
const schemaWindowSuccessor uint32 = 29
const schemaWindowBaselineRelease = "homeward-window-28-29-baseline"
const schemaWindowSuccessorRelease = "homeward-window-28-29-successor"
const compiledSchemaReleaseID = "homeward-window-28-29-baseline"
const compiledMinimumReaderID = "homeward-window-28-29-baseline"
const compiledMinimumWriterID = "homeward-window-28-29-baseline"

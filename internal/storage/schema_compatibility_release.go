package storage

// This is the complete, explicit compiled pair. SQL ordinals and release IDs
// are independent. A later real release replaces these literals deliberately;
// no process can select a profile or acquire older-reader compatibility.
const schemaWindowBaseline uint32 = 25
const schemaWindowSuccessor uint32 = 26
const schemaWindowBaselineRelease = "homeward-window-25-26-baseline"
const schemaWindowSuccessorRelease = "homeward-window-25-26-successor"
const compiledSchemaReleaseID = "homeward-window-25-26-baseline"
const compiledMinimumReaderID = "homeward-window-25-26-baseline"
const compiledMinimumWriterID = "homeward-window-25-26-baseline"

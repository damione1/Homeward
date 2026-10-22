package storage

// This is the complete, explicit compiled pair. SQL ordinals and release IDs
// are independent. A later real release replaces these literals deliberately;
// no process can select a profile or acquire older-reader compatibility.
const schemaWindowBaseline uint32 = 26
const schemaWindowSuccessor uint32 = 27
const schemaWindowBaselineRelease = "homeward-window-26-27-baseline"
const schemaWindowSuccessorRelease = "homeward-window-26-27-successor"
const compiledSchemaReleaseID = "homeward-window-26-27-baseline"
const compiledMinimumReaderID = "homeward-window-26-27-baseline"
const compiledMinimumWriterID = "homeward-window-26-27-baseline"

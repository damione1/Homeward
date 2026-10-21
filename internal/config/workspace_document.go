package config

// WorkspaceDocumentLocation exposes only the already selected operational
// locator to the trusted workspace owner. It never chooses a new launch source,
// resolves environment values or accepts a locator from an RPC.
func (s StartupInputs) WorkspaceDocumentLocation() (physical, logical string, ok bool) {
	if !s.Valid() || s.workspacePath == "" {
		return "", "", false
	}
	return s.workspacePath, s.logicalPath, true
}

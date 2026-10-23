// Package processbudget configures resources owned by an executable process.
// Library entrypoints such as server.Run deliberately do not change global Go
// runtime settings in an embedding application.
package processbudget

import "runtime/debug"

// ServerGoMemoryLimit is the permanent homed Go-managed-memory soft budget.
// The remaining96MiB under the MVP256MiB RSS threshold allows room for the
// executable, OS and memory outside Go. This is not a hard RSS guarantee:
// qualification still measures the complete process high-water RSS.
const ServerGoMemoryLimit int64 = 160 << 20

// ConfigureServer is called once by the process owner before server startup.
// A stricter operator GOMEMLIMIT/runtime setting is preserved. The Go runtime
// naturally adjusts collection/scavenging to its soft limit; no explicit GC or
// change to GOGC, cryptographic cost, worker count or server admission is made.
func ConfigureServer() int64 {
	current := debug.SetMemoryLimit(-1)
	if current <= ServerGoMemoryLimit {
		return current
	}
	debug.SetMemoryLimit(ServerGoMemoryLimit)
	return ServerGoMemoryLimit
}

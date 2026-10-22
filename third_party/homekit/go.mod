module github.com/mctofu/homekit

go 1.27.2

require (
	github.com/brutella/hc v1.2.5
	github.com/google/uuid v1.6.0
)

require (
	github.com/tadglines/go-pkgs v0.0.0-20140924210655-1f86682992f1 // indirect
	github.com/xiam/to v0.0.0-20191116183551-8328998fc0ed // indirect
	golang.org/x/crypto v0.31.0 // indirect
	golang.org/x/sys v0.28.0 // indirect
	golang.org/x/text v0.21.0 // indirect
)

// Reuse exactly the transcript-free hc source and cached dependency versions qualified by NHA-009.
replace github.com/brutella/hc => ../../qualification/ecobee/third_party/hc

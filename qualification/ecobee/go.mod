module github.com/damione1/Homeward/qualification/ecobee

go 1.27.2

require (
	github.com/brutella/hc v1.2.5
	github.com/mctofu/homekit v0.0.0-20250114051306-55b8dd50cddc
)

require (
	github.com/cenkalti/backoff v2.2.1+incompatible // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/grandcat/zeroconf v1.0.0 // indirect
	github.com/miekg/dns v1.1.27 // indirect
	github.com/tadglines/go-pkgs v0.0.0-20140924210655-1f86682992f1 // indirect
	github.com/xiam/to v0.0.0-20191116183551-8328998fc0ed // indirect
	golang.org/x/crypto v0.31.0 // indirect
	golang.org/x/net v0.33.0 // indirect
	golang.org/x/sys v0.28.0 // indirect
	golang.org/x/text v0.21.0 // indirect
)

// The qualification-only copy removes pairing transcript prints; see third_party/hc.upstream.json.
replace github.com/brutella/hc => ./third_party/hc

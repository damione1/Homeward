module github.com/damione1/Homeward

go 1.27.2

require (
	connectrpc.com/connect v1.21.0
	filippo.io/age v1.3.2
	github.com/arran4/golang-ical v0.3.8
	github.com/aws/aws-sdk-go-v2 v1.45.1
	github.com/aws/aws-sdk-go-v2/service/s3 v1.109.1
	github.com/aws/smithy-go v1.28.1
	github.com/brutella/hc v1.2.5
	github.com/coder/websocket v1.8.15
	github.com/eclipse/paho.golang v0.23.0
	github.com/fxamacker/cbor/v2 v2.9.6
	github.com/go-webauthn/webauthn v0.18.2
	github.com/jackc/pgx/v5 v5.10.0
	github.com/mctofu/homekit v0.0.0-20250114051306-55b8dd50cddc
	github.com/mochi-mqtt/server/v2 v2.7.9
	github.com/nikolalohinski/gonja/v2 v2.9.1
	github.com/teambition/rrule-go v1.8.2
	go.yaml.in/yaml/v3 v3.0.5
	golang.org/x/crypto v0.58.0
	golang.org/x/net v0.60.0
	golang.org/x/sys v0.49.0
	golang.org/x/text v0.43.0
	google.golang.org/protobuf v1.36.11
)

require (
	filippo.io/hpke v0.4.0 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.20 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.5.1 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.8.1 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.5.1 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.19 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.11.1 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.14.1 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.20.1 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/go-webauthn/x v0.3.1 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/google/go-tpm v0.9.8 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/philhofer/fwd v1.2.0 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/rogpeppe/go-internal v1.16.0 // indirect
	github.com/rs/xid v1.4.0 // indirect
	github.com/sirupsen/logrus v1.9.3 // indirect
	github.com/tadglines/go-pkgs v0.0.0-20140924210655-1f86682992f1 // indirect
	github.com/tinylib/msgp v1.6.4 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	github.com/xiam/to v0.0.0-20191116183551-8328998fc0ed // indirect
	golang.org/x/exp v0.0.0-20240719175910-8a7402abbf56 // indirect
	golang.org/x/sync v0.24.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// NHA-136 reuses the existing transcript-free hc copy; no duplicate library.
replace github.com/brutella/hc => ./qualification/ecobee/third_party/hc

replace github.com/mctofu/homekit => ./third_party/homekit

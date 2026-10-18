package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/damione1/Homeward/internal/config"
)

func TestNHA181PublicTransportAdmission(t *testing.T) {
	parse := func(listen, public string) (config.Config, error) {
		return config.ParseWithEnv([]byte("listen: '"+listen+"'\n"+public+testPostgres(t)), testLookup)
	}
	legacy, err := parse("0.0.0.0:0", "")
	if err != nil {
		t.Fatalf("legacy wildcard must still parse: %v", err)
	}
	settings, err := legacy.PublicTransport()
	if err != nil || settings.Transport() != config.PublicTransportH2C {
		t.Fatalf("legacy mode: %v, %v", settings.Transport(), err)
	}
	if _, present := settings.TLSFiles(); present {
		t.Fatal("legacy h2c acquired TLS files")
	}
	if _, err := parse("127.0.0.1:0", "public:\n  transport: h2c\n  url: https://proxy.example\n"); err != nil {
		t.Fatalf("h2c proxy metadata changed admission: %v", err)
	}
	tlsConfig, err := parse("0.0.0.0:0", "public:\n  transport: tls\n  tls:\n    certificate_file: public.crt\n    private_key_file: public.key\n")
	if err != nil {
		t.Fatal(err)
	}
	settings, err = tlsConfig.PublicTransport()
	if err != nil || settings.Transport() != config.PublicTransportTLS {
		t.Fatalf("TLS mode: %v, %v", settings.Transport(), err)
	}
	files, present := settings.TLSFiles()
	if !present || files.CertificateFile() != "public.crt" || files.PrivateKeyFile() != "public.key" {
		t.Fatalf("TLS pair was not retained: present=%v", present)
	}
	if _, err := parse("127.0.0.1:0", "public:\n  transport: tls\n  url: https://home.example\n  tls:\n    certificate_file: public.crt\n    private_key_file: public.key\n"); err != nil {
		t.Fatalf("https canonical TLS URL rejected: %v", err)
	}
	if (config.PublicSettings{}).Transport() != config.PublicTransportUnspecified {
		t.Fatal("zero public settings became a serving transport")
	}
}

func TestNHA181PublicTransportRejectsPartialAndUnsafeFiles(t *testing.T) {
	for _, public := range []string{
		"public:\n  transport: ''\n",
		"public:\n  transport: https\n",
		"public:\n  transport: h2c\n  tls:\n    certificate_file: public.crt\n    private_key_file: public.key\n",
		"public:\n  transport: tls\n",
		"public:\n  transport: tls\n  tls: {}\n",
		"public:\n  transport: tls\n  tls:\n    certificate_file: public.crt\n",
		"public:\n  transport: tls\n  tls:\n    certificate_file: public.crt\n    private_key_file: public.crt\n",
		"public:\n  transport: tls\n  url: http://127.0.0.1:8787\n  tls:\n    certificate_file: public.crt\n    private_key_file: public.key\n",
		"public:\n  transport: tls\n  tls:\n    certificate_file: ../public.crt\n    private_key_file: public.key\n",
		"public:\n  transport: tls\n  tls:\n    certificate_file: /tmp/public.crt\n    private_key_file: public.key\n",
		"public:\n  transport: tls\n  tls:\n    certificate_file: public.crt\n    private_key_file: '..'\n",
		"public:\n  transport: tls\n  tls:\n    certificate_file: public.crt\n    private_key_file: 'bad\\name'\n",
		"public:\n  transport: tls\n  tls:\n    certificate_file: public.crt\n    private_key_file: 'secret key'\n",
		"public:\n  transport: tls\n  tls:\n    certificate_file: public.crt\n    private_key_file: '" + strings.Repeat("k", 129) + "'\n",
	} {
		t.Run(public, func(t *testing.T) {
			_, err := config.ParseWithEnv([]byte("listen: '127.0.0.1:0'\n"+public+testPostgres(t)), testLookup)
			if !errors.Is(err, config.ErrInvalidPublicSettings) {
				t.Fatalf("unexpected admission result: %v", err)
			}
		})
	}
}

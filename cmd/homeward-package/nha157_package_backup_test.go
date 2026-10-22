package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNHA157PackageInstallationUsesPrivateCredentialEnvironment(t *testing.T) {
	_, credentials, _ := packageFixture(t)
	password := strings.Repeat("a", 64)
	if err := os.WriteFile(filepath.Join(credentials, "password"), []byte(password), 0600); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"backup", "restore"} {
		argv, env, err := installationInvocation([]string{operation, "--instance", "local"}, credentials, []string{"HOMEWARD_POSTGRES_PASSWORD=stale", "HOMEWARD_POSTGRES_PASSWORD=second-stale", "OTHER=preserved"})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.Join(argv, " "), password) || !strings.Contains(strings.Join(argv, " "), "/opt/homeward/pg-client/bin") {
			t.Fatal("invalid operator argv")
		}
		found := 0
		for _, entry := range env {
			if strings.HasPrefix(entry, "HOMEWARD_POSTGRES_PASSWORD=") {
				found++
				if entry != "HOMEWARD_POSTGRES_PASSWORD="+password {
					t.Fatal("wrong private credential")
				}
			}
		}
		if found != 1 {
			t.Fatal("ambiguous credential environment")
		}
		if argv[len(argv)-2] != "--instance" || argv[len(argv)-1] != "local" || !strings.Contains(strings.Join(env, "\n"), "OTHER=preserved") {
			t.Fatal("operator options or unrelated environment were lost")
		}
		if operation == "backup" && !strings.Contains(strings.Join(argv, " "), "/var/lib/homeward/configuration.yaml.bootstrap.yaml") {
			t.Fatal("backup selected operational workspace instead of launch bootstrap")
		}
	}
}

func TestNHA157PackageInstallationRejectsLostCredentialsAndUnknownOperations(t *testing.T) {
	_, credentials, _ := packageFixture(t)
	for _, args := range [][]string{nil, {"serve"}, {"unknown"}, {"backup"}, {"restore"}} {
		if argv, env, err := installationInvocation(args, credentials, nil); err == nil || argv != nil || env != nil {
			t.Fatal("invalid local operation admitted")
		}
	}
	if _, err := os.Stat(filepath.Join(credentials, "password")); !os.IsNotExist(err) {
		t.Fatal("operator invocation generated credentials")
	}
}

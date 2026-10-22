package ecobee

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mctofu/homekit/client"
)

func TestNHA136WrongAccessoryPublicKeyNeverReadsProtectedState(t *testing.T) {
	f := newOrdinaryFixture(t)
	legitimate := f.NewClient()
	ctx, cancel := context.WithTimeout(f.Ctx, 5*time.Second)
	defer cancel()
	if _, err := legitimate.API.Accessories(ctx); err != nil {
		t.Fatal("positive identity witness", err)
	}
	expectedReads := f.CharacteristicGET.Load()
	expectedPUT := f.CharacteristicPUT.Load()
	wrong := *f.Pairing
	wrong.PublicKey = append([]byte(nil), f.Pairing.PublicKey...)
	wrong.PublicKey[0] ^= 1
	rejected := client.NewAccessoryClient(f.DialLoopback, f.Controller, &wrong)
	defer rejected.Close()
	if _, err := rejected.Accessories(ctx); err == nil || ctx.Err() != nil {
		t.Fatal("wrong accessory key lacked explicit rejection", err)
	}
	if f.CharacteristicGET.Load() != expectedReads || f.CharacteristicPUT.Load() != expectedPUT {
		t.Fatal("wrong identity reached protected state/action")
	}
}

func TestNHA136SyntheticPairSetupDoesNotPrintCredentials(t *testing.T) {
	oldOut, oldErr := os.Stdout, os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = writer, writer
	defer func() { os.Stdout, os.Stderr = oldOut, oldErr; writer.Close(); reader.Close() }()
	captured := make(chan []byte, 1)
	go func() { data, _ := io.ReadAll(reader); captured <- data }()
	fixture := newOrdinaryFixture(t)
	os.Stdout, os.Stderr = oldOut, oldErr
	writer.Close()
	output := <-captured
	if strings.TrimSpace(string(output)) != "" || bytes.Contains(output, fixture.Controller.PrivateKey) {
		t.Fatal("synthetic pairing transcript reached stdout/stderr")
	}
}

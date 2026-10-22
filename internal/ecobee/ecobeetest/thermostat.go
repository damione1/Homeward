// Package ecobeetest provides a loopback-only synthetic HAP accessory for tests.
// All Pairing keys are freshly generated test material; no LAN discovery occurs.
package ecobeetest

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brutella/hc/accessory"
	"github.com/brutella/hc/characteristic"
	"github.com/brutella/hc/crypto"
	"github.com/brutella/hc/db"
	"github.com/brutella/hc/event"
	"github.com/brutella/hc/hap"
	haphttp "github.com/brutella/hc/hap/http"
	"github.com/mctofu/homekit/client"
)

const operationTimeout = 5 * time.Second

// Adapted from mctofu/homekit/client/setup_test.go at 55b8dd50cddc.
// Copyright (c) 2020 David McIntosh, MIT; see third_party/NOTICE and LICENSE.mctofu-homekit.
// The lock protects the fixture session's deferred crypto transition, not network I/O.
type synchronizedContext struct{ hap.Context }

func (c *synchronizedContext) SetSessionForConnection(s hap.Session, conn net.Conn) {
	c.Context.SetSessionForConnection(&synchronizedSession{Session: s}, conn)
}

type synchronizedSession struct {
	hap.Session
	mu sync.Mutex
}

func (s *synchronizedSession) Decrypter() crypto.Decrypter {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Session.Decrypter()
}

func (s *synchronizedSession) Encrypter() crypto.Encrypter {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Session.Encrypter()
}

func (s *synchronizedSession) SetCryptographer(c crypto.Cryptographer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Session.SetCryptographer(c)
}

var _ hap.Context = (*synchronizedContext)(nil)
var _ hap.Session = (*synchronizedSession)(nil)

type memoryStore struct {
	mu     sync.Mutex
	values map[string][]byte
}

func (m *memoryStore) Set(key string, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[key] = append([]byte(nil), value...)
	return nil
}

func (m *memoryStore) Delete(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.values, key)
	return nil
}

func (m *memoryStore) Get(key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.values[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), v...), nil
}

func (m *memoryStore) KeysWithSuffix(suffix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keys []string
	for key := range m.values {
		if strings.HasSuffix(key, suffix) {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

// observedConnection counts successful ordinary encrypted-frame writes. It never
// modifies traffic or retains/dumps any plaintext, ciphertext, identity or key.
type observedConnection struct {
	net.Conn
	encryptedWrites atomic.Int32
}

func (c *observedConnection) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if err == nil && n == len(p) && len(p) > 18 && int(binary.LittleEndian.Uint16(p[:2])) == len(p)-18 {
		c.encryptedWrites.Add(1)
	}
	return n, err
}

type OwnedClient struct {
	API       *client.AccessoryClient
	mu        sync.Mutex
	wire      *observedConnection
	closeOnce sync.Once
}

func (c *OwnedClient) connection() *observedConnection {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wire
}

func (c *OwnedClient) close() error {
	var err error
	c.closeOnce.Do(func() { err = c.API.Close() })
	return err
}

type Fixture struct {
	identities  map[string]client.CharacteristicReadRequest
	Ctx         context.Context
	Cancel      context.CancelFunc
	hapCtx      *synchronizedContext
	server      *httptest.Server
	setupHTTP   *http.Transport
	Address     string
	port        int
	clients     []*OwnedClient
	handlerMu   sync.Mutex
	interceptor atomic.Pointer[Interceptor]
	requests    map[string]connectionRequests

	Controller *client.ControllerIdentity
	Pairing    *client.AccessoryConnectionConfig

	TemperatureWrites atomic.Int32
	ModeWrites        atomic.Int32
	verifyRequests    atomic.Int32
	CharacteristicGET atomic.Int32
	CharacteristicPUT atomic.Int32

	blockNext   atomic.Bool
	entered     chan struct{}
	release     chan struct{}
	exited      chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
	exitedOnce  sync.Once
	workers     sync.WaitGroup
	closeOnce   sync.Once
}

type connectionRequests struct {
	verify, accessoriesGET, characteristicsGET, characteristicsPUT int
}

func New(t *testing.T) *Fixture {
	t.Helper()
	Ctx, Cancel := context.WithCancel(context.Background())
	f := &Fixture{
		Ctx: Ctx, Cancel: Cancel,
		entered: make(chan struct{}), release: make(chan struct{}), exited: make(chan struct{}),
		requests: make(map[string]connectionRequests),
	}
	t.Cleanup(func() { f.close(t) })

	const syntheticPIN = "111-22-333"
	const syntheticDevice = "CA:FE:00:11:22:33"
	database := db.NewDatabaseWithStorage(&memoryStore{values: make(map[string][]byte)})
	device, err := hap.NewSecuredDevice(syntheticDevice, syntheticPIN, database)
	if err != nil {
		t.Fatal("create synthetic accessory:", err)
	}
	thermo := accessory.NewThermostat(accessory.Info{
		Name: "NHA136 synthetic thermostat", Manufacturer: "Homeward qualification",
		Model: "ECB601", SerialNumber: "0001",
	}, 21.5, 0, 100, 0.1)
	thermostat := thermo.Thermostat
	thermostat.CurrentTemperature.SetValue(21.5)
	thermostat.TargetTemperature.SetValue(21.5)
	thermostat.CurrentHeatingCoolingState.SetValue(0)
	thermostat.TargetHeatingCoolingState.SetValue(1)
	thermostat.TargetHeatingCoolingState.MinValue = 0
	thermostat.TargetHeatingCoolingState.MaxValue = 3
	thermostat.TargetHeatingCoolingState.StepValue = 1
	thermostat.TemperatureDisplayUnits.SetValue(0)
	humidity := characteristic.NewCurrentRelativeHumidity()
	humidity.SetValue(45)
	thermostat.AddCharacteristic(humidity.Characteristic)
	currentFan := characteristic.NewCurrentFanState()
	currentFan.SetValue(0)
	thermostat.AddCharacteristic(currentFan.Characteristic)
	targetFan := characteristic.NewTargetFanState()
	targetFan.SetValue(1)
	targetFan.MinValue = 0
	targetFan.MaxValue = 1
	targetFan.StepValue = 1
	heatThreshold := characteristic.NewHeatingThresholdTemperature()
	heatThreshold.SetValue(20.5)
	thermostat.AddCharacteristic(heatThreshold.Characteristic)
	coolThreshold := characteristic.NewCoolingThresholdTemperature()
	coolThreshold.SetValue(24.5)
	thermostat.AddCharacteristic(coolThreshold.Characteristic)
	thermostat.AddCharacteristic(targetFan.Characteristic)
	thermostat.TargetTemperature.OnValueRemoteUpdate(func(float64) { f.TemperatureWrites.Add(1) })
	thermostat.TargetHeatingCoolingState.OnValueRemoteUpdate(func(int) { f.ModeWrites.Add(1) })
	// Install the callback before any server goroutine can access the characteristic.
	thermostat.CurrentTemperature.OnValueRemoteGet(func() float64 {
		if f.blockNext.CompareAndSwap(true, false) {
			f.enteredOnce.Do(func() { close(f.entered) })
			defer f.exitedOnce.Do(func() { close(f.exited) })
			select {
			case <-f.release:
			case <-f.Ctx.Done():
			}
		}
		return 21.5
	})
	container := accessory.NewContainer()
	if err := container.AddAccessory(thermo.Accessory); err != nil {
		t.Fatal("add synthetic accessory:", err)
	}
	f.identities = make(map[string]client.CharacteristicReadRequest)
	for _, service := range thermo.Accessory.GetServices() {
		for _, characteristic := range service.GetCharacteristics() {
			f.identities[characteristic.Type] = client.CharacteristicReadRequest{AccessoryID: thermo.Accessory.ID, CharacteristicID: characteristic.ID}
		}
	}
	f.hapCtx = &synchronizedContext{hap.NewContextForSecuredDevice(device)}
	hapServer := haphttp.NewServer(haphttp.Config{
		Port: "127.0.0.1:0", Context: f.hapCtx, Database: database, Device: device,
		Container: container, Mutex: &sync.Mutex{}, Emitter: event.NewEmitter(),
	})
	// NewServer already owns a listening socket; Close below must own it even
	// if a later setup assertion fails. No hc ListenAndServe cancellation loop.
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// hc characteristic values are mutable; serialize fixture handlers across
		// the old and replacement sessions. Background connection reads still run.
		if hook := f.interceptor.Load(); hook != nil && (*hook)(w, r) {
			return
		}
		f.handlerMu.Lock()
		defer f.handlerMu.Unlock()
		counts := f.requests[r.RemoteAddr]
		if r.URL.Path == "/pair-verify" {
			f.verifyRequests.Add(1)
			counts.verify++
		}
		if r.Method == http.MethodGet && r.URL.Path == "/accessories" {
			counts.accessoriesGET++
		}
		if r.Method == http.MethodGet && r.URL.Path == "/characteristics" {
			f.CharacteristicGET.Add(1)
			counts.characteristicsGET++
		}
		// Count every received PUT before hc can discard a same-value update.
		if r.Method == http.MethodPut && r.URL.Path == "/characteristics" {
			f.CharacteristicPUT.Add(1)
			counts.characteristicsPUT++
		}
		f.requests[r.RemoteAddr] = counts
		hapServer.Mux.ServeHTTP(w, r)
	}))
	initialListener := f.server.Listener
	f.server.Listener = hapServer
	if err := initialListener.Close(); err != nil {
		t.Fatal("close unused httptest listener:", err)
	}
	f.server.Start()
	f.port, err = strconv.Atoi(hapServer.Port())
	if err != nil {
		t.Fatal("synthetic port:", err)
	}
	f.Address = net.JoinHostPort("127.0.0.1", strconv.Itoa(f.port))
	f.setupHTTP = &http.Transport{DialContext: f.DialLoopback, DisableCompression: true}
	f.Controller, err = client.NewRandomControllerConfig()
	if err != nil {
		t.Fatal("create synthetic Controller:", err)
	}
	pairCtx, pairCancel := context.WithTimeout(f.Ctx, 10*time.Second)
	defer pairCancel()
	f.Pairing, err = client.NewSetupClient(&http.Client{Transport: f.setupHTTP}).Pair(pairCtx,
		&client.AccessoryPairingConfig{
			PIN: syntheticPIN, DeviceID: syntheticDevice,
			IPConnectionInfo: client.IPConnectionInfo{IPAddress: "127.0.0.1", Port: f.port},
		}, f.Controller)
	if err != nil {
		t.Fatal("ordinary synthetic pair-setup:", err)
	}
	return f
}

func (f *Fixture) DialLoopback(Ctx context.Context, network, Address string) (net.Conn, error) {
	if network != "tcp" || Address != f.Address {
		return nil, fmt.Errorf("qualification dial outside the owned loopback listener")
	}
	return (&net.Dialer{}).DialContext(Ctx, network, Address)
}

func (f *Fixture) NewClient() *OwnedClient {
	return f.NewClientWithIdentity(f.Controller)
}

func (f *Fixture) NewClientWithIdentity(identity *client.ControllerIdentity) *OwnedClient {
	c := &OwnedClient{}
	c.API = client.NewAccessoryClient(func(Ctx context.Context, network, Address string) (net.Conn, error) {
		raw, err := f.DialLoopback(Ctx, network, Address)
		if err != nil {
			return nil, err
		}
		wire := &observedConnection{Conn: raw}
		c.mu.Lock()
		c.wire = wire
		c.mu.Unlock()
		return wire, nil
	}, identity, f.Pairing)
	f.clients = append(f.clients, c)
	return c
}

func (f *Fixture) requestsFor(remote string) connectionRequests {
	f.handlerMu.Lock()
	defer f.handlerMu.Unlock()
	return f.requests[remote]
}

func (f *Fixture) unblock() {
	f.releaseOnce.Do(func() { close(f.release) })
}

func (f *Fixture) close(t *testing.T) {
	t.Helper()
	f.closeOnce.Do(func() {
		f.Cancel()
		f.unblock()
		for _, c := range f.clients {
			if err := c.close(); err != nil {
				t.Error("close synthetic client:", err)
			}
		}
		if f.setupHTTP != nil {
			f.setupHTTP.CloseIdleConnections()
		}
		workersDone := make(chan struct{})
		go func() { f.workers.Wait(); close(workersDone) }()
		waitShutdown(t, workersDone, "synthetic request goroutine stopped")
		if f.server != nil {
			serverDone := make(chan struct{})
			go func() { f.server.Close(); close(serverDone) }()
			waitShutdown(t, serverDone, "synthetic server and handlers stopped")
		}
		if f.hapCtx != nil && len(f.hapCtx.ActiveConnections()) != 0 {
			t.Error("synthetic HAP sessions remain after shutdown")
		}
	})
}

func waitShutdown(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(operationTimeout):
		t.Error(description + ": timeout")
	}
}

func waitSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(operationTimeout):
		t.Fatal(description + ": timeout")
	}
}

// Interceptor is a test barrier around the owned server, never a product hook.
type Interceptor func(http.ResponseWriter, *http.Request) bool

func (f *Fixture) SetInterceptor(hook Interceptor) {
	if hook == nil {
		f.interceptor.Store(nil)
		return
	}
	f.interceptor.Store(&hook)
}

func (f *Fixture) VerifyRequests() int32 { return f.verifyRequests.Load() }

func (f *Fixture) Close(t *testing.T) { f.close(t) }

// CharacteristicIdentity exposes the immutable fake's literal HAP metadata to
// independent wire oracles; it never consults the production climate translator.
func (f *Fixture) CharacteristicIdentity(hapType string) (client.CharacteristicReadRequest, bool) {
	id, ok := f.identities[hapType]
	return id, ok
}

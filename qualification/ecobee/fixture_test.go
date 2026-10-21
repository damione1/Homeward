package ecobee

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

type ownedClient struct {
	api       *client.AccessoryClient
	mu        sync.Mutex
	wire      *observedConnection
	closeOnce sync.Once
}

func (c *ownedClient) connection() *observedConnection {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wire
}

func (c *ownedClient) close() error {
	var err error
	c.closeOnce.Do(func() { err = c.api.Close() })
	return err
}

type ordinaryFixture struct {
	ctx       context.Context
	cancel    context.CancelFunc
	hapCtx    *synchronizedContext
	server    *httptest.Server
	setupHTTP *http.Transport
	address   string
	port      int
	clients   []*ownedClient
	handlerMu sync.Mutex
	requests  map[string]connectionRequests

	controller *client.ControllerIdentity
	pairing    *client.AccessoryConnectionConfig

	temperatureWrites atomic.Int32
	modeWrites        atomic.Int32
	verifyRequests    atomic.Int32
	characteristicGET atomic.Int32
	characteristicPUT atomic.Int32

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

func newOrdinaryFixture(t *testing.T) *ordinaryFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f := &ordinaryFixture{
		ctx: ctx, cancel: cancel,
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
		Name: "NHA009 synthetic thermostat", Manufacturer: "Homeward qualification",
		Model: "synthetic", SerialNumber: "0001",
	}, 21.5, 0, 100, 0.1)
	thermostat := thermo.Thermostat
	thermostat.CurrentTemperature.SetValue(21.5)
	thermostat.TargetTemperature.SetValue(21.5)
	thermostat.CurrentHeatingCoolingState.SetValue(0)
	thermostat.TargetHeatingCoolingState.SetValue(1)
	thermostat.TemperatureDisplayUnits.SetValue(0)
	humidity := characteristic.NewCurrentRelativeHumidity()
	humidity.SetValue(45)
	thermostat.AddCharacteristic(humidity.Characteristic)
	currentFan := characteristic.NewCurrentFanState()
	currentFan.SetValue(0)
	thermostat.AddCharacteristic(currentFan.Characteristic)
	targetFan := characteristic.NewTargetFanState()
	targetFan.SetValue(1)
	thermostat.AddCharacteristic(targetFan.Characteristic)
	thermostat.TargetTemperature.OnValueRemoteUpdate(func(float64) { f.temperatureWrites.Add(1) })
	thermostat.TargetHeatingCoolingState.OnValueRemoteUpdate(func(int) { f.modeWrites.Add(1) })
	// Install the callback before any server goroutine can access the characteristic.
	thermostat.CurrentTemperature.OnValueRemoteGet(func() float64 {
		if f.blockNext.CompareAndSwap(true, false) {
			f.enteredOnce.Do(func() { close(f.entered) })
			defer f.exitedOnce.Do(func() { close(f.exited) })
			select {
			case <-f.release:
			case <-f.ctx.Done():
			}
		}
		return 21.5
	})
	container := accessory.NewContainer()
	if err := container.AddAccessory(thermo.Accessory); err != nil {
		t.Fatal("add synthetic accessory:", err)
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
			f.characteristicGET.Add(1)
			counts.characteristicsGET++
		}
		// Count every received PUT before hc can discard a same-value update.
		if r.Method == http.MethodPut && r.URL.Path == "/characteristics" {
			f.characteristicPUT.Add(1)
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
	f.address = net.JoinHostPort("127.0.0.1", strconv.Itoa(f.port))
	f.setupHTTP = &http.Transport{DialContext: f.dialLoopback, DisableCompression: true}
	f.controller, err = client.NewRandomControllerConfig()
	if err != nil {
		t.Fatal("create synthetic controller:", err)
	}
	pairCtx, pairCancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer pairCancel()
	f.pairing, err = client.NewSetupClient(&http.Client{Transport: f.setupHTTP}).Pair(pairCtx,
		&client.AccessoryPairingConfig{
			PIN: syntheticPIN, DeviceID: syntheticDevice,
			IPConnectionInfo: client.IPConnectionInfo{IPAddress: "127.0.0.1", Port: f.port},
		}, f.controller)
	if err != nil {
		t.Fatal("ordinary synthetic pair-setup:", err)
	}
	return f
}

func (f *ordinaryFixture) dialLoopback(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" || address != f.address {
		return nil, fmt.Errorf("qualification dial outside the owned loopback listener")
	}
	return (&net.Dialer{}).DialContext(ctx, network, address)
}

func (f *ordinaryFixture) newClient() *ownedClient {
	return f.newClientWithIdentity(f.controller)
}

func (f *ordinaryFixture) newClientWithIdentity(identity *client.ControllerIdentity) *ownedClient {
	c := &ownedClient{}
	c.api = client.NewAccessoryClient(func(ctx context.Context, network, address string) (net.Conn, error) {
		raw, err := f.dialLoopback(ctx, network, address)
		if err != nil {
			return nil, err
		}
		wire := &observedConnection{Conn: raw}
		c.mu.Lock()
		c.wire = wire
		c.mu.Unlock()
		return wire, nil
	}, identity, f.pairing)
	f.clients = append(f.clients, c)
	return c
}

func (f *ordinaryFixture) requestsFor(remote string) connectionRequests {
	f.handlerMu.Lock()
	defer f.handlerMu.Unlock()
	return f.requests[remote]
}

func (f *ordinaryFixture) unblock() {
	f.releaseOnce.Do(func() { close(f.release) })
}

func (f *ordinaryFixture) close(t *testing.T) {
	t.Helper()
	f.closeOnce.Do(func() {
		f.cancel()
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

package archive

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/damione1/Homeward/internal/backups"
	"github.com/damione1/Homeward/internal/backups/local"
	"github.com/damione1/Homeward/internal/storage"
)

func privateRecoveryDirFixture(t *testing.T) string {
	t.Helper()
	// testing.TempDir does not promise mode0700 for its numbered leaf.
	directory := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal("create private recovery fixture directory")
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatal("recovery fixture directory is not private")
	}
	return directory
}

func identityFixture(t *testing.T) (RecoveryPrivate, RecoveryPublic) {
	t.Helper()
	i, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal("generate fixture identity")
	}
	private := RecoveryPrivate{identity: i}
	public, _ := private.Public()
	return private, public
}
func serverKeyFixture(t *testing.T) storage.BackupServerKey {
	t.Helper()
	raw := []byte("HOMEWARD_SECRETS_KEY_V1=" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x51}, 32)) + "\n")
	key, err := storage.ParseBackupServerKey(raw)
	if err != nil {
		t.Fatal("admit fixture server key")
	}
	return key
}
func inputFixture(t *testing.T) Input {
	t.Helper()
	yaml := "input_boolean:\n  NHA155_YAML_CANARY:\n    initial: true\n"
	source, err := NewYAMLSource("configuration.yaml", int64(len(yaml)), strings.NewReader(yaml))
	if err != nil {
		t.Fatal("admit fixture source")
	}
	database := "PGDMP_NHA155_DATABASE_CANARY"
	input, err := NewInput(strings.NewReader(database), int64(len(database)), []YAMLSource{source}, serverKeyFixture(t))
	if err != nil {
		t.Fatal("admit fixture input")
	}
	return input
}
func sealedFixture(t *testing.T, public RecoveryPublic) (*Sealed, []byte) {
	t.Helper()
	id, _ := backups.ParseArtifactID("15515515515515515515515515515515")
	sealed, err := Seal(context.Background(), filepath.Join(t.TempDir(), "cipher"), id, public, inputFixture(t), DefaultLimits())
	if err != nil {
		t.Fatal("seal fixture")
	}
	t.Cleanup(func() { _ = sealed.Close() })
	var b bytes.Buffer
	if _, err := sealed.WriteTo(&b); err != nil {
		t.Fatal("export fixture ciphertext")
	}
	return sealed, bytes.Clone(b.Bytes())
}
func recoverBytes(t *testing.T, ciphertext []byte, id backups.ArtifactID, private RecoveryPrivate, limits Limits) (RestoreResult, error, string) {
	t.Helper()
	parent := filepath.Join(t.TempDir(), "recovered")
	result, err := Recover(context.Background(), bytes.NewReader(ciphertext), id, private, parent, "restored", limits)
	return result, err, filepath.Join(parent, "restored")
}
func requireNoRestore(t *testing.T, root string) {
	t.Helper()
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatal("failed recovery published a destination")
	}
	entries, err := os.ReadDir(filepath.Dir(root))
	if err != nil {
		t.Fatal("inspect recovery parent")
	}
	if len(entries) != 0 {
		t.Fatal("failed recovery left partial files")
	}
}

func TestNHA155EncryptedVersionedArchiveAndOpaqueProviders(t *testing.T) {
	private, public := identityFixture(t)
	sealed, ciphertext := sealedFixture(t, public)
	var key bytes.Buffer
	_, _ = serverKeyFixture(t).WriteTo(&key)
	for _, secret := range [][]byte{[]byte("NHA155_YAML_CANARY"), []byte("NHA155_DATABASE_CANARY"), key.Bytes(), []byte(private.identity.String()), []byte("manifest.json")} {
		if bytes.Contains(ciphertext, secret) {
			t.Fatal("sensitive payload in ciphertext")
		}
	}
	for _, kind := range []string{"memory", "local"} {
		t.Run(kind, func(t *testing.T) {
			var provider backups.Provider = &memoryProvider{}
			var owner backups.ManagedProvider
			if kind == "local" {
				instance, _ := backups.ParseInstanceID("local")
				settings, _ := backups.NewLocalSettings(instance, true, filepath.Join(t.TempDir(), "store"), backups.DefaultLimits())
				var err error
				owner, err = (local.Factory{}).Open(context.Background(), settings)
				if err != nil {
					t.Fatal("open local fixture")
				}
				defer owner.Close()
				provider = owner
			}
			published, err := sealed.Publish(context.Background(), provider)
			if err != nil || published.State() != backups.Published {
				t.Fatal("publish opaque ciphertext")
			}
			parent := filepath.Join(t.TempDir(), "recover")
			result, err := RecoverProvider(context.Background(), provider, sealed.ID(), private, parent, "complete", DefaultLimits())
			if err != nil || result.State() != Restored {
				t.Fatal("recover opaque ciphertext")
			}
			raw, err := os.ReadFile(filepath.Join(parent, "complete", "yaml", "configuration.yaml"))
			if err != nil || !bytes.Contains(raw, []byte("NHA155_YAML_CANARY")) {
				t.Fatal("exact YAML not recovered")
			}
			for _, name := range []string{".env", "database.dump", "yaml/configuration.yaml"} {
				info, err := os.Stat(filepath.Join(parent, "complete", name))
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatal("restored file is not private")
				}
			}
		})
	}
}

func TestNHA155PublicOnlyChild(t *testing.T) {
	if os.Getenv("NHA155_CHILD") != "1" {
		t.Skip("subprocess helper")
	}
	if strings.Contains(strings.Join(os.Environ(), "\n"), "AGE-SECRET-KEY-") {
		t.Fatal("private recovery material reached backup process")
	}
	public, err := ParseRecoveryPublic(os.Getenv("NHA155_PUBLIC"))
	if err != nil {
		t.Fatal("public-only child input")
	}
	image, err := Seal(context.Background(), filepath.Join(os.Getenv("NHA155_ROOT"), "cipher"), mustArtifactID(t), public, inputFixture(t), DefaultLimits())
	if err != nil {
		t.Fatal("public-only child seal")
	}
	defer image.Close()
	f, err := os.OpenFile(filepath.Join(os.Getenv("NHA155_ROOT"), "export.age"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("child ciphertext output")
	}
	_, writeErr := image.WriteTo(f)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatal("child ciphertext output failed")
	}
}
func mustArtifactID(t *testing.T) backups.ArtifactID {
	t.Helper()
	id, err := backups.ParseArtifactID("15515515515515515515515515515515")
	if err != nil {
		t.Fatal("fixture ID")
	}
	return id
}
func TestNHA155PublicOnlyNonInteractiveBackup(t *testing.T) {
	private, public := identityFixture(t)
	text, _ := public.PublicText()
	root := t.TempDir()
	command := exec.Command(os.Args[0], "-test.run=^TestNHA155PublicOnlyChild$")
	command.Env = []string{"NHA155_CHILD=1", "NHA155_PUBLIC=" + text, "NHA155_ROOT=" + root}
	command.Stdin = nil
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatal("noninteractive backup subprocess failed")
	}
	if bytes.Contains(output, []byte(private.identity.String())) {
		t.Fatal("private recovery in process output")
	}
	ciphertext, err := os.ReadFile(filepath.Join(root, "export.age"))
	if err != nil {
		t.Fatal("read child ciphertext")
	}
	result, err, _ := recoverBytes(t, ciphertext, mustArtifactID(t), private, DefaultLimits())
	if err != nil || result.State() != Restored {
		t.Fatal("public-only backup not recoverable")
	}
}

type tarFixtureEntry struct {
	header tar.Header
	data   []byte
}

func plaintextFixture(t *testing.T, ciphertext []byte, private RecoveryPrivate) []byte {
	t.Helper()
	r, err := age.Decrypt(bytes.NewReader(ciphertext), private.identity)
	if err != nil {
		t.Fatal("decrypt hostile fixture")
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal("read hostile fixture")
	}
	return b
}
func encryptFixture(t *testing.T, plain []byte, public RecoveryPublic) []byte {
	t.Helper()
	var b bytes.Buffer
	w, err := age.Encrypt(&b, public.recipient)
	if err != nil {
		t.Fatal("encrypt hostile fixture")
	}
	if _, err := w.Write(plain); err != nil {
		t.Fatal("write hostile fixture")
	}
	if err := w.Close(); err != nil {
		t.Fatal("finish hostile fixture")
	}
	return b.Bytes()
}
func hostileTar(t *testing.T, plain []byte, change func([]tarFixtureEntry)) []byte {
	t.Helper()
	reader := tar.NewReader(bytes.NewReader(plain))
	var entries []tarFixtureEntry
	for {
		h, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal("parse fixture tar")
		}
		b, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal("read fixture tar")
		}
		entries = append(entries, tarFixtureEntry{*h, b})
	}
	change(entries)
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, entry := range entries {
		entry.header.Size = int64(len(entry.data))
		if entry.header.Typeflag != tar.TypeReg {
			entry.header.Size = 0
			entry.data = nil
		}
		if err := w.WriteHeader(&entry.header); err != nil {
			t.Fatal("hostile tar header")
		}
		if _, err := w.Write(entry.data); err != nil {
			t.Fatal("hostile tar body")
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal("hostile tar footer")
	}
	return b.Bytes()
}
func changeManifest(t *testing.T, entries []tarFixtureEntry, change func(*manifest)) {
	t.Helper()
	last := &entries[len(entries)-1]
	var m manifest
	if json.Unmarshal(last.data, &m) != nil {
		t.Fatal("fixture manifest")
	}
	change(&m)
	last.data, _ = json.Marshal(m)
}
func TestNHA155RejectAlterationsAndUnsafeExtraction(t *testing.T) {
	private, public := identityFixture(t)
	image, ciphertext := sealedFixture(t, public)
	plain := plaintextFixture(t, ciphertext, private)
	mutations := map[string][]byte{}
	for name, at := range map[string]int{"header": 0, "payload": len(ciphertext) / 2, "last-tag": len(ciphertext) - 1} {
		copy := bytes.Clone(ciphertext)
		copy[at] ^= 1
		mutations[name] = copy
	}
	mutations["truncated"] = bytes.Clone(ciphertext[:len(ciphertext)-1])
	mutations["ciphertext-suffix"] = append(bytes.Clone(ciphertext), 1)
	mutations["age-version"] = bytes.Replace(ciphertext, []byte("age-encryption.org/v1"), []byte("age-encryption.org/v9"), 1)
	mutations["plaintext-suffix"] = encryptFixture(t, append(bytes.Clone(plain), 1), public)
	mutations["tar-footer-truncated"] = encryptFixture(t, plain[:len(plain)-1024], public)
	for name, change := range map[string]func([]tarFixtureEntry){
		"traversal-header": func(e []tarFixtureEntry) { e[0].header.Name = "../escape" },
		"absolute-header":  func(e []tarFixtureEntry) { e[0].header.Name = "/escape" },
		"symlink":          func(e []tarFixtureEntry) { e[0].header.Typeflag = tar.TypeSymlink; e[0].header.Linkname = "../escape" },
		"hardlink":         func(e []tarFixtureEntry) { e[0].header.Typeflag = tar.TypeLink; e[0].header.Linkname = "../escape" },
		"fifo":             func(e []tarFixtureEntry) { e[0].header.Typeflag = tar.TypeFifo },
		"pax": func(e []tarFixtureEntry) {
			e[0].header.Format = tar.FormatPAX
			e[0].header.PAXRecords = map[string]string{"comment": "untrusted"}
		},
		"duplicate":        func(e []tarFixtureEntry) { e[1].header.Name = e[0].header.Name },
		"key-format":       func(e []tarFixtureEntry) { e[1].data[0] ^= 1 },
		"manifest-version": func(e []tarFixtureEntry) { changeManifest(t, e, func(m *manifest) { m.Version = 99 }) },
		"manifest-traversal": func(e []tarFixtureEntry) {
			changeManifest(t, e, func(m *manifest) { m.Entries[2].Path = "yaml/../escape" })
		},
		"manifest-backslash": func(e []tarFixtureEntry) {
			changeManifest(t, e, func(m *manifest) { m.Entries[2].Path = "yaml/C:\\escape" })
		},
		"manifest-hash": func(e []tarFixtureEntry) {
			changeManifest(t, e, func(m *manifest) { m.Entries[0].SHA256 = strings.Repeat("0", 64) })
		},
		"manifest-size": func(e []tarFixtureEntry) { changeManifest(t, e, func(m *manifest) { m.Entries[0].Size++ }) },
		"manifest-identity": func(e []tarFixtureEntry) {
			changeManifest(t, e, func(m *manifest) { m.Artifact = strings.Repeat("0", 32) })
		},
		"manifest-missing": func(e []tarFixtureEntry) { e[len(e)-1].header.Name = "yaml/000001" },
		"manifest-duplicate-json": func(e []tarFixtureEntry) {
			last := &e[len(e)-1]
			last.data = append(bytes.Clone(last.data[:len(last.data)-1]), []byte(",\"version\":1}")...)
		},
	} {
		mutations[name] = encryptFixture(t, hostileTar(t, plain, change), public)
	}
	for name, changed := range mutations {
		t.Run(name, func(t *testing.T) {
			result, err, root := recoverBytes(t, changed, image.ID(), private, DefaultLimits())
			if err == nil || result.State() != RestoreNotPublished {
				t.Fatal("hostile archive accepted")
			}
			requireNoRestore(t, root)
		})
	}
	wrong, _ := identityFixture(t)
	for _, identity := range []RecoveryPrivate{{}, wrong} {
		result, err, root := recoverBytes(t, ciphertext, image.ID(), identity, DefaultLimits())
		if err == nil || result.State() != RestoreNotPublished {
			t.Fatal("missing/wrong recovery accepted")
		}
		if identity.Valid() {
			requireNoRestore(t, root)
		}
	}
}

type memoryReader struct {
	*bytes.Reader
	metadata backups.Metadata
	failEOF  bool
	closeErr error
}

func (r *memoryReader) Metadata() backups.Metadata { return r.metadata }
func (r *memoryReader) Close() error               { return r.closeErr }
func (r *memoryReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.EOF && r.failEOF {
		return n, backups.NewError(backups.Corrupt)
	}
	return n, err
}

type memoryProvider struct {
	payload   []byte
	metadata  backups.Metadata
	ambiguous bool
	failEOF   bool
	closeErr  error
}

func (*memoryProvider) Capabilities() backups.Capabilities {
	c, _ := backups.NewCapabilities(backups.DefaultLimits())
	return c
}
func (p *memoryProvider) Write(_ context.Context, intent backups.WriteIntent, reader io.Reader) (backups.WriteResult, error) {
	data, err := io.ReadAll(io.LimitReader(reader, intent.MaxBytes()+1))
	if err != nil || int64(len(data)) != intent.MaxBytes() {
		return backups.NewWriteResult(backups.NotPublished, intent.ID(), backups.Metadata{})
	}
	state := backups.Published
	if p.payload != nil {
		if !bytes.Equal(data, p.payload) {
			r, _ := backups.NewWriteResult(backups.NotPublished, intent.ID(), backups.Metadata{})
			return r, backups.NewError(backups.Conflict)
		}
		state = backups.AlreadyPresent
	}
	p.payload = bytes.Clone(data)
	p.metadata, _ = backups.NewMetadata(intent.ID(), int64(len(data)), sha256.Sum256(data), time.Now().UTC())
	if p.ambiguous {
		p.ambiguous = false
		r, _ := backups.NewWriteResult(backups.Indeterminate, intent.ID(), backups.Metadata{})
		return r, backups.NewError(backups.Unavailable)
	}
	return backups.NewWriteResult(state, intent.ID(), p.metadata)
}
func (p *memoryProvider) Open(_ context.Context, _ backups.ArtifactID) (backups.ArtifactReader, error) {
	return &memoryReader{Reader: bytes.NewReader(p.payload), metadata: p.metadata, failEOF: p.failEOF, closeErr: p.closeErr}, nil
}
func (*memoryProvider) List(context.Context, backups.ListRequest) (backups.Page, error) {
	return backups.Page{}, backups.NewError(backups.Unsupported)
}
func (*memoryProvider) Delete(context.Context, backups.ArtifactID) (backups.DeleteResult, error) {
	return backups.DeleteResult{}, backups.NewError(backups.Unsupported)
}
func (p *memoryProvider) Verify(context.Context, backups.ArtifactID) (backups.VerifiedArtifact, error) {
	return backups.NewVerifiedArtifact(p.metadata)
}

func TestNHA155ExactCiphertextRetryAndCompleteProviderEOF(t *testing.T) {
	private, public := identityFixture(t)
	image, ciphertext := sealedFixture(t, public)
	provider := &memoryProvider{ambiguous: true}
	first, err := image.Publish(context.Background(), provider)
	if err == nil || first.State() != backups.Indeterminate {
		t.Fatal("ambiguous write was claimed successful")
	}
	second, err := image.Publish(context.Background(), provider)
	if err != nil || second.State() != backups.AlreadyPresent || !bytes.Equal(ciphertext, provider.payload) {
		t.Fatal("retry changed encrypted image")
	}
	provider.failEOF = true
	parent := filepath.Join(t.TempDir(), "parent")
	result, err := RecoverProvider(context.Background(), provider, image.ID(), private, parent, "restored", DefaultLimits())
	if err == nil || result.State() != RestoreNotPublished {
		t.Fatal("provider failed EOF ignored")
	}
	requireNoRestore(t, filepath.Join(parent, "restored"))
}

func TestNHA155RecoveryExportRetentionAndRedaction(t *testing.T) {
	operator, server := privateRecoveryDirFixture(t), privateRecoveryDirFixture(t)
	privateFile, publicFile := filepath.Join(operator, "recovery.key"), filepath.Join(server, "recovery.pub")
	fingerprint, err := ExportRecovery(privateFile, publicFile, server)
	if err != nil || len(fingerprint) != 64 {
		t.Fatal("offline recovery export")
	}
	identity, err := LoadRecoveryPrivate(privateFile)
	if err != nil {
		t.Fatal("load retained recovery")
	}
	raw, _ := os.ReadFile(privateFile)
	publicRaw, _ := os.ReadFile(publicFile)
	for _, value := range []any{identity, serverKeyFixture(t), inputFixture(t)} {
		encoded, _ := json.Marshal(value)
		for _, rendered := range []string{fmt.Sprintf("%s %v %+v %#v %x", value, value, value, value, value), string(encoded)} {
			if strings.Contains(rendered, strings.TrimSpace(string(raw))) || strings.Contains(rendered, "NHA155_") {
				t.Fatal("private value escaped formatting")
			}
		}
	}
	if got, err := VerifyRetainedRecovery(privateFile, publicFile, server); err != nil || got != fingerprint {
		t.Fatal("retained-copy challenge")
	}
	copyFile := filepath.Join(operator, "second.key")
	if got, err := CopyRecovery(privateFile, copyFile, server); err != nil || got != fingerprint {
		t.Fatal("second recovery export")
	}
	if _, err := ExportRecovery(privateFile, publicFile, server); err == nil {
		t.Fatal("recovery export overwrote existing file")
	}
	after, _ := os.ReadFile(privateFile)
	if !bytes.Equal(after, raw) {
		t.Fatal("existing recovery changed")
	}
	if _, err := CopyRecovery(privateFile, filepath.Join(server, "forbidden.key"), server); err == nil {
		t.Fatal("recovery stored with server data")
	}
	if err := os.Remove(privateFile); err != nil {
		t.Fatal("remove first fixture copy")
	}
	if _, err := VerifyRetainedRecovery(privateFile, publicFile, server); err == nil {
		t.Fatal("missing copy accepted")
	}
	if _, err := VerifyRetainedRecovery(copyFile, publicFile, server); err != nil {
		t.Fatal("second copy not independently retained")
	}
	for _, keyPath := range []string{copyFile, publicFile} {
		info, err := os.Stat(keyPath)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("export permissions")
		}
	}
	wrongPrivate, _ := identityFixture(t)
	wrongRaw := []byte(wrongPrivate.identity.String() + "\n")
	if os.WriteFile(copyFile, wrongRaw, 0600) != nil {
		t.Fatal("wrong key fixture")
	}
	if _, err := VerifyRetainedRecovery(copyFile, publicFile, server); err == nil {
		t.Fatal("wrong copy accepted")
	}
	if bytes.Contains(publicRaw, []byte("AGE-SECRET-KEY-")) {
		t.Fatal("private key in public export")
	}
}

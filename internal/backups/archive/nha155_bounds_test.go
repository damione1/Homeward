package archive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"github.com/damione1/Homeward/internal/backups"
	"github.com/damione1/Homeward/internal/storage"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type failedInput struct{}

func (failedInput) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestNHA155BoundsAdmissionCancellationAndExistingDestination(t *testing.T) {
	private, public := identityFixture(t)
	image, ciphertext := sealedFixture(t, public)
	for _, path := range []string{"../secret", "/secret", "a/../secret", "a\\secret", "C:secret", ".", "a\x00b"} {
		if _, err := NewYAMLSource(path, 1, strings.NewReader("a")); err == nil {
			t.Fatal("unsafe source path admitted")
		}
	}
	var nilReader *bytes.Reader
	if _, err := NewYAMLSource("configuration.yaml", 1, nilReader); err == nil {
		t.Fatal("typed nil source admitted")
	}
	var nilWriter *bytes.Buffer
	if _, err := serverKeyFixture(t).WriteTo(nilWriter); err == nil {
		t.Fatal("typed nil key writer admitted")
	}
	if _, err := (storage.BackupServerKey{}).WriteTo(io.Discard); err == nil {
		t.Fatal("zero key admitted")
	}
	if _, err := ParseRecoveryPrivate([]byte("AGE-SECRET-KEY-NHA155_PRIVATE_CANARY")); err == nil || strings.Contains(err.Error(), "CANARY") {
		t.Fatal("malformed private key leaked")
	}
	if _, err := Seal(context.Background(), filepath.Join(t.TempDir(), "cipher"), image.ID(), RecoveryPublic{}, inputFixture(t), DefaultLimits()); err == nil {
		t.Fatal("missing public recovery accepted")
	}
	small, _ := NewLimits(128, 64, 3)
	result, err, root := recoverBytes(t, ciphertext, image.ID(), private, small)
	if err == nil || result.State() != RestoreNotPublished {
		t.Fatal("ciphertext budget ignored")
	}
	requireNoRestore(t, root)
	plainLimit, _ := NewLimits(1<<20, 1, 3)
	result, err, root = recoverBytes(t, ciphertext, image.ID(), private, plainLimit)
	if err == nil || result.State() != RestoreNotPublished {
		t.Fatal("plaintext budget ignored")
	}
	requireNoRestore(t, root)
	input := inputFixture(t)
	input.database = failedInput{}
	dir := filepath.Join(t.TempDir(), "failed-cipher")
	if image, err := Seal(context.Background(), dir, mustArtifactID(t), public, input, DefaultLimits()); err == nil || image != nil {
		t.Fatal("failed source was sealed")
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatal("failed sealing left temporary artifact")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Seal(ctx, dir, mustArtifactID(t), public, inputFixture(t), DefaultLimits()); err == nil {
		t.Fatal("cancelled sealing accepted")
	}
	parent := filepath.Join(t.TempDir(), "existing")
	if err := os.MkdirAll(filepath.Join(parent, "restore"), 0700); err != nil {
		t.Fatal("existing destination fixture")
	}
	canary := filepath.Join(parent, "restore", "existing")
	if err := os.WriteFile(canary, []byte("preserve-existing"), 0600); err != nil {
		t.Fatal("existing destination canary")
	}
	result, err = Recover(context.Background(), bytes.NewReader(ciphertext), image.ID(), private, parent, "restore", DefaultLimits())
	if err == nil || result.State() != RestoreNotPublished {
		t.Fatal("existing destination overwritten")
	}
	got, _ := os.ReadFile(canary)
	if string(got) != "preserve-existing" {
		t.Fatal("existing destination changed")
	}
}

func TestNHA155RecoveryKeySymlinksAndPermissions(t *testing.T) {
	operator, server := privateRecoveryDirFixture(t), privateRecoveryDirFixture(t)
	nonPrivate := privateRecoveryDirFixture(t)
	if err := os.Chmod(nonPrivate, 0755); err != nil {
		t.Fatal("prepare non-private recovery directory fixture")
	}
	refused, unusedPublic := filepath.Join(nonPrivate, "refused.key"), filepath.Join(server, "unused.pub")
	if _, err := ExportRecovery(refused, unusedPublic, server); err == nil {
		t.Fatal("non-private recovery export directory accepted")
	}
	for _, path := range []string{refused, unusedPublic} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatal("refused recovery export published a key file")
		}
	}
	private, public := filepath.Join(operator, "private.key"), filepath.Join(server, "public.key")
	if _, err := ExportRecovery(private, public, server); err != nil {
		t.Fatal("recovery export fixture")
	}
	if err := os.Symlink("private.key", filepath.Join(operator, "alias.key")); err != nil {
		t.Fatal("relative symlink fixture")
	}
	if _, err := LoadRecoveryPrivate(filepath.Join(operator, "alias.key")); err == nil {
		t.Fatal("relative internal key symlink accepted")
	}
	if err := os.Chmod(private, 0644); err != nil {
		t.Fatal("permission fixture")
	}
	if _, err := LoadRecoveryPrivate(private); err == nil {
		t.Fatal("publicly readable private recovery accepted")
	}
}

func TestNHA155SealedZeroAndTypedNilAreAdmittedExplicitly(t *testing.T) {
	var zero Sealed
	if _, err := zero.WriteTo(io.Discard); err == nil {
		t.Fatal("zero sealed image was readable")
	}
	if _, err := zero.Publish(context.Background(), &memoryProvider{}); err == nil {
		t.Fatal("zero sealed image was publishable")
	}
	if err := zero.Close(); err == nil {
		t.Fatal("zero sealed image was not classified invalid")
	}
	if err := zero.Close(); err != nil {
		t.Fatal("zero sealed close was not idempotent")
	}
	var nilImage *Sealed
	if err := nilImage.Close(); err != nil {
		t.Fatal("nil image close failed")
	}
	if _, err := nilImage.WriteTo(io.Discard); err == nil {
		t.Fatal("nil image was readable")
	}
	if _, err := nilImage.Publish(context.Background(), &memoryProvider{}); err == nil {
		t.Fatal("nil image was publishable")
	}
	_, public := identityFixture(t)
	image, _ := sealedFixture(t, public)
	var nilProvider *memoryProvider
	if _, err := image.Publish(context.Background(), nilProvider); err == nil {
		t.Fatal("typed nil provider admitted")
	}
	var nilWriter *bytes.Buffer
	if _, err := image.WriteTo(nilWriter); err == nil {
		t.Fatal("typed nil ciphertext writer admitted")
	}
	if err := image.Close(); err != nil {
		t.Fatal("close initialized image")
	}
	if err := image.Close(); err != nil {
		t.Fatal("initialized close was not idempotent")
	}
}

func TestNHA155RestoreErrorsAfterRealPublicationRemainIndeterminate(t *testing.T) {
	private, public := identityFixture(t)
	image, ciphertext := sealedFixture(t, public)
	for _, phase := range []string{"rename-after-effect", "sync-after-effect"} {
		t.Run(phase, func(t *testing.T) {
			parent := filepath.Join(t.TempDir(), "parent")
			operations := productionRestoreIO()
			performed := false
			if phase == "rename-after-effect" {
				operations.rename = func(root *os.Root, from, to string) error {
					if err := renameExclusive(root, from, to); err != nil {
						return err
					}
					performed = true
					return syscall.EIO
				}
			} else {
				operations.sync = func(root *os.Root, name string) error {
					if err := syncRoot(root, name); err != nil {
						return err
					}
					performed = true
					return syscall.EIO
				}
			}
			result, err := recoverArchive(context.Background(), bytes.NewReader(ciphertext), nil, nil, image.ID(), private, parent, "complete", DefaultLimits(), operations)
			if !performed || err == nil || result.State() != RestoreIndeterminate {
				t.Fatal("after-effect error lost its ambiguous publication state")
			}
			for _, name := range []string{".env", "database.dump", "yaml/configuration.yaml"} {
				if _, err := os.Stat(filepath.Join(parent, "complete", name)); err != nil {
					t.Fatal("completed destination removed after ambiguous publication")
				}
			}
			raw, err := os.ReadFile(filepath.Join(parent, "complete", "database.dump"))
			if err != nil || string(raw) != "PGDMP_NHA155_DATABASE_CANARY" {
				t.Fatal("after-effect destination was partial")
			}
			if entries, err := os.ReadDir(parent); err != nil || len(entries) != 1 || entries[0].Name() != "complete" {
				t.Fatal("after-effect operation left another visible restoration")
			}
			retry, err := Recover(context.Background(), bytes.NewReader(ciphertext), image.ID(), private, parent, "complete", DefaultLimits())
			if err == nil || retry.State() != RestoreNotPublished {
				t.Fatal("retry overwrote ambiguous complete destination")
			}
		})
	}
}

func TestNHA155ConcurrentRestorePublicationIsExclusive(t *testing.T) {
	private, public := identityFixture(t)
	parent := filepath.Join(t.TempDir(), "parent")
	ready, release := make(chan struct{}, 2), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	operations := productionRestoreIO()
	operations.rename = func(root *os.Root, from, to string) error {
		ready <- struct{}{}
		<-release
		return renameExclusive(root, from, to)
	}
	type outcome struct {
		candidate int
		result    RestoreResult
		err       error
	}
	results := make(chan outcome, 2)
	for i, label := range []string{"candidate-left", "candidate-right"} {
		input := inputFixture(t)
		input.database, input.databaseSize = strings.NewReader(label), int64(len(label))
		image, err := Seal(context.Background(), filepath.Join(t.TempDir(), "cipher"), mustArtifactID(t), public, input, DefaultLimits())
		if err != nil {
			t.Fatal("seal concurrent candidate")
		}
		var ciphertext bytes.Buffer
		_, writeErr := image.WriteTo(&ciphertext)
		closeErr := image.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatal("prepare concurrent candidate")
		}
		go func(candidate int, raw []byte) {
			result, err := recoverArchive(context.Background(), bytes.NewReader(raw), nil, nil, mustArtifactID(t), private, parent, "winner", DefaultLimits(), operations)
			results <- outcome{candidate, result, err}
		}(i, bytes.Clone(ciphertext.Bytes()))
	}
	for i := 0; i < 2; i++ {
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent restore did not reach the publication boundary")
		}
	}
	close(release)
	winner := -1
	for i := 0; i < 2; i++ {
		select {
		case result := <-results:
			if result.err == nil && result.result.State() == Restored {
				if winner != -1 {
					t.Fatal("two recoveries overwrote the same destination")
				}
				winner = result.candidate
			} else if result.result.State() != RestoreNotPublished || !errors.Is(result.err, fail(Conflict)) {
				t.Fatal("concurrent loser was not refused exclusively")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent recovery did not terminate")
		}
	}
	if winner < 0 {
		t.Fatal("no concurrent recovery won")
	}
	raw, err := os.ReadFile(filepath.Join(parent, "winner", "database.dump"))
	if err != nil || string(raw) != []string{"candidate-left", "candidate-right"}[winner] {
		t.Fatal("published bytes do not belong to the winner")
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 1 || entries[0].Name() != "winner" {
		t.Fatal("concurrent restoration left a partial destination")
	}
}

type cancellingReader struct {
	source io.Reader
	cancel context.CancelFunc
}

func (r cancellingReader) Read(p []byte) (int, error) {
	n, err := r.source.Read(p)
	if n > 0 {
		r.cancel()
	}
	return n, err
}
func TestNHA155MultiChunkAuthenticationAndProviderFailures(t *testing.T) {
	private, public := identityFixture(t)
	var ciphertext []byte
	databaseSize := 64 << 10
	for attempt := 0; attempt < 3; attempt++ {
		input := inputFixture(t)
		input.database = bytes.NewReader(bytes.Repeat([]byte{0x35}, databaseSize))
		input.databaseSize = int64(databaseSize)
		image, err := Seal(context.Background(), filepath.Join(t.TempDir(), "cipher"), mustArtifactID(t), public, input, DefaultLimits())
		if err != nil {
			t.Fatal("seal chunk-boundary fixture")
		}
		var b bytes.Buffer
		_, writeErr := image.WriteTo(&b)
		closeErr := image.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatal("read chunk-boundary fixture")
		}
		ciphertext = bytes.Clone(b.Bytes())
		plain := plaintextFixture(t, ciphertext, private)
		if len(plain)%(64<<10) == 0 {
			break
		}
		databaseSize += (64 << 10) - len(plain)%(64<<10)
	}
	plain := plaintextFixture(t, ciphertext, private)
	if len(plain) < 2*(64<<10) || len(plain)%(64<<10) != 0 {
		t.Fatal("fixture did not reach a full final age chunk")
	}
	if result, err, _ := recoverBytes(t, ciphertext, mustArtifactID(t), private, DefaultLimits()); err != nil || result.State() != Restored {
		t.Fatal("full final chunk did not round trip")
	}
	for _, at := range []int{len(ciphertext) / 2, len(ciphertext) - 1} {
		changed := bytes.Clone(ciphertext)
		changed[at] ^= 1
		result, err, path := recoverBytes(t, changed, mustArtifactID(t), private, DefaultLimits())
		if err == nil || result.State() != RestoreNotPublished {
			t.Fatal("multi-chunk tampering accepted")
		}
		requireNoRestore(t, path)
	}
	for _, changed := range [][]byte{ciphertext[:len(ciphertext)-1], append(bytes.Clone(ciphertext), 1)} {
		result, err, path := recoverBytes(t, changed, mustArtifactID(t), private, DefaultLimits())
		if err == nil || result.State() != RestoreNotPublished {
			t.Fatal("full final chunk truncation or suffix accepted")
		}
		requireNoRestore(t, path)
	}
	for _, defect := range []string{"close", "size", "hash"} {
		provider := &memoryProvider{payload: bytes.Clone(ciphertext)}
		provider.metadata, _ = backups.NewMetadata(mustArtifactID(t), int64(len(ciphertext)), sha256.Sum256(ciphertext), time.Now().UTC())
		switch defect {
		case "close":
			provider.closeErr = errors.New("NHA155_PRIVATE_CLOSE_CANARY")
		case "size":
			provider.metadata, _ = backups.NewMetadata(mustArtifactID(t), int64(len(ciphertext)+1), sha256.Sum256(ciphertext), time.Now().UTC())
		case "hash":
			provider.metadata, _ = backups.NewMetadata(mustArtifactID(t), int64(len(ciphertext)), sha256.Sum256([]byte("wrong")), time.Now().UTC())
		}
		parent := filepath.Join(t.TempDir(), "parent")
		result, err := RecoverProvider(context.Background(), provider, mustArtifactID(t), private, parent, "restored", DefaultLimits())
		if err == nil || result.State() != RestoreNotPublished || strings.Contains(err.Error(), "CANARY") {
			t.Fatal("provider terminal failure accepted or leaked")
		}
		requireNoRestore(t, filepath.Join(parent, "restored"))
	}
	ctx, cancel := context.WithCancel(context.Background())
	parent := filepath.Join(t.TempDir(), "parent")
	result, err := Recover(ctx, cancellingReader{bytes.NewReader(ciphertext), cancel}, mustArtifactID(t), private, parent, "restored", DefaultLimits())
	if err == nil || result.State() != RestoreNotPublished {
		t.Fatal("mid-stream cancellation accepted")
	}
	requireNoRestore(t, filepath.Join(parent, "restored"))
}

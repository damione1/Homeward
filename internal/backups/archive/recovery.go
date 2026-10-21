package archive

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
)

func LoadRecoveryPrivate(filePath string) (RecoveryPrivate, error) {
	raw, err := readKeyFile(filePath, true)
	if err != nil {
		return RecoveryPrivate{}, err
	}
	defer clear(raw)
	return ParseRecoveryPrivate(raw)
}
func LoadRecoveryPublic(filePath string) (RecoveryPublic, error) {
	raw, err := readKeyFile(filePath, false)
	if err != nil {
		return RecoveryPublic{}, err
	}
	defer clear(raw)
	return ParseRecoveryPublic(strings.TrimSuffix(string(raw), "\n"))
}
func publicFingerprint(public RecoveryPublic) string {
	text, _ := public.PublicText()
	digest := sha256.Sum256([]byte("homeward/recovery-public/v1\x00" + text))
	return hex.EncodeToString(digest[:])
}
func outsideServerData(filePath, serverDataDir string) bool {
	if !filepath.IsAbs(filePath) || filepath.Clean(filePath) != filePath || !filepath.IsAbs(serverDataDir) || filepath.Clean(serverDataDir) != serverDataDir || serverDataDir == "/" || filepath.Base(filePath) == ".env" {
		return false
	}
	inside := func(name, root string) bool {
		relative, err := filepath.Rel(root, name)
		return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
	}
	if inside(filePath, serverDataDir) {
		return false
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(filePath))
	if err != nil {
		return false
	}
	resolvedServer, err := filepath.EvalSymlinks(serverDataDir)
	if err != nil {
		// Offline operator machines need not have the lost server's mount.
		if !os.IsNotExist(err) {
			return false
		}
		resolvedServer = serverDataDir
	}
	return !inside(filepath.Join(parent, filepath.Base(filePath)), resolvedServer)
}

// ExportRecovery generates on the operator machine. The private copy is written
// first and is never deleted if the subsequent public export fails. No server
// database or .env is a source or destination of private recovery material.
func ExportRecovery(privateOutput, publicOutput, serverDataDir string) (string, error) {
	if !outsideServerData(privateOutput, serverDataDir) || privateOutput == publicOutput || filepath.Base(publicOutput) == ".env" {
		return "", fail(Invalid)
	}
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		return "", fail(Unavailable)
	}
	private := RecoveryPrivate{identity: identity}
	raw := []byte(identity.String() + "\n")
	defer clear(raw)
	if err := exclusiveFile(privateOutput, raw); err != nil {
		return "", err
	}
	public, _ := private.Public()
	text, _ := public.PublicText()
	if err := exclusiveFile(publicOutput, []byte(text+"\n")); err != nil {
		return "", err
	}
	return publicFingerprint(public), nil
}

// CopyRecovery exports an existing operator copy; it never regenerates one.
func CopyRecovery(privateSource, privateOutput, serverDataDir string) (string, error) {
	if !outsideServerData(privateSource, serverDataDir) || !outsideServerData(privateOutput, serverDataDir) {
		return "", fail(Invalid)
	}
	identity, err := LoadRecoveryPrivate(privateSource)
	if err != nil {
		return "", err
	}
	raw := []byte(identity.identity.String() + "\n")
	defer clear(raw)
	if err := exclusiveFile(privateOutput, raw); err != nil {
		return "", err
	}
	public, _ := identity.Public()
	return publicFingerprint(public), nil
}

// VerifyRetainedRecovery reads the actual retained copy and proves possession
// by decrypting a fresh challenge. Its only output is a public-key fingerprint.
func VerifyRetainedRecovery(privateFile, publicFile, serverDataDir string) (string, error) {
	if !outsideServerData(privateFile, serverDataDir) {
		return "", fail(Invalid)
	}
	private, err := LoadRecoveryPrivate(privateFile)
	if err != nil {
		return "", err
	}
	public, err := LoadRecoveryPublic(publicFile)
	if err != nil {
		return "", err
	}
	derived, _ := private.Public()
	derivedText, _ := derived.PublicText()
	expectedText, _ := public.PublicText()
	if derivedText != expectedText {
		return "", fail(Integrity)
	}
	var challenge [32]byte
	if _, err := rand.Read(challenge[:]); err != nil {
		return "", fail(Unavailable)
	}
	defer clear(challenge[:])
	var ciphertext bytes.Buffer
	w, err := age.Encrypt(&ciphertext, public.recipient)
	if err != nil {
		return "", fail(Unavailable)
	}
	if _, err := w.Write(challenge[:]); err != nil {
		return "", fail(Unavailable)
	}
	if err := w.Close(); err != nil {
		return "", fail(Unavailable)
	}
	r, err := age.Decrypt(bytes.NewReader(ciphertext.Bytes()), private.identity)
	if err != nil {
		return "", fail(Integrity)
	}
	got, err := io.ReadAll(io.LimitReader(r, 33))
	defer clear(got)
	if err != nil || len(got) != len(challenge) || subtle.ConstantTimeCompare(got, challenge[:]) != 1 {
		return "", fail(Integrity)
	}
	return publicFingerprint(public), nil
}

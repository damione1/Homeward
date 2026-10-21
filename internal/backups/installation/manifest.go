package installation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"time"

	"github.com/damione1/Homeward/internal/storage"
)

const manifestPath = "installation.json"
const maxManifest = 1 << 20
const maxSources = 1023

type sourceWitness struct {
	Path      string `json:"path"`
	Archive   string `json:"archive"`
	Document  string `json:"document"`
	Revision  int64  `json:"revision"`
	SHA256    string `json:"sha256"`
	Bootstrap bool   `json:"bootstrap"`
}
type manifest struct {
	Version        int                   `json:"version"`
	Generation     int64                 `json:"generation"`
	ManifestSHA256 string                `json:"manifest_sha256"`
	CutUTC         time.Time             `json:"cut_utc"`
	Sources        []sourceWitness       `json:"sources"`
	Tables         []storage.BackupTable `json:"tables"`
}

func sourceName(index int) string { return fmt.Sprintf("sources/%06d.yaml", index) }
func newManifest(snapshot *storage.BackupSnapshot) (manifest, error) {
	hash := snapshot.Generation().ManifestSHA256()
	result := manifest{Version: 1, Generation: snapshot.Generation().Revision().Number(), ManifestSHA256: hex.EncodeToString(hash[:]), CutUTC: snapshot.Cut(), Tables: snapshot.Tables()}
	for index, source := range snapshot.Sources() {
		digest := source.SHA256()
		result.Sources = append(result.Sources, sourceWitness{source.Path(), sourceName(index), source.Ref().Document().String(), source.Ref().Revision().Number(), hex.EncodeToString(digest[:]), source.Bootstrap()})
	}
	if err := result.valid(); err != nil {
		return manifest{}, err
	}
	return result, nil
}
func safePath(value string) bool {
	return fs.ValidPath(value) && value != "." && path.Clean(value) == value && len(value) <= 1024 && !strings.ContainsAny(value, "\\\x00:\r\n")
}
func digest(raw string) ([32]byte, error) {
	var result [32]byte
	b, err := hex.DecodeString(raw)
	if err != nil || len(b) != 32 || hex.EncodeToString(b) != raw {
		return result, failure(Integrity)
	}
	copy(result[:], b)
	return result, nil
}
func (m manifest) valid() error {
	if m.Version != 1 || m.Generation <= 0 || m.CutUTC.IsZero() || m.CutUTC.Location() != time.UTC || len(m.Sources) < 2 || len(m.Sources) > maxSources || len(m.Tables) == 0 || len(m.Tables) > 512 {
		return failure(Integrity)
	}
	if _, err := digest(m.ManifestSHA256); err != nil {
		return err
	}
	paths := make(map[string]bool)
	docs := make(map[string]bool)
	bootstrap := 0
	for index, s := range m.Sources {
		if !safePath(s.Path) || s.Path == ".env" || s.Path == "database.dump" || s.Path == "installation-restored.json" || strings.HasPrefix(s.Path, "yaml/") || s.Archive != sourceName(index) || paths[s.Path] || docs[s.Document] || s.Revision <= 0 {
			return failure(Integrity)
		}
		if _, err := storage.NewConfigDocumentID(s.Document); err != nil {
			return failure(Integrity)
		}
		if _, err := digest(s.SHA256); err != nil {
			return err
		}
		paths[s.Path] = true
		docs[s.Document] = true
		if s.Bootstrap {
			bootstrap++
		}
	}
	if bootstrap != 1 {
		return failure(Integrity)
	}
	revision, _ := storage.NewConfigGenerationRevision(m.Generation)
	hash, _ := digest(m.ManifestSHA256)
	if _, err := storage.NewBackupRestoreExpectation(revision, hash, m.Tables); err != nil {
		return failure(Integrity)
	}
	return nil
}
func decodeManifest(raw []byte) (manifest, error) {
	var result manifest
	if len(raw) == 0 || len(raw) > maxManifest {
		return result, failure(Integrity)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil {
		return manifest{}, failure(Integrity)
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return manifest{}, failure(Integrity)
	}
	if err := result.valid(); err != nil {
		return manifest{}, err
	}
	// Canonical encoding also rejects duplicate keys, alternate fields and values
	// accepted by encoding/json's permissive object/number handling.
	canonical, err := json.Marshal(result)
	if err != nil || !bytes.Equal(canonical, raw) {
		return manifest{}, failure(Integrity)
	}
	return result, nil
}
func (m manifest) matchSources(sources []storage.BackupSource) error {
	if len(sources) != len(m.Sources) {
		return failure(Integrity)
	}
	for i, source := range sources {
		expected := m.Sources[i]
		hash := source.SHA256()
		if source.Path() != expected.Path || source.Ref().Document().String() != expected.Document || source.Ref().Revision().Number() != expected.Revision || source.Bootstrap() != expected.Bootstrap || hex.EncodeToString(hash[:]) != expected.SHA256 || sha256.Sum256(source.Bytes()) != hash {
			return failure(Integrity)
		}
	}
	return nil
}

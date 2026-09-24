// Package seed embeds the official chrisvanmeer.orbitron Ansible collection
// (as a built collection artifact plus generated metadata) into the Orbitron
// binary, so the local cache is seeded out of the box with a copy of the very
// collection used to install and operate the daemon. Seeding happens both from
// `orbitron --install` and on every daemon start, and only ever adds a strictly
// newer bundled version, keeping existing cache state untouched. The artifacts
// are regenerated from the in-repo collection sources by `make seed`.
package seed

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"orbitron/internal/config"
	"orbitron/internal/fetcher"
	"orbitron/internal/version"
)

// Identity of the bundled collection, mirrored from the generated metadata.
const (
	Namespace = "chrisvanmeer"
	Name      = "orbitron"
)

//go:embed collection.tar.gz
var CollectionTarGz []byte

//go:embed meta.json
var metaJSON []byte

// Meta describes the bundled collection artifact.
type Meta struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Version   string `json:"version"`
}

// Load returns the metadata of the embedded collection, generated from the
// in-repo galaxy.yml whenever a new seed artifact is built.
func Load() (Meta, error) {
	var m Meta
	if err := json.Unmarshal(metaJSON, &m); err != nil {
		return m, fmt.Errorf("parse embedded collection metadata: %w", err)
	}
	if m.Namespace == "" || m.Name == "" || m.Version == "" {
		return m, fmt.Errorf("embedded collection metadata is incomplete: %+v", m)
	}
	return m, nil
}

// Result reports what a SeedCache call did.
type Result struct {
	Seeded  bool   // the embedded artifact was written
	Skipped bool   // a same-or-newer version was already cached; nothing touched
	Version string // the effectively present (bundled) version
	Path    string // path of the (written) artifact, empty when skipped
}

// ArtifactName returns the cache file name for a bundled version.
func ArtifactName(ns, name, v string) string {
	return ns + "-" + name + "-" + v + ".tar.gz"
}

// bundledVersions returns every version of the bundled collection already
// present in the cache (files named "<namespace>-<name>-<version>.tar.gz"),
// or nil when the directory does not exist yet.
func bundledVersions(colsDir string) ([]string, error) {
	entries, err := os.ReadDir(colsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	prefix := Namespace + "-" + Name + "-"
	var versions []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".tar.gz") || !strings.HasPrefix(name, prefix) {
			continue
		}
		versions = append(versions, strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".tar.gz"))
	}
	return versions, nil
}

// shouldSeed reports whether the embedded version is strictly newer than every
// version already cached. Re-runs keep an existing copy untouched (same or
// newer versions, and even older ones), so upgrading the binary never
// overwrites, downgrades, or duplicates state.
func shouldSeed(embedded string, existing []string) bool {
	for _, v := range existing {
		if version.Compare(embedded, v) <= 0 {
			return false
		}
	}
	return true
}

// SeedCache places the embedded Ansible collection into the daemon's cache
// (honoring a custom storage_path from the daemon configuration) when the
// embedded version is strictly newer than anything already present, and
// declares it through a hash-deduped default requirements manifest so a later
// prune keeps it. It never removes or modifies existing artifacts or user
// manifests.
func SeedCache(storagePath string) (Result, error) {
	var res Result
	if len(CollectionTarGz) == 0 {
		return res, nil
	}

	meta, err := Load()
	if err != nil {
		return res, err
	}
	res.Version = meta.Version

	colsDir := filepath.Join(storagePath, "collections", meta.Namespace)
	existing, err := bundledVersions(colsDir)
	if err != nil {
		return res, fmt.Errorf("list cached %s.%s versions: %w", meta.Namespace, meta.Name, err)
	}
	if !shouldSeed(meta.Version, existing) {
		res.Skipped = true
		return res, nil
	}

	if err := os.MkdirAll(colsDir, 0750); err != nil {
		return res, fmt.Errorf("create cache dir %s: %w", colsDir, err)
	}

	path := filepath.Join(colsDir, ArtifactName(meta.Namespace, meta.Name, meta.Version))
	if err := os.WriteFile(path, CollectionTarGz, 0640); err != nil {
		return res, fmt.Errorf("write %s: %w", path, err)
	}
	res.Seeded = true
	res.Path = path

	f := fetcher.NewFetcher(storagePath, 1, fetcher.ProxyConfig{}, config.TLSConfig{})
	manifest := fmt.Sprintf("collections:\n  - name: %s.%s\n    version: %s\n", meta.Namespace, meta.Name, meta.Version)
	if err := f.SaveManifest("collections", []byte(manifest)); err != nil {
		return res, fmt.Errorf("write default requirements manifest: %w", err)
	}

	return res, nil
}

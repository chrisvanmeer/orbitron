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

// Owner identifies the uid/gid that must own everything the seed writes into (or
// adopts in) the cache. The installer passes it because it runs as root:
// without it, every file a root-run seed creates stays root-owned, and the
// daemon — which runs as the unprivileged orbitron user — cannot read it back.
// A nil Owner leaves ownership alone, which is correct for a caller that
// already runs as the service user, such as the daemon's own startup seed.
type Owner struct {
	UID int
	GID int
}

// Result reports what a SeedCache call did.
type Result struct {
	Seeded          bool   // the embedded artifact was written
	Skipped         bool   // the bundled version was already cached; no artifact written
	ManifestPath    string // path of the requirements manifest declaring the bundled version
	ManifestAdopted bool   // that manifest already existed and was left in place
	Version         string // the effectively present (bundled) version
	Path            string // path of the (written) artifact, empty when skipped
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

// cached reports whether the exact bundled version is already present, as
// opposed to merely having a version that is new enough to suppress the seed.
func cached(embedded string, existing []string) bool {
	for _, v := range existing {
		if v == embedded {
			return true
		}
	}
	return false
}

// chown is os.Chown behind a variable so tests can assert which paths a seed
// claims ownership of without needing the privileges to actually change it.
var chown = os.Chown

// own applies owner to path. A nil owner is a no-op, and a path that does not
// exist is not an error: a skipped seed may never have created the directory.
// A failure here is returned rather than swallowed, because a file that is not
// owned by the service user is precisely the defect that leaves the daemon
// unable to read — and therefore unable to dump — its own cache.
func own(owner *Owner, path string) error {
	if owner == nil {
		return nil
	}
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := chown(path, owner.UID, owner.GID); err != nil {
		return fmt.Errorf("set ownership on %s: %w", path, err)
	}
	return nil
}

// ownDirs applies owner to a directory chain, tolerating absent entries. The
// chain exists so a custom storage_path whose parents were created by an
// earlier root-run stays traversable for the service user.
func ownDirs(owner *Owner, dirs ...string) error {
	for _, dir := range dirs {
		if err := own(owner, dir); err != nil {
			return err
		}
	}
	return nil
}

// SeedCache places the embedded Ansible collection into the daemon's cache
// (honoring a custom storage_path from the daemon configuration) when the
// embedded version is strictly newer than anything already present, and
// declares it through a hash-deduped default requirements manifest so a later
// prune keeps it. It never removes or modifies existing artifacts or user
// manifests, and never downgrades a cached version that is newer than the
// bundled one.
//
// The manifest is (re)declared whenever the bundled version is present in the
// cache, which also repairs a seed that was interrupted between writing the
// artifact and writing the manifest: the presence of the artifact is exactly
// what makes shouldSeed skip, so without this such a cache would stay
// undeclared forever.
//
// When owner is non-nil, everything the seed writes or adopts is chowned to it.
func SeedCache(storagePath string, owner *Owner) (Result, error) {
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
	manifestsDir := filepath.Join(storagePath, "manifests")
	existing, err := bundledVersions(colsDir)
	if err != nil {
		return res, fmt.Errorf("list cached %s.%s versions: %w", meta.Namespace, meta.Name, err)
	}

	seedArtifact := shouldSeed(meta.Version, existing)
	// A cached version that is strictly newer than the bundled one means the
	// binary was downgraded: there is nothing of ours left to declare.
	declare := seedArtifact || cached(meta.Version, existing)

	if seedArtifact {
		if err := os.MkdirAll(colsDir, 0750); err != nil {
			return res, fmt.Errorf("create cache dir %s: %w", colsDir, err)
		}
		path := filepath.Join(colsDir, ArtifactName(meta.Namespace, meta.Name, meta.Version))
		if err := os.WriteFile(path, CollectionTarGz, 0640); err != nil {
			return res, fmt.Errorf("write %s: %w", path, err)
		}
		if err := own(owner, path); err != nil {
			return res, err
		}
		res.Seeded = true
		res.Path = path
	} else {
		res.Skipped = true
	}

	if declare {
		if err := declareBundled(storagePath, meta, owner, &res); err != nil {
			return res, err
		}
	}

	// The directory chain must be traversable for the service user. A custom
	// storage_path outside the standard subtree may have root-owned parents
	// from an earlier install that the chownRecursive pass no longer covers.
	if err := ownDirs(owner, storagePath, filepath.Dir(colsDir), colsDir, manifestsDir); err != nil {
		return res, err
	}
	return res, nil
}

// declareBundled writes the default requirements manifest that declares the
// bundled collection, or adopts the existing one when its content-addressed
// path is already present. Adoption is what keeps a daemon restart from
// rewriting the file, and it is also the repair path for a manifest that an
// earlier root-run left root-owned: the installer adopts and chowns it, so
// upgrading to a fixed binary also repairs an already damaged cache.
func declareBundled(storagePath string, meta Meta, owner *Owner, res *Result) error {
	manifest := fmt.Sprintf("collections:\n  - name: %s.%s\n    version: %s\n", meta.Namespace, meta.Name, meta.Version)
	data := []byte(manifest)
	path := filepath.Join(storagePath, "manifests", fetcher.ManifestName("collections", data))
	res.ManifestPath = path

	if _, err := os.Lstat(path); err == nil {
		res.ManifestAdopted = true
		return own(owner, path)
	} else if !os.IsNotExist(err) {
		return err
	}

	f := fetcher.NewFetcher(storagePath, 1, fetcher.ProxyConfig{}, config.TLSConfig{})
	if err := f.SaveManifest("collections", data); err != nil {
		return fmt.Errorf("write default requirements manifest: %w", err)
	}
	return own(owner, path)
}

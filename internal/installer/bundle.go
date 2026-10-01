package installer

import (
	"fmt"
	"path/filepath"
	"strings"

	"orbitron/internal/config"
	"orbitron/internal/logger"
	"orbitron/internal/seed"
)

// seedStoragePath returns the effective daemon storage directory, honoring a
// custom storage_path from the daemon configuration when present instead of
// assuming the installer default path.
func seedStoragePath() (string, error) {
	cfg, err := config.LoadConfig(ConfigFile)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", ConfigFile, err)
	}
	if cfg.StoragePath != "" {
		return cfg.StoragePath, nil
	}
	return StorageDir, nil
}

// seedBundledCollection places the embedded Ansible collection into the daemon
// cache (following the configured storage_path) when the embedded version is
// strictly newer than anything already present, and reports what happened.
//
// Ownership is not fixed up here: the seed applies it to the artifact, the
// manifest and the whole directory chain in one place, so every entry point
// that seeds the cache (this installer and the daemon's own startup seed)
// ends up with the same guarantee. This function only reports the outcome.
func seedBundledCollection(uid, gid int) error {
	meta, err := seed.Load()
	if err != nil {
		return err
	}

	storagePath, err := seedStoragePath()
	if err != nil {
		return err
	}

	// A custom storage_path outside the standard subtree is not covered by the
	// installer's recursive chown pass. SeedCache will still chown the directory
	// chain and any entries it creates or adopts, but other existing content in
	// that custom tree is not recursively corrected here. Warn if it lies outside
	// the standard subtree.
	if !withinStandardStorage(storagePath) {
		fmt.Printf("  ! Custom storage_path %s lies outside %s: other existing content in that tree is not recursively corrected, so ensure the orbitron user can read and write it\n", storagePath, StorageDir)
	}

	res, err := seed.SeedCache(storagePath, &seed.Owner{UID: uid, GID: gid})
	if err != nil {
		return err
	}
	if res.Skipped {
		fmt.Printf("  ℹ Bundled %s.%s %s already cached; keeping existing copy\n", meta.Namespace, meta.Name, meta.Version)
		if res.ManifestAdopted {
			fmt.Printf("  ✔ Ownership verified on the existing requirements manifest\n")
		}
		return nil
	}
	if !res.Seeded {
		logger.Info("No embedded Ansible collection seed available; skipping")
		return nil
	}

	fmt.Printf("  ✔ Seeded %s.%s %s into cache (%d bytes)\n", meta.Namespace, meta.Name, meta.Version, len(seed.CollectionTarGz))
	fmt.Printf("  ✔ Declared %s.%s %s in a default requirements manifest\n", meta.Namespace, meta.Name, meta.Version)
	return nil
}

// withinStandardStorage reports whether storagePath sits inside the directory
// tree the installer creates and chowns recursively, in which case ownership is
// already consistent before the seed runs.
func withinStandardStorage(storagePath string) bool {
	clean := filepath.Clean(storagePath)
	base := filepath.Clean(StorageDir)
	return clean == base || strings.HasPrefix(clean, base+string(filepath.Separator))
}

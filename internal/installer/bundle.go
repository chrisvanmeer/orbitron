package installer

import (
	"fmt"
	"os"
	"path/filepath"

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
// strictly newer than anything already present, fixes ownership so the
// orbitron user can read what it seeded, and reports what happened.
func seedBundledCollection(uid, gid int) error {
	meta, err := seed.Load()
	if err != nil {
		return err
	}

	storagePath, err := seedStoragePath()
	if err != nil {
		return err
	}

	res, err := seed.SeedCache(storagePath)
	if err != nil {
		return err
	}
	if res.Skipped {
		fmt.Printf("  ℹ Bundled %s.%s %s already cached; keeping existing copy\n", meta.Namespace, meta.Name, meta.Version)
		return nil
	}
	if !res.Seeded {
		logger.Info("No embedded Ansible collection seed available; skipping")
		return nil
	}

	_ = os.Chown(res.Path, uid, gid)
	// With a custom storage_path the daemon-owned directories may not exist
	// yet; make sure the chain the seed created stays readable by the
	// orbitron user (0750 root-owned parents would block traversal).
	for _, dir := range []string{storagePath, filepath.Join(storagePath, "collections"), filepath.Join(storagePath, "collections", meta.Namespace), filepath.Join(storagePath, "manifests")} {
		_ = os.Chown(dir, uid, gid)
	}
	fmt.Printf("  ✔ Seeded %s.%s %s into cache (%d bytes)\n", meta.Namespace, meta.Name, meta.Version, len(seed.CollectionTarGz))
	fmt.Printf("  ✔ Declared %s.%s %s in a default requirements manifest\n", meta.Namespace, meta.Name, meta.Version)
	return nil
}

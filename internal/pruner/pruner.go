// Package pruner removes cached role and collection versions that have not
// been accessed by any client within a configurable retention window. It is
// fully access-based: every download served by the mirror records a timestamp
// (see internal/access), and prune deletes only versions that were never
// resolved into a download within the last N days, or that were never touched
// after being mirrored more than N days ago.
//
// The on-disk layout that the pruner understands is:
//
//	<storage>/roles/<name>/<version>/
//	<storage>/collections/<namespace>/<namespace>-<name>-<version>.tar.gz
//
// A role version that is never touched (its directory was freshly mirrored but
// never downloaded) is still kept up to the first prune pass to avoid
// immediately deleting data that mirrors are still population.
package pruner

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"orbitron/internal/access"
)

// Pruner scans the storage tree and reports which role/collection versions are
// safe to remove given the access history recorded for each stored version.
type Pruner struct {
	storagePath string
}

// NewPruner creates a pruner rooted at the given storage path. The recorder's
// access index lives at <storagePath>/.access.json and is shared with the
// server so that downloads update the same timeline the pruner reads.
func NewPruner(storagePath string) *Pruner {
	return &Pruner{storagePath: storagePath}
}

// parseCollectionArtifact extracts the collection name and version from a
// stored artifact filename of the form "<namespace>-<name>-<version>.tar.gz".
func parseCollectionArtifact(namespace, filename string) (string, string, bool) {
	if !strings.HasSuffix(filename, ".tar.gz") {
		return "", "", false
	}
	stem := strings.TrimSuffix(filename, ".tar.gz")
	prefix := namespace + "-"
	if !strings.HasPrefix(stem, prefix) {
		return "", "", false
	}
	remainder := strings.TrimPrefix(stem, prefix)
	lastHyphen := strings.LastIndex(remainder, "-")
	if lastHyphen == -1 {
		return "", "", false
	}
	return remainder[:lastHyphen], remainder[lastHyphen+1:], true
}

// computeDirSize returns the recursive, block-aware size of a path so freed
// disk space can be reported accurately for directories and files alike.
func computeDirSize(path string) int64 {
	var size int64
	_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err == nil {
			if stat, ok := info.Sys().(*syscall.Stat_t); ok {
				size += stat.Blocks * 512
			} else if !info.IsDir() {
				size += info.Size()
			}
		}
		return nil
	})
	return size
}

// PruneResult is a structured outcome of a prune operation, used by
// programmatic callers (e.g., the HTTP prune endpoint) rather than the
// interactive CLI.
type PruneResult struct {
	Items      []string `json:"items"`
	FreedBytes int64    `json:"freed_bytes"`
	Executed   bool     `json:"executed"`
}

// Scan returns the absolute paths of every role/collection version candidate
// for pruning given the access retention window in days. It never deletes.
//
// A version is a candidate when the shared access recorder says it should not
// be kept for the window (i.e. it was last accessed more than maxAge ago, or
// was never accessed). Versions that have never been touched are never
// returned in the first pass, since a freshly mirrored artifact has not yet
// had a chance to be served.
func (p *Pruner) Scan(days int) []string {
	maxAge := time.Duration(days) * 24 * time.Hour
	rec := access.New(p.storagePath)
	var toDelete []string

	// 1. Scan roles storage directory.
	rolesDir := filepath.Join(p.storagePath, "roles")
	roleEntries, err := os.ReadDir(rolesDir)
	if err == nil {
		for _, rEntry := range roleEntries {
			if !rEntry.IsDir() {
				continue
			}
			roleName := rEntry.Name()
			versionsDir := filepath.Join(rolesDir, roleName)
			verEntries, err := os.ReadDir(versionsDir)
			if err != nil {
				continue
			}

			for _, vEntry := range verEntries {
				if !vEntry.IsDir() {
					continue
				}
				version := vEntry.Name()
				if !rec.ShouldKeep(access.RoleKey(roleName, version), maxAge) {
					toDelete = append(toDelete, filepath.Join(versionsDir, version))
				}
			}
		}
	}

	// 2. Scan collections storage directory (.tar.gz artifact files).
	collectionsDir := filepath.Join(p.storagePath, "collections")
	colEntries, err := os.ReadDir(collectionsDir)
	if err == nil {
		for _, cEntry := range colEntries {
			if !cEntry.IsDir() {
				continue
			}
			namespace := cEntry.Name()
			nsDir := filepath.Join(collectionsDir, namespace)
			files, err := os.ReadDir(nsDir)
			if err != nil {
				continue
			}

			for _, f := range files {
				colName, colVer, ok := parseCollectionArtifact(namespace, f.Name())
				if !ok {
					continue
				}
				fullName := namespace + "." + colName
				if !rec.ShouldKeep(access.CollectionKey(fullName, colVer), maxAge) {
					toDelete = append(toDelete, filepath.Join(nsDir, f.Name()))
				}
			}
		}
	}

	return toDelete
}

// RunPruneDryRun returns the candidate paths for pruning without deleting or
// prompting, mirroring the read-only scan used by RunPrune.
func (p *Pruner) RunPruneDryRun(days int) ([]string, error) {
	return p.Scan(days), nil
}

// RunPruneAPI prunes the unreferenced versions, optionally as a dry run, and
// returns a structured result. It is the programmatic counterpart of the
// interactive RunPrune and backs the HTTP prune endpoint.
func (p *Pruner) RunPruneAPI(dryRun bool, days int) (*PruneResult, error) {
	toDelete := p.Scan(days)

	result := &PruneResult{
		Items:    toDelete,
		Executed: !dryRun,
	}

	if dryRun {
		return result, nil
	}

	for _, path := range toDelete {
		freed := computeDirSize(path)
		if err := os.RemoveAll(path); err != nil {
			return result, fmt.Errorf("failed to remove %s: %w", path, err)
		}
		result.FreedBytes += freed
	}

	return result, nil
}

// RunPrune scans the access timeline, finds stale versions, prompts, and
// removes them. The retention window is configurable through days.
func (p *Pruner) RunPrune(days int) error {
	fmt.Println("🔍 Scanning access timeline and storage for stale versions...")

	toDelete := p.Scan(days)

	if len(toDelete) == 0 {
		fmt.Println("✨ Storage is clean. No versions outside the access window found.")
		return nil
	}

	fmt.Printf("\nFound %d version(s) to prune (older than %d days):\n\n", len(toDelete), days)
	for _, path := range toDelete {
		fmt.Printf("  ❌ %s\n", path)
	}

	// Prompt for user confirmation
	reader := bufio.NewReader(os.Stdin)
	fmt.Print("\nDo you want to permanently delete these items? [y/N]: ")
	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(strings.ToLower(input))

	if input == "y" || input == "yes" {
		fmt.Println("\n🧹 Deleting stale versions...")
		for _, path := range toDelete {
			if err := os.RemoveAll(path); err != nil {
				fmt.Printf("  ⚠️ Failed to remove %s: %v\n", path, err)
			} else {
				fmt.Printf("  ✔ Removed %s\n", path)
			}
		}
		fmt.Println("\n✨ Prune operation completed.")
	} else {
		fmt.Println("Aborted. No files were removed.")
	}

	return nil
}

package pruner

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"orbitron/internal/fetcher"
	ver "orbitron/internal/version"
)

type Pruner struct {
	storagePath  string
	manifestPath string
}

func NewPruner(storagePath string) *Pruner {
	return &Pruner{
		storagePath:  storagePath,
		manifestPath: filepath.Join(storagePath, "manifests"),
	}
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

// collectDeclared parses every stored manifest and returns the declared
// version requirements (exact versions, "latest", or specifier sets) for
// each role and collection name.
func (p *Pruner) collectDeclared() (map[string][]string, map[string][]string) {
	roles := make(map[string][]string)
	collections := make(map[string][]string)

	manifestFiles, err := os.ReadDir(p.manifestPath)
	if err != nil {
		return roles, collections
	}

	for _, mf := range manifestFiles {
		if mf.IsDir() {
			continue
		}

		data, err := os.ReadFile(filepath.Join(p.manifestPath, mf.Name()))
		if err != nil {
			continue
		}

		reqs, err := fetcher.ParseRequirements(data)
		if err != nil {
			continue
		}

		for _, role := range reqs.Roles {
			name := role.Name
			if name == "" && role.Src != "" {
				name = strings.TrimSuffix(filepath.Base(role.Src), ".git")
			}
			if name != "" {
				roles[name] = append(roles[name], role.Version)
			}
		}

		for _, col := range reqs.Collections {
			name := col.Name
			if name == "" && col.Src != "" {
				name = strings.TrimSuffix(filepath.Base(col.Src), ".git")
			}
			if name != "" {
				collections[name] = append(collections[name], col.Version)
			}
		}
	}

	return roles, collections
}

// keepVersion reports whether the on-disk candidate version of a role or
// collection should be kept given any of the declared requirements. Each
// requirement is checked with the full set of on-disk versions so that
// "latest" and constraint requirements only retain the highest match.
func keepVersion(declarations []string, diskVersions []string, candidate string) bool {
	for _, declared := range declarations {
		if ver.ShouldKeep(declared, diskVersions, candidate) {
			return true
		}
	}
	return false
}

// RunPrune scans manifests, finds orphaned versions on disk, prompts, and removes them
func (p *Pruner) RunPrune() error {
	fmt.Println("🔍 Scanning manifests and storage for orphaned versions...")

	activeRoles, activeCollections := p.collectDeclared()

	// 2. Scan roles storage directory
	var toDelete []string

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

			var diskVersions []string
			for _, vEntry := range verEntries {
				if vEntry.IsDir() {
					diskVersions = append(diskVersions, vEntry.Name())
				}
			}

			for _, vEntry := range verEntries {
				if !vEntry.IsDir() {
					continue
				}
				version := vEntry.Name()
				if !keepVersion(activeRoles[roleName], diskVersions, version) {
					toDelete = append(toDelete, filepath.Join(versionsDir, version))
				}
			}
		}
	}

	// 3. Scan collections storage directory (.tar.gz files and git folders)
	collectionsDir := filepath.Join(p.storagePath, "collections")
	colEntries, err := os.ReadDir(collectionsDir)
	if err == nil {
		for _, cEntry := range colEntries {
			if cEntry.IsDir() && cEntry.Name() != "git" {
				nsDir := filepath.Join(collectionsDir, cEntry.Name())
				files, err := os.ReadDir(nsDir)
				if err != nil {
					continue
				}

				colVersionMap := make(map[string][]string) // fullName -> on-disk versions
				for _, f := range files {
					colName, colVer, ok := parseCollectionArtifact(cEntry.Name(), f.Name())
					if !ok {
						continue
					}
					fullName := cEntry.Name() + "." + colName
					colVersionMap[fullName] = append(colVersionMap[fullName], colVer)
				}

				for _, f := range files {
					colName, colVer, ok := parseCollectionArtifact(cEntry.Name(), f.Name())
					if !ok {
						continue
					}
					fullName := cEntry.Name() + "." + colName
					if !keepVersion(activeCollections[fullName], colVersionMap[fullName], colVer) {
						toDelete = append(toDelete, filepath.Join(nsDir, f.Name()))
					}
				}
			}
		}
	}

	if len(toDelete) == 0 {
		fmt.Println("✨ Storage is clean. No unreferenced or older versions found.")
		return nil
	}

	// 4. Print summary
	fmt.Printf("\nFound %d unreferenced role/collection version(s) to prune:\n\n", len(toDelete))
	for _, path := range toDelete {
		fmt.Printf("  ❌ %s\n", path)
	}

	// 5. Prompt for user confirmation
	reader := bufio.NewReader(os.Stdin)
	fmt.Print("\nDo you want to permanently delete these items? [y/N]: ")
	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(strings.ToLower(input))

	if input == "y" || input == "yes" {
		fmt.Println("\n🧹 Deleting unreferenced items...")
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

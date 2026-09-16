package pruner

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"orbitron/fetcher"
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

// RunPrune scans manifests, finds orphaned versions on disk, prompts, and removes them
func (p *Pruner) RunPrune() error {
	fmt.Println("🔍 Scanning manifests and storage for orphaned versions...")

	// 1. Collect active versions from manifests
	activeRoles := make(map[string]map[string]bool)       // roleName -> version -> active
	activeCollections := make(map[string]map[string]bool) // collectionName -> version -> active

	manifestFiles, err := os.ReadDir(p.manifestPath)
	if err == nil {
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
				ver := role.Version
				if ver == "" {
					ver = "latest"
				}
				if activeRoles[name] == nil {
					activeRoles[name] = make(map[string]bool)
				}
				activeRoles[name][ver] = true
			}

			for _, col := range reqs.Collections {
				name := col.Name
				if name == "" && col.Src != "" {
					name = strings.TrimSuffix(filepath.Base(col.Src), ".git")
				}
				ver := col.Version
				if ver == "" {
					ver = "latest"
				}
				if activeCollections[name] == nil {
					activeCollections[name] = make(map[string]bool)
				}
				activeCollections[name][ver] = true
			}
		}
	}

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

			for _, vEntry := range verEntries {
				if !vEntry.IsDir() {
					continue
				}
				version := vEntry.Name()
				if !activeRoles[roleName][version] {
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
				for _, f := range files {
					// Check .tar.gz files format: namespace-name-version.tar.gz
					if strings.HasSuffix(f.Name(), ".tar.gz") {
						isUsed := false
						for _, versions := range activeCollections {
							for ver := range versions {
								if strings.Contains(f.Name(), ver) {
									isUsed = true
									break
								}
							}
						}
						if !isUsed {
							toDelete = append(toDelete, filepath.Join(nsDir, f.Name()))
						}
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

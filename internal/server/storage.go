package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"orbitron/internal/fetcher"
	"orbitron/internal/logger"
	ver "orbitron/internal/version"
)

type storageVersion struct {
	Version   string `json:"version"`
	Declared  bool   `json:"declared"`
	SizeBytes int64  `json:"size_bytes"`
}

type storageItem struct {
	Type     string           `json:"type"`
	Name     string           `json:"name"`
	Versions []storageVersion `json:"versions"`
}

type storageInventory struct {
	Roles       []storageItem `json:"roles"`
	Collections []storageItem `json:"collections"`
}

// collectionArtifactName extracts the collection name and version from a
// stored artifact filename of the form "<namespace>-<name>-<version>.tar.gz".
func collectionArtifactName(namespace, filename string) (name, version string, ok bool) {
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

// manifestRoleName resolves the canonical role identity the way the fetcher and
// dashboard index it: the name, or the basename of src with .git stripped.
func manifestRoleName(item fetcher.RoleItem) string {
	target := strings.TrimSpace(item.Name)
	if target == "" {
		target = strings.TrimSpace(item.Src)
	}
	if target == "" {
		return ""
	}
	target = strings.TrimSuffix(target, ".git")
	if idx := strings.LastIndexAny(target, "/:"); idx != -1 {
		target = target[idx+1:]
	}
	return strings.Trim(target, "\"'")
}

// declaredVersionsFromManifests collects every version requirement declared
// per role and collection identity across the stored manifests.
func declaredVersionsFromManifests(metas []fetcher.ManifestMeta) (roles, collections map[string][]string) {
	roles = make(map[string][]string)
	collections = make(map[string][]string)

	for _, m := range metas {
		for _, item := range m.Roles {
			if name := manifestRoleName(item); name != "" {
				roles[name] = append(roles[name], strings.Trim(strings.TrimSpace(item.Version), "\"'"))
			}
		}
		for _, item := range m.Collections {
			if name := strings.Trim(strings.TrimSpace(item.Name), "\"'"); name != "" {
				collections[name] = append(collections[name], strings.Trim(strings.TrimSpace(item.Version), "\"'"))
			}
		}
	}
	return roles, collections
}

// versionDeclared reports whether candidate is kept by any of the declared
// version requirements against the full set of on-disk versions, mirroring the
// pruner's per-requirement keepVersion check.
func versionDeclared(declared, disk []string, candidate string) bool {
	for _, requirement := range declared {
		if ver.ShouldKeep(requirement, disk, candidate) {
			return true
		}
	}
	return false
}

// storageEntrySize returns the recursive byte size of a cached version entry.
func storageEntrySize(path string) int64 {
	var size int64
	_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	return size
}

// HandleStorageInventory returns the contents of the local cache: every cached
// role and collection version, whether it is declared active by a stored
// manifest, and its on-disk size. It is the read-only counterpart of the
// per-version DELETE endpoints, letting automation inspect state idempotently.
func (s *Server) HandleStorageInventory(w http.ResponseWriter, r *http.Request) {
	if s.fetcher == nil {
		http.Error(w, "fetcher not initialized", http.StatusInternalServerError)
		return
	}

	metas, err := s.fetcher.ListManifests()
	if err != nil {
		logger.Error("Failed to list manifests: %v", err)
		http.Error(w, "Failed to list manifests", http.StatusInternalServerError)
		return
	}
	declaredRoles, declaredCols := declaredVersionsFromManifests(metas)

	inv := storageInventory{
		Roles:       []storageItem{},
		Collections: []storageItem{},
	}

	// Roles: one directory per role name, one subdirectory per version.
	rolesDir := filepath.Join(s.cfg.StoragePath, "roles")
	if entries, err := os.ReadDir(rolesDir); err == nil {
		for _, rEntry := range entries {
			if !rEntry.IsDir() {
				continue
			}
			name := rEntry.Name()
			versionsDir := filepath.Join(rolesDir, name)
			verEntries, err := os.ReadDir(versionsDir)
			if err != nil {
				continue
			}

			var versions []string
			for _, vEntry := range verEntries {
				if vEntry.IsDir() {
					versions = append(versions, vEntry.Name())
				}
			}
			sort.Sort(sort.Reverse(sort.StringSlice(versions)))

			var svs []storageVersion
			for _, version := range versions {
				svs = append(svs, storageVersion{
					Version:   version,
					Declared:  versionDeclared(declaredRoles[name], versions, version),
					SizeBytes: storageEntrySize(filepath.Join(versionsDir, version)),
				})
			}
			inv.Roles = append(inv.Roles, storageItem{Type: "role", Name: name, Versions: svs})
		}
	}

	// Collections: one directory per namespace, artifacts named
	// "<namespace>-<name>-<version>.tar.gz".
	colDir := filepath.Join(s.cfg.StoragePath, "collections")
	if nsEntries, err := os.ReadDir(colDir); err == nil {
		for _, nsEntry := range nsEntries {
			if !nsEntry.IsDir() || nsEntry.Name() == "git" {
				continue
			}
			nsPath := filepath.Join(colDir, nsEntry.Name())
			nsFiles, err := os.ReadDir(nsPath)
			if err != nil {
				continue
			}

			fileByVer := make(map[string]map[string]string) // fullName -> version -> artifact path
			grouped := make(map[string][]string)            // fullName -> versions
			for _, f := range nsFiles {
				if f.IsDir() || !strings.HasSuffix(f.Name(), ".tar.gz") {
					continue
				}
				colName, colVer, ok := collectionArtifactName(nsEntry.Name(), f.Name())
				if !ok {
					continue
				}
				full := nsEntry.Name() + "." + colName
				grouped[full] = append(grouped[full], colVer)
				if fileByVer[full] == nil {
					fileByVer[full] = make(map[string]string)
				}
				fileByVer[full][colVer] = filepath.Join(nsPath, f.Name())
			}

			for full, versions := range grouped {
				sort.Sort(sort.Reverse(sort.StringSlice(versions)))

				var svs []storageVersion
				for _, version := range versions {
					svs = append(svs, storageVersion{
						Version:   version,
						Declared:  versionDeclared(declaredCols[full], versions, version),
						SizeBytes: storageEntrySize(fileByVer[full][version]),
					})
				}
				inv.Collections = append(inv.Collections, storageItem{Type: "collection", Name: full, Versions: svs})
			}
		}
	}

	sort.Slice(inv.Roles, func(i, j int) bool { return inv.Roles[i].Name < inv.Roles[j].Name })
	sort.Slice(inv.Collections, func(i, j int) bool { return inv.Collections[i].Name < inv.Collections[j].Name })

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(inv)
}

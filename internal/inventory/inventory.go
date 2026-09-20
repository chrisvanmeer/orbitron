// Package inventory scans the Orbitron local cache and reports every cached
// role and collection version with its block-allocated disk usage. It is the
// single source of truth shared by the /metrics endpoint so that telemetry
// always agrees with what the dashboard cache matrix shows.
package inventory

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// Version is a single cached version of a role or collection.
type Version struct {
	Version   string
	SizeBytes int64
}

// Item is a distinct cached role name or collection full name with the
// versions currently on disk.
type Item struct {
	Type      string // "role" or "collection"
	Namespace string // set for collections only
	Name      string // role directory name, or "<namespace>.<name>" for collections
	Versions  []Version
}

// Inventory is the complete on-disk mirror inventory. An empty Inventory is
// safe to use and reports zero for every aggregate.
type Inventory struct {
	Roles       []Item
	Collections []Item
}

// RoleCount returns the number of distinct cached role names.
func (i *Inventory) RoleCount() int { return len(i.Roles) }

// CollectionCount returns the number of distinct cached collection names.
func (i *Inventory) CollectionCount() int { return len(i.Collections) }

// RoleVersionCount returns the total number of cached role versions.
func (i *Inventory) RoleVersionCount() int {
	n := 0
	for _, it := range i.Roles {
		n += len(it.Versions)
	}
	return n
}

// CollectionVersionCount returns the total number of cached collection versions.
func (i *Inventory) CollectionVersionCount() int {
	n := 0
	for _, it := range i.Collections {
		n += len(it.Versions)
	}
	return n
}

// NamespaceCount returns the number of distinct collection namespaces.
func (i *Inventory) NamespaceCount() int {
	namespaces := make(map[string]struct{})
	for _, it := range i.Collections {
		namespaces[it.Namespace] = struct{}{}
	}
	return len(namespaces)
}

// RoleBytes returns the block-allocated disk usage of all cached role versions.
func (i *Inventory) RoleBytes() int64 {
	var n int64
	for _, it := range i.Roles {
		for _, v := range it.Versions {
			n += v.SizeBytes
		}
	}
	return n
}

// CollectionBytes returns the block-allocated disk usage of all cached collection
// archives.
func (i *Inventory) CollectionBytes() int64 {
	var n int64
	for _, it := range i.Collections {
		for _, v := range it.Versions {
			n += v.SizeBytes
		}
	}
	return n
}

// StorageBytes returns the block-allocated disk usage of every cached role and
// collection version combined.
func (i *Inventory) StorageBytes() int64 { return i.RoleBytes() + i.CollectionBytes() }

// Scan inventories the cache rooted at storagePath. It mirrors the dashboard
// cache matrix: a role counts only when at least one version directory exists,
// and collection archives are parsed from "<namespace>-<name>-<version>.tar.gz"
// files. Directories without a match (such as "collections/git") contribute
// nothing. Walking errors for individual entries are skipped; a missing storage
// root yields an empty Inventory.
func Scan(storagePath string) *Inventory {
	inv := &Inventory{
		Roles:       []Item{},
		Collections: []Item{},
	}

	// Roles: one directory per role name, one subdirectory per version.
	rolesDir := filepath.Join(storagePath, "roles")
	roleEntries, err := os.ReadDir(rolesDir)
	if err == nil {
		for _, rEntry := range roleEntries {
			if !rEntry.IsDir() {
				continue
			}
			versionsDir := filepath.Join(rolesDir, rEntry.Name())
			verEntries, err := os.ReadDir(versionsDir)
			if err != nil {
				continue
			}

			var versions []Version
			for _, vEntry := range verEntries {
				if !vEntry.IsDir() {
					continue
				}
				versions = append(versions, Version{
					Version:   vEntry.Name(),
					SizeBytes: blockSize(filepath.Join(versionsDir, vEntry.Name())),
				})
			}
			if len(versions) == 0 {
				continue
			}
			sort.Slice(versions, func(i, j int) bool { return versions[i].Version > versions[j].Version })
			inv.Roles = append(inv.Roles, Item{Type: "role", Name: rEntry.Name(), Versions: versions})
		}
	}

	// Collections: one directory per namespace, artifacts named
	// "<namespace>-<name>-<version>.tar.gz".
	colDir := filepath.Join(storagePath, "collections")
	nsEntries, err := os.ReadDir(colDir)
	if err == nil {
		for _, nsEntry := range nsEntries {
			if !nsEntry.IsDir() {
				continue
			}
			namespace := nsEntry.Name()
			nsPath := filepath.Join(colDir, namespace)
			files, err := os.ReadDir(nsPath)
			if err != nil {
				continue
			}

			grouped := make(map[string][]Version)
			for _, f := range files {
				if f.IsDir() {
					continue
				}
				colName, colVer, ok := artifactName(namespace, f.Name())
				if !ok {
					continue
				}
				full := namespace + "." + colName
				path := filepath.Join(nsPath, f.Name())
				grouped[full] = append(grouped[full], Version{Version: colVer, SizeBytes: blockSize(path)})
			}

			for full, versions := range grouped {
				sort.Slice(versions, func(i, j int) bool { return versions[i].Version > versions[j].Version })
				inv.Collections = append(inv.Collections, Item{
					Type:      "collection",
					Namespace: namespace,
					Name:      full,
					Versions:  versions,
				})
			}
		}
	}

	sort.Slice(inv.Roles, func(i, j int) bool { return inv.Roles[i].Name < inv.Roles[j].Name })
	sort.Slice(inv.Collections, func(i, j int) bool { return inv.Collections[i].Name < inv.Collections[j].Name })
	return inv
}

// artifactName extracts the collection name and version from an artifact
// filename of the form "<namespace>-<name>-<version>.tar.gz".
func artifactName(namespace, filename string) (name, version string, ok bool) {
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

// blockSize returns the block-allocated disk usage of path, matching the
// dashboard's DISK USAGE calculation (stat.Blocks * 512, falling back to the
// logical size on filesystems without block accounting).
func blockSize(path string) int64 {
	var size int64
	_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			size += stat.Blocks * 512
			return nil
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	return size
}

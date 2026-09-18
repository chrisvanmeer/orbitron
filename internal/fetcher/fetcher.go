package fetcher

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	yaml "gopkg.in/yaml.v3"

	"orbitron/internal/logger"
	ver "orbitron/internal/version"
)

type CollectionItem struct {
	Name    string `yaml:"name"`
	Src     string `yaml:"src"`
	SCM     string `yaml:"scm"`
	Type    string `yaml:"type"`
	Version string `yaml:"version"`
	Source  string `yaml:"source"`
}

type RoleItem struct {
	Name    string `yaml:"name"`
	Src     string `yaml:"src"`
	SCM     string `yaml:"scm"`
	Version string `yaml:"version"`
}

type RequirementsYML struct {
	Collections []CollectionItem `yaml:"collections"`
	Roles       []RoleItem       `yaml:"roles"`
}

type Fetcher struct {
	storagePath    string
	manifestPath   string
	httpClient     *http.Client
	maxConcurrency int
	tracker        *SyncTracker
}

func NewFetcher(storagePath string, maxConcurrency int) *Fetcher {
	manifestPath := filepath.Join(storagePath, "manifests")
	collectionsPath := filepath.Join(storagePath, "collections")
	rolesPath := filepath.Join(storagePath, "roles")

	dirs := []string{storagePath, manifestPath, collectionsPath, rolesPath}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0750); err != nil {
			logger.Error("Failed to initialize storage directory (%s): %v", dir, err)
		}
	}

	if maxConcurrency <= 0 {
		maxConcurrency = 4
	}

	dialer := &net.Dialer{
		Timeout:   3 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &Fetcher{
		storagePath:    storagePath,
		manifestPath:   manifestPath,
		maxConcurrency: maxConcurrency,
		tracker:        NewSyncTracker(),
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
	}
}

// ParseRequirements parses YAML byte streams into RequirementsYML struct,
// supporting both wrapped maps (roles:/collections:) and bare YAML lists.
func ParseRequirements(data []byte) (*RequirementsYML, error) {
	var reqs RequirementsYML

	// 1. Try standard map format {roles: [...], collections: [...]}
	if err := yaml.Unmarshal(data, &reqs); err == nil && (len(reqs.Roles) > 0 || len(reqs.Collections) > 0) {
		return &reqs, nil
	}

	// 2. Fallback: Try bare list format for roles
	var roleList []RoleItem
	if err := yaml.Unmarshal(data, &roleList); err == nil && len(roleList) > 0 {
		reqs.Roles = roleList
		return &reqs, nil
	}

	// 3. Fallback: Try bare list format for collections
	var colList []CollectionItem
	if err := yaml.Unmarshal(data, &colList); err == nil && len(colList) > 0 {
		reqs.Collections = colList
		return &reqs, nil
	}

	// 4. Return standard unmarshal error if all parsing attempts failed
	err := yaml.Unmarshal(data, &reqs)
	return &reqs, err
}

func (f *Fetcher) SaveManifest(manifestType string, data []byte) error {
	if err := os.MkdirAll(f.manifestPath, 0750); err != nil {
		return err
	}
	// Store manifests under a content hash so multiple distinct manifests
	// can coexist (and re-sync) without re-appending identical ones.
	sum := sha256.Sum256(data)
	name := fmt.Sprintf("%s_%x_requirements.yml", manifestType, sum[:6])
	return os.WriteFile(filepath.Join(f.manifestPath, name), data, 0640)
}

// SaveManifestReplacing stores a requirements manifest, first removing any
// previously stored manifest of the same type that declares an overlapping
// role or collection name, so only the newest declaration for each name
// governs (mirrors the ACTIVE badge's per-name identity).
func (f *Fetcher) SaveManifestReplacing(manifestType string, data []byte) error {
	declared := fetcherDeclaredNames(manifestType, data)

	entries, err := os.ReadDir(f.manifestPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range entries {
		p := filepath.Join(f.manifestPath, entry.Name())
		existing, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for name := range fetcherDeclaredNames(manifestType, existing) {
			if declared[name] {
				_ = os.Remove(p)
				break
			}
		}
	}
	return f.SaveManifest(manifestType, data)
}

// fetcherDeclaredNames returns the identity names declared by a requirements
// manifest, using the same normalization as the dashboard's ACTIVE badge.
func fetcherDeclaredNames(manifestType string, data []byte) map[string]bool {
	names := make(map[string]bool)
	reqs, err := ParseRequirements(data)
	if err != nil {
		return names
	}
	if manifestType == "roles" {
		for _, r := range reqs.Roles {
			if n := extractFetcherRoleName(r); n != "" {
				names[n] = true
			}
		}
	} else {
		for _, c := range reqs.Collections {
			if n := strings.Trim(strings.TrimSpace(c.Name), "\"'"); n != "" {
				names[n] = true
			}
		}
	}
	return names
}

// extractFetcherRoleName mirrors dashboard extractRoleName (name or src, drop
// .git, keep trailing path segment, trim quotes).
func extractFetcherRoleName(r RoleItem) string {
	target := strings.TrimSpace(r.Name)
	if target == "" {
		target = strings.TrimSpace(r.Src)
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

// ManifestMeta describes a single stored requirements manifest as surfaced by
// the management API: its on-disk name, content hash, and parsed entries.
type ManifestMeta struct {
	Type        string           `json:"type"`
	File        string           `json:"file"`
	SHA256      string           `json:"sha256"`
	Roles       []RoleItem       `json:"roles,omitempty"`
	Collections []CollectionItem `json:"collections,omitempty"`
}

// ListManifests returns every stored requirements manifest. Manifests whose
// content can no longer be parsed are still reported (with empty entries) so
// operators can discover and repair them; the SHA-256 covers the raw content
// exactly as POSTed, enabling content-addressed idempotency in automation.
func (f *Fetcher) ListManifests() ([]ManifestMeta, error) {
	entries, err := os.ReadDir(f.manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	metas := make([]ManifestMeta, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(f.manifestPath, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		manifestType := "roles"
		if strings.HasPrefix(entry.Name(), "collections_") {
			manifestType = "collections"
		}

		meta := ManifestMeta{
			Type:   manifestType,
			File:   entry.Name(),
			SHA256: fmt.Sprintf("%x", sha256.Sum256(data)),
		}
		if reqs, err := ParseRequirements(data); err == nil {
			meta.Roles = reqs.Roles
			meta.Collections = reqs.Collections
		}
		metas = append(metas, meta)
	}
	return metas, nil
}

// runConcurrently executes the given jobs with at most f.maxConcurrency workers.
func (f *Fetcher) runConcurrently(jobs []func()) {
	if len(jobs) == 0 {
		return
	}

	workers := f.maxConcurrency
	if workers <= 0 {
		workers = 1
	}
	if len(jobs) < workers {
		workers = len(jobs)
	}

	jobCh := make(chan func())
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobCh {
				job()
			}
		}()
	}

	for _, job := range jobs {
		jobCh <- job
	}
	close(jobCh)
	wg.Wait()
}

// StatusSnapshot returns a concurrency-safe view of the current and recent
// background sync jobs, suitable for JSON serialization in the API layer.
func (f *Fetcher) StatusSnapshot() SyncSnapshot {
	if f.tracker == nil {
		return SyncSnapshot{History: []SyncJob{}}
	}
	return f.tracker.snapshot()
}

// roleItemName returns the best human-readable identity for a role item.
func (f *Fetcher) roleItemName(item RoleItem) string {
	if n := strings.TrimSpace(item.Name); n != "" {
		return n
	}
	return extractFetcherRoleName(item)
}

func (f *Fetcher) ProcessRoles(items []RoleItem) {
	f.tracker.begin(SyncKindRoles, len(items))
	f.runRoleJobs(items)
	f.tracker.end()
	logger.Info("Role requirements sync completed.")
}

func (f *Fetcher) ProcessCollections(items []CollectionItem) {
	f.tracker.begin(SyncKindCollections, len(items))
	f.runCollectionJobs(items)
	f.tracker.end()
	logger.Info("Collection requirements sync completed.")
}

// runRoleJobs executes the given role items concurrently, reporting each
// result to the tracker. It never starts or finishes a tracker job itself, so
// callers can chain a single "full" job around both role and collection sets.
func (f *Fetcher) runRoleJobs(items []RoleItem) {
	jobs := make([]func(), 0, len(items))
	for _, item := range items {
		item := item
		name := f.roleItemName(item)
		jobs = append(jobs, func() {
			err := f.ProcessRole(item)
			f.tracker.itemDone()
			if err != nil {
				logger.Error("Error processing role (%s): %v", item.Name, err)
				f.tracker.itemFailed(name, err)
			}
		})
	}
	f.runConcurrently(jobs)
}

// runCollectionJobs executes the given collection items concurrently,
// reporting each result to the tracker without starting a job of its own.
func (f *Fetcher) runCollectionJobs(items []CollectionItem) {
	jobs := make([]func(), 0, len(items))
	for _, item := range items {
		item := item
		jobs = append(jobs, func() {
			err := f.ProcessCollection(item)
			f.tracker.itemDone()
			if err != nil {
				logger.Error("Error processing collection (%s): %v", item.Name, err)
				f.tracker.itemFailed(strings.TrimSpace(item.Name), err)
			}
		})
	}
	f.runConcurrently(jobs)
}

func (f *Fetcher) SyncAll() {
	logger.Info("Starting parallel sync of all stored manifests...")

	files, err := os.ReadDir(f.manifestPath)
	if err != nil {
		logger.Error("Failed to read manifests directory: %v", err)
		return
	}

	var roles []RoleItem
	var collections []CollectionItem

	for _, file := range files {
		if file.IsDir() {
			continue
		}

		manifestFilePath := filepath.Join(f.manifestPath, file.Name())
		data, err := os.ReadFile(manifestFilePath)
		if err != nil {
			logger.Error("Failed to read manifest %s: %v", file.Name(), err)
			continue
		}

		reqs, err := ParseRequirements(data)
		if err != nil {
			logger.Error("Failed to parse manifest %s: %v", file.Name(), err)
			continue
		}

		roles = append(roles, reqs.Roles...)
		collections = append(collections, reqs.Collections...)
	}

	f.tracker.begin(SyncKindFull, len(roles)+len(collections))

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		f.runRoleJobs(roles)
	}()
	go func() {
		defer wg.Done()
		f.runCollectionJobs(collections)
	}()
	wg.Wait()

	f.tracker.end()
	logger.Info("Full sync job completed.")
}

func (f *Fetcher) SyncGitRepo(gitURL, version, targetDir string) error {
	// Versions already on disk are never re-downloaded. A corrupt version is
	// repaired by deleting it first and letting the next sync fetch it fresh.
	if version != "" && dirExistsNonEmpty(targetDir) {
		logger.Info("Role version %s already exists at %s; skipping", version, targetDir)
		return nil
	}

	if _, err := exec.LookPath("git"); err != nil {
		logger.Error("Git sync failed: 'git' binary is not installed")
		return fmt.Errorf("git executable not found in system PATH")
	}

	if err := os.MkdirAll(filepath.Dir(targetDir), 0750); err != nil {
		return err
	}

	if _, err := os.Stat(filepath.Join(targetDir, ".git")); err == nil {
		logger.Info("Git repo exists at %s. Fetching updates...", targetDir)
		cmdFetch := exec.Command("git", "-C", targetDir, "fetch", "--all", "--tags")
		cmdFetch.Env = os.Environ()
		if err := cmdFetch.Run(); err != nil {
			logger.Warn("Git fetch failed (%v), re-cloning...", err)
			if rmErr := os.RemoveAll(targetDir); rmErr != nil {
				logger.Warn("Failed to remove stale repo at %s: %v", targetDir, rmErr)
			}
		} else {
			if version != "" {
				cmdCheckout := exec.Command("git", "-C", targetDir, "checkout", version)
				cmdCheckout.Env = os.Environ()
				return cmdCheckout.Run()
			}
			return nil
		}
	}

	logger.Info("Cloning Git repo %s (ref: %s)...", gitURL, version)
	args := []string{"clone"}
	if version != "" {
		args = append(args, "--branch", version, "--depth", "1")
	}
	args = append(args, gitURL, targetDir)

	cmd := exec.Command("git", args...)
	cmd.Env = os.Environ()
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone failed (%v): %s", err, string(output))
	}

	logger.Info("Successfully cloned %s into %s", gitURL, targetDir)
	return nil
}

func (f *Fetcher) ProcessCollection(item CollectionItem) error {
	gitURL := item.Src
	if gitURL == "" && (strings.HasPrefix(item.Name, "http://") || strings.HasPrefix(item.Name, "https://") || strings.HasPrefix(item.Name, "git@")) {
		gitURL = item.Name
	}

	isGit := item.SCM == "git" || item.Type == "git" || gitURL != ""

	if isGit {
		if ver.IsAll(item.Version) {
			return fmt.Errorf("version %q is only supported for Galaxy collections; git-sourced collections cannot mirror every version", item.Version)
		}
		repoName := filepath.Base(gitURL)
		repoName = strings.TrimSuffix(repoName, ".git")
		targetDir := filepath.Join(f.storagePath, "collections", "git", repoName)
		return f.SyncGitRepo(gitURL, item.Version, targetDir)
	}

	parts := strings.Split(item.Name, ".")
	if len(parts) == 2 {
		return f.SyncGalaxyCollection(parts[0], parts[1], item.Version)
	}
	return fmt.Errorf("invalid galaxy collection name format: %s", item.Name)
}

// DelRoleVersion removes the cached version directory for a galaxy role and,
// when that version was pinned exactly in a stored requirements manifest,
// removes the pin so a later sync does not silently re-fetch it. Constraint
// or "latest" declarations are left untouched because they are not bound to
// the concrete on-disk version.
func (f *Fetcher) DelRoleVersion(namespace, name, version string) error {
	roleDir := namespace + "." + name
	target := filepath.Join(f.storagePath, "roles", roleDir, filepath.Base(version))
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("failed to delete cached role version: %w", err)
	}
	return f.stripRolePins(namespace, name, version)
}

// stripRolePins rewrites every stored manifest, removing role entries that
// were pinned exactly to namespace.name @ version.
func (f *Fetcher) stripRolePins(namespace, name, version string) error {
	manifestFiles, err := os.ReadDir(f.manifestPath)
	if err != nil {
		return nil
	}

	for _, mf := range manifestFiles {
		if mf.IsDir() {
			continue
		}
		path := filepath.Join(f.manifestPath, mf.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		reqs, err := ParseRequirements(data)
		if err != nil {
			continue
		}

		kept := reqs.Roles[:0]
		removed := false
		for _, r := range reqs.Roles {
			if r.Name == namespace+"."+name && !ver.IsLatest(r.Version) && !ver.IsConstraint(r.Version) && strings.TrimSpace(strings.TrimPrefix(r.Version, "v")) == strings.TrimPrefix(version, "v") {
				removed = true
				continue
			}
			kept = append(kept, r)
		}
		if !removed {
			continue
		}
		reqs.Roles = kept
		if err := saveRequirements(path, reqs); err != nil {
			logger.Warn("Failed to update manifest %s after role deletion: %v", mf.Name(), err)
		}
	}
	return nil
}

// DelCollectionVersion removes a single cached collection artifact and, when
// it was pinned exactly in a stored requirements manifest, removes the pin.
func (f *Fetcher) DelCollectionVersion(namespace, name, version string) error {
	target := filepath.Join(f.storagePath, "collections", namespace, fmt.Sprintf("%s-%s-%s.tar.gz", namespace, name, filepath.Base(version)))
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete cached collection version: %w", err)
	}
	return f.stripCollectionPins(namespace, name, version)
}

// stripCollectionPins rewrites every stored manifest, removing collection
// entries pinned exactly to namespace.name @ version.
func (f *Fetcher) stripCollectionPins(namespace, name, version string) error {
	manifestFiles, err := os.ReadDir(f.manifestPath)
	if err != nil {
		return nil
	}

	for _, mf := range manifestFiles {
		if mf.IsDir() {
			continue
		}
		path := filepath.Join(f.manifestPath, mf.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		reqs, err := ParseRequirements(data)
		if err != nil {
			continue
		}

		kept := reqs.Collections[:0]
		removed := false
		for _, c := range reqs.Collections {
			if c.Name == namespace+"."+name && !ver.IsLatest(c.Version) && !ver.IsConstraint(c.Version) && strings.TrimSpace(strings.TrimPrefix(c.Version, "v")) == strings.TrimPrefix(version, "v") {
				removed = true
				continue
			}
			kept = append(kept, c)
		}
		if !removed {
			continue
		}
		reqs.Collections = kept
		if err := saveRequirements(path, reqs); err != nil {
			logger.Warn("Failed to update manifest %s after collection deletion: %v", mf.Name(), err)
		}
	}
	return nil
}

func saveRequirements(path string, reqs *RequirementsYML) error {
	if len(reqs.Roles) == 0 && len(reqs.Collections) == 0 {
		_ = os.Remove(path)
		return nil
	}
	data, err := yaml.Marshal(reqs)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0640)
}

// dirExistsNonEmpty reports whether path is a directory that already contains
// entries; used to skip content that is already cached on disk.
func dirExistsNonEmpty(path string) bool {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false
	}
	return len(entries) > 0
}

// ProcessRole syncs a single required role, dispatching to Git whenever the
// item carries an SCM of git or an explicit git source URL, and otherwise to
// the Galaxy V1 role API.
func (f *Fetcher) ProcessRole(item RoleItem) error {
	gitURL := item.Src
	if gitURL == "" && (strings.HasPrefix(item.Name, "http://") || strings.HasPrefix(item.Name, "https://") || strings.HasPrefix(item.Name, "git@")) {
		gitURL = item.Name
	}

	version := item.Version
	if version == "" {
		version = "latest"
	}

	isGit := item.SCM == "git" || gitURL != ""

	if isGit {
		if ver.IsAll(item.Version) {
			return fmt.Errorf("version %q is only supported for Galaxy roles; git-sourced roles cannot mirror every version", item.Version)
		}
		roleName := item.Name
		if roleName == "" {
			roleName = filepath.Base(gitURL)
			roleName = strings.TrimSuffix(roleName, ".git")
		}
		targetDir := filepath.Join(f.storagePath, "roles", roleName, version)
		return f.SyncGitRepo(gitURL, item.Version, targetDir)
	}

	parts := strings.Split(item.Name, ".")
	if len(parts) == 2 {
		return f.SyncGalaxyRole(parts[0], parts[1], version)
	}
	return fmt.Errorf("invalid galaxy role name format: %s", item.Name)
}

// fetchRoleVersions returns the published version tags for a Galaxy V1 role.
func (f *Fetcher) fetchRoleVersions(roleID int) ([]string, error) {
	galaxyBase := "https://galaxy.ansible.com"
	base, err := url.Parse(galaxyBase)
	if err != nil {
		return nil, err
	}

	var versions []string
	var nextURL string
	for page := 0; page < 100; page++ {
		if page == 0 {
			nextURL = fmt.Sprintf("%s/api/v1/roles/%d/versions/", galaxyBase, roleID)
		}
		if nextURL == "" {
			break
		}

		resp, err := f.httpClient.Get(nextURL)
		if err != nil {
			return nil, fmt.Errorf("failed to query role versions api: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("role versions api returned status %s", resp.Status)
		}

		var pageData struct {
			Results []struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"results"`
			Next     string `json:"next"`
			NextLink string `json:"next_link"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&pageData)
		_ = resp.Body.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("failed to decode role versions response: %w", decodeErr)
		}

		for _, result := range pageData.Results {
			v := result.Version
			if v == "" {
				v = result.Name
			}
			if v != "" {
				versions = append(versions, v)
			}
		}

		nextURL = pageData.Next
		if nextURL == "" && pageData.NextLink != "" {
			ref, err := url.Parse(pageData.NextLink)
			if err != nil {
				break
			}
			nextURL = base.ResolveReference(ref).String()
		}
	}

	return versions, nil
}

// fetchCollectionVersions returns the published versions for a Galaxy V3
// collection, following the links.next pagination of the versions index.
func (f *Fetcher) fetchCollectionVersions(namespace, name string) ([]string, error) {
	galaxyBase := "https://galaxy.ansible.com"
	base, err := url.Parse(galaxyBase)
	if err != nil {
		return nil, err
	}

	var versions []string
	nextURL := fmt.Sprintf("%s/api/v3/plugin/ansible/content/published/collections/index/%s/%s/versions/", galaxyBase, namespace, name)

	for page := 0; page < 200 && nextURL != ""; page++ {
		resp, err := f.httpClient.Get(nextURL)
		if err != nil {
			return nil, fmt.Errorf("failed to query collection versions api: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("collection versions api returned status %s", resp.Status)
		}

		var pageData struct {
			Links struct {
				Next string `json:"next"`
			} `json:"links"`
			Data []struct {
				Version string `json:"version"`
			} `json:"data"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&pageData)
		_ = resp.Body.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("failed to decode collection versions response: %w", decodeErr)
		}

		for _, item := range pageData.Data {
			if item.Version != "" {
				versions = append(versions, item.Version)
			}
		}

		if pageData.Links.Next == "" {
			break
		}
		ref, err := url.Parse(pageData.Links.Next)
		if err != nil {
			break
		}
		nextURL = base.ResolveReference(ref).String()
	}

	return versions, nil
}

// resolveGalaxyVersion resolves a declared requirement (a concrete version,
// "latest", or a specifier set) against the published versions of a role or
// collection. An empty return with a nil error means "no parseable published
// versions", in which case callers fall back to the default branch.
func resolveGalaxyVersion(versions []string, declared string) (string, error) {
	if !ver.IsConstraint(declared) {
		if ver.IsLatest(declared) {
			picked, err := ver.Highest(versions)
			if err != nil {
				return "", nil
			}
			return picked, nil
		}
		return strings.TrimSpace(declared), nil
	}
	picked, err := ver.Pick(versions, declared)
	if err != nil {
		return "", err
	}
	return picked, nil
}

func (f *Fetcher) SyncGalaxyRole(namespace, name, version string) error {
	apiURL := fmt.Sprintf("https://galaxy.ansible.com/api/v1/roles/?owner__username=%s&name=%s", namespace, name)

	resp, err := f.httpClient.Get(apiURL)
	if err != nil {
		return fmt.Errorf("failed to query galaxy v1 api: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("galaxy v1 api returned status %s", resp.Status)
	}

	var result struct {
		Results []struct {
			ID         int    `json:"id"`
			GitHubUser string `json:"github_user"`
			GitHubRepo string `json:"github_repo"`
		} `json:"results"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("failed to decode galaxy response: %w", err)
	}

	if len(result.Results) == 0 {
		return fmt.Errorf("galaxy role %s.%s not found", namespace, name)
	}

	first := result.Results[0]
	ghUser := first.GitHubUser
	ghRepo := first.GitHubRepo

	gitURL := fmt.Sprintf("https://github.com/%s/%s.git", ghUser, ghRepo)
	roleDir := fmt.Sprintf("%s.%s", namespace, name)

	if ver.IsAll(version) {
		published, err := f.fetchRoleVersions(first.ID)
		if err != nil {
			return fmt.Errorf("failed to resolve versions for galaxy role %s.%s: %w", namespace, name, err)
		}
		if len(published) == 0 {
			logger.Info("Galaxy role %s.%s has no published versions; cloning default branch", namespace, name)
			targetDir := filepath.Join(f.storagePath, "roles", roleDir, "latest")
			return f.SyncGitRepo(gitURL, "", targetDir)
		}
		logger.Info("Mirroring all %d published versions of galaxy role %s.%s", len(published), namespace, name)
		for _, publishedVersion := range published {
			targetDir := filepath.Join(f.storagePath, "roles", roleDir, publishedVersion)
			if err := f.SyncGitRepo(gitURL, publishedVersion, targetDir); err != nil {
				return err
			}
		}
		return nil
	}

	if ver.IsLatest(version) || ver.IsConstraint(version) {
		published, err := f.fetchRoleVersions(first.ID)
		if err != nil {
			return fmt.Errorf("failed to resolve versions for galaxy role %s.%s: %w", namespace, name, err)
		}

		resolved, err := resolveGalaxyVersion(published, version)
		if err != nil {
			return fmt.Errorf("cannot satisfy version %q for galaxy role %s.%s: %w", version, namespace, name, err)
		}
		if resolved == "" {
			// No published tags: fall back to the default branch.
			logger.Info("Galaxy role %s.%s has no published versions; cloning default branch", namespace, name)
			targetDir := filepath.Join(f.storagePath, "roles", roleDir, "latest")
			return f.SyncGitRepo(gitURL, "", targetDir)
		}

		targetDir := filepath.Join(f.storagePath, "roles", roleDir, resolved)
		logger.Info("Resolved Galaxy role %s.%s to %s (version: %s)", namespace, name, gitURL, resolved)
		return f.SyncGitRepo(gitURL, resolved, targetDir)
	}

	targetDir := filepath.Join(f.storagePath, "roles", roleDir, version)
	logger.Info("Resolved Galaxy role %s.%s to Git repo %s (version: %s)", namespace, name, gitURL, version)
	return f.SyncGitRepo(gitURL, version, targetDir)
}

func (f *Fetcher) SyncGalaxyCollection(namespace, name, version string) error {
	if ver.IsAll(version) {
		published, err := f.fetchCollectionVersions(namespace, name)
		if err != nil {
			return fmt.Errorf("failed to resolve versions for galaxy collection %s.%s: %w", namespace, name, err)
		}
		if len(published) == 0 {
			return fmt.Errorf("galaxy collection %s.%s has no published versions", namespace, name)
		}
		logger.Info("Mirroring all %d published versions of galaxy collection %s.%s", len(published), namespace, name)
		for _, publishedVersion := range published {
			if err := f.downloadCollectionArtifact(namespace, name, publishedVersion); err != nil {
				return err
			}
		}
		return nil
	}

	if version == "" || ver.IsLatest(version) || ver.IsConstraint(version) {
		published, err := f.fetchCollectionVersions(namespace, name)
		if err != nil {
			return fmt.Errorf("failed to resolve versions for galaxy collection %s.%s: %w", namespace, name, err)
		}

		resolved, err := ver.Pick(published, version)
		if err != nil {
			return fmt.Errorf("cannot satisfy version %q for galaxy collection %s.%s: %w", version, namespace, name, err)
		}
		version = resolved
	}

	return f.downloadCollectionArtifact(namespace, name, version)
}

// downloadCollectionArtifact fetches a single galaxy collection tarball, but
// only when it is not already cached on disk: content that exists is trusted
// and never re-downloaded. A corrupt artifact is repaired by deleting it and
// letting the next sync fetch it fresh.
func (f *Fetcher) downloadCollectionArtifact(namespace, name, version string) error {
	// Galaxy V3 direct artifact URL scheme: artifacts/namespace-name-version.tar.gz
	downloadURL := fmt.Sprintf("https://galaxy.ansible.com/api/v3/plugin/ansible/content/published/collections/artifacts/%s-%s-%s.tar.gz", namespace, name, version)

	targetDir := filepath.Join(f.storagePath, "collections", namespace)
	targetFile := filepath.Join(targetDir, fmt.Sprintf("%s-%s-%s.tar.gz", namespace, name, version))

	if _, err := os.Stat(targetFile); err == nil {
		logger.Info("Collection %s.%s (%s) already cached; skipping", namespace, name, version)
		return nil
	}

	if err := os.MkdirAll(targetDir, 0750); err != nil {
		return err
	}

	logger.Info("Downloading Galaxy collection %s.%s (%s)...", namespace, name, version)
	resp, err := f.httpClient.Get(downloadURL)
	if err != nil {
		return fmt.Errorf("failed to fetch collection from galaxy: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("galaxy returned status code: %s", resp.Status)
	}

	out, err := os.OpenFile(targetFile, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0640)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return err
	}

	logger.Info("Successfully saved collection archive to %s", targetFile)
	return nil
}

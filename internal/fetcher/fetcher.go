package fetcher

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	yaml "gopkg.in/yaml.v3"

	"orbitron/internal/logger"
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
	storagePath  string
	manifestPath string
	httpClient   *http.Client
}

func NewFetcher(storagePath string) *Fetcher {
	manifestPath := filepath.Join(storagePath, "manifests")
	collectionsPath := filepath.Join(storagePath, "collections")
	rolesPath := filepath.Join(storagePath, "roles")

	dirs := []string{storagePath, manifestPath, collectionsPath, rolesPath}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			logger.Error("Failed to initialize storage directory (%s): %v", dir, err)
		}
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
		storagePath:  storagePath,
		manifestPath: manifestPath,
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

func (f *Fetcher) SaveManifest(filename string, data []byte) error {
	if err := os.MkdirAll(f.manifestPath, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(f.manifestPath, filename), data, 0644)
}

func (f *Fetcher) SyncAll() {
	logger.Info("Starting parallel sync of all stored manifests...")

	files, err := os.ReadDir(f.manifestPath)
	if err != nil {
		logger.Error("Failed to read manifests directory: %v", err)
		return
	}

	var wg sync.WaitGroup

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

		for _, col := range reqs.Collections {
			wg.Add(1)
			go func(c CollectionItem) {
				defer wg.Done()
				if err := f.ProcessCollection(c); err != nil {
					logger.Error("SyncAll collection error (%s): %v", c.Name, err)
				}
			}(col)
		}

		for _, role := range reqs.Roles {
			wg.Add(1)
			go func(r RoleItem) {
				defer wg.Done()
				if err := f.ProcessRole(r); err != nil {
					logger.Error("SyncAll role error (%s): %v", r.Name, err)
				}
			}(role)
		}
	}

	wg.Wait()
	logger.Info("Full sync job completed.")
}

func (f *Fetcher) SyncGitRepo(gitURL, version, targetDir string) error {
	if _, err := exec.LookPath("git"); err != nil {
		logger.Error("Git sync failed: 'git' binary is not installed")
		return fmt.Errorf("git executable not found in system PATH")
	}

	if err := os.MkdirAll(filepath.Dir(targetDir), 0755); err != nil {
		return err
	}

	if _, err := os.Stat(filepath.Join(targetDir, ".git")); err == nil {
		logger.Info("Git repo exists at %s. Fetching updates...", targetDir)
		cmdFetch := exec.Command("git", "-C", targetDir, "fetch", "--all", "--tags")
		cmdFetch.Env = os.Environ()
		if err := cmdFetch.Run(); err != nil {
			logger.Warn("Git fetch failed (%v), re-cloning...", err)
			os.RemoveAll(targetDir)
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

func (f *Fetcher) SyncGalaxyRole(namespace, name, version string) error {
	apiURL := fmt.Sprintf("https://galaxy.ansible.com/api/v1/roles/?owner__username=%s&name=%s", namespace, name)

	resp, err := f.httpClient.Get(apiURL)
	if err != nil {
		return fmt.Errorf("failed to query galaxy v1 api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("galaxy v1 api returned status %s", resp.Status)
	}

	var result struct {
		Results []struct {
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

	ghUser := result.Results[0].GitHubUser
	ghRepo := result.Results[0].GitHubRepo

	gitURL := fmt.Sprintf("https://github.com/%s/%s.git", ghUser, ghRepo)
	targetDir := filepath.Join(f.storagePath, "roles", fmt.Sprintf("%s.%s", namespace, name), version)

	logger.Info("Resolved Galaxy role %s.%s to Git repo %s (version: %s)", namespace, name, gitURL, version)
	return f.SyncGitRepo(gitURL, version, targetDir)
}

func (f *Fetcher) SyncGalaxyCollection(namespace, name, version string) error {
	var downloadURL string

	if version == "" || version == "latest" {
		// Fetch highest available version info from Galaxy V3 API
		apiURL := fmt.Sprintf("https://galaxy.ansible.com/api/v3/plugin/ansible/content/published/collections/index/%s/%s/versions/?is_highest=true", namespace, name)
		resp, err := f.httpClient.Get(apiURL)
		if err != nil {
			return fmt.Errorf("failed to query galaxy collection v3 api: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("galaxy collection v3 api returned status %s", resp.Status)
		}

		var result struct {
			Results []struct {
				Version     string `json:"version"`
				DownloadURL string `json:"download_url"`
			} `json:"results"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || len(result.Results) == 0 {
			return fmt.Errorf("failed to resolve latest version for %s.%s", namespace, name)
		}

		version = result.Results[0].Version
		downloadURL = result.Results[0].DownloadURL
	} else {
		// Galaxy V3 direct artifact URL scheme: artifacts/namespace-name-version.tar.gz
		downloadURL = fmt.Sprintf("https://galaxy.ansible.com/api/v3/plugin/ansible/content/published/collections/artifacts/%s-%s-%s.tar.gz", namespace, name, version)
	}

	targetDir := filepath.Join(f.storagePath, "collections", namespace)
	targetFile := filepath.Join(targetDir, fmt.Sprintf("%s-%s-%s.tar.gz", namespace, name, version))

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return err
	}

	logger.Info("Downloading Galaxy collection %s.%s (%s)...", namespace, name, version)
	resp, err := f.httpClient.Get(downloadURL)
	if err != nil {
		return fmt.Errorf("failed to fetch collection from galaxy: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("galaxy returned status code: %s", resp.Status)
	}

	out, err := os.Create(targetFile)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return err
	}

	logger.Info("Successfully saved collection archive to %s", targetFile)
	return nil
}

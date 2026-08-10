package localrollback

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const scopeConfigFilename = "config.yaml"

// ScopeOptions identifies every input used to choose the file snapshot scope.
type ScopeOptions struct {
	StateDir         string
	WorkingDirectory string
	HomeDirectory    string
	Watch            []string
	WatchOnly        []string
}

// Scope is the resolved set of watched roots and physically resolved exclusions.
type Scope struct {
	Watched  []string
	Excluded []string
}

type scopeConfig struct {
	Watch   []string `yaml:"watch"`
	Exclude []string `yaml:"exclude"`
}

// ScopeConfigPath returns the read-only snapshot-scope configuration filename.
func ScopeConfigPath(stateDir string) string {
	return filepath.Join(stateDir, scopeConfigFilename)
}

// ResolveScope loads the state-directory configuration and applies the documented
// precedence rules for defaults, additive watches, replacing watches, and exclusions.
func ResolveScope(options ScopeOptions) (Scope, error) {
	config, err := loadScopeConfig(options.StateDir, options.HomeDirectory)
	if err != nil {
		return Scope{}, err
	}

	var watched []string
	if len(options.WatchOnly) > 0 {
		watched = append(watched, options.WatchOnly...)
	} else {
		defaults, err := DefaultWatchPaths(options.WorkingDirectory, options.HomeDirectory)
		if err != nil {
			return Scope{}, fmt.Errorf("choose default watched paths: %w", err)
		}
		for _, path := range defaults {
			if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
				continue
			}
			watched = append(watched, path)
		}
		watched = append(watched, config.Watch...)
		watched = append(watched, options.Watch...)
	}

	watched, err = normalizeRoots(watched)
	if err != nil {
		return Scope{}, fmt.Errorf("resolve watched paths: %w", err)
	}
	excluded, err := normalizeExclusions(config.Exclude)
	if err != nil {
		return Scope{}, fmt.Errorf("resolve excluded paths: %w", err)
	}

	filtered := watched[:0]
	for _, root := range watched {
		resolved, resolveErr := resolvePathAllowMissing(root)
		if resolveErr == nil && isExcluded(resolved, excluded) {
			continue
		}
		filtered = append(filtered, root)
	}
	return Scope{Watched: filtered, Excluded: excluded}, nil
}

func loadScopeConfig(stateDir, homeDirectory string) (scopeConfig, error) {
	filename := ScopeConfigPath(stateDir)
	data, err := os.ReadFile(filename)
	if errors.Is(err, os.ErrNotExist) {
		return scopeConfig{}, nil
	}
	if err != nil {
		return scopeConfig{}, fmt.Errorf("read snapshot scope config %s: %w", filename, err)
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var config scopeConfig
	if err := decoder.Decode(&config); err != nil {
		if errors.Is(err, io.EOF) {
			return config, nil
		}
		return scopeConfig{}, fmt.Errorf("load snapshot scope config %s: decode YAML: %w", filename, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return scopeConfig{}, fmt.Errorf("load snapshot scope config %s: multiple YAML documents are not allowed", filename)
		}
		return scopeConfig{}, fmt.Errorf("load snapshot scope config %s: decode trailing YAML: %w", filename, err)
	}

	config.Watch, err = validateConfigPaths(filename, "watch", config.Watch, homeDirectory)
	if err != nil {
		return scopeConfig{}, err
	}
	config.Exclude, err = validateConfigPaths(filename, "exclude", config.Exclude, homeDirectory)
	if err != nil {
		return scopeConfig{}, err
	}
	return config, nil
}

func validateConfigPaths(filename, field string, paths []string, homeDirectory string) ([]string, error) {
	validated := make([]string, 0, len(paths))
	for index, path := range paths {
		if strings.TrimSpace(path) == "" {
			return nil, fmt.Errorf("load snapshot scope config %s: %s[%d] cannot be empty", filename, field, index)
		}
		expanded, err := expandConfigHome(path, homeDirectory)
		if err != nil {
			return nil, fmt.Errorf("load snapshot scope config %s: %s[%d]: %w", filename, field, index, err)
		}
		if !filepath.IsAbs(expanded) {
			return nil, fmt.Errorf("load snapshot scope config %s: %s[%d] path %q must be absolute", filename, field, index, path)
		}
		validated = append(validated, filepath.Clean(expanded))
	}
	return validated, nil
}

func expandConfigHome(path, homeDirectory string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~"+string(os.PathSeparator)) {
		if strings.HasPrefix(path, "~") {
			return "", fmt.Errorf("path %q uses unsupported user-home expansion", path)
		}
		return path, nil
	}
	if homeDirectory == "" {
		return "", fmt.Errorf("expand %q: home directory is unavailable", path)
	}
	if path == "~" {
		return filepath.Clean(homeDirectory), nil
	}
	return filepath.Join(homeDirectory, strings.TrimPrefix(path, "~"+string(os.PathSeparator))), nil
}

func normalizeExclusions(paths []string) ([]string, error) {
	unique := make(map[string]bool)
	resolved := make([]string, 0, len(paths))
	for _, path := range paths {
		physical, err := resolvePathAllowMissing(path)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", path, err)
		}
		if !unique[physical] {
			unique[physical] = true
			resolved = append(resolved, physical)
		}
	}
	sort.Strings(resolved)
	return resolved, nil
}

func resolveExistingPath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func resolvePathAllowMissing(path string) (string, error) {
	path = filepath.Clean(path)
	candidate := path
	var suffix []string
	for {
		_, err := os.Lstat(candidate)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(candidate)
			if err != nil {
				return "", err
			}
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return path, nil
		}
		suffix = append(suffix, filepath.Base(candidate))
		candidate = parent
	}
}

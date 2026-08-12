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

const (
	ChangeListScopeCloneOnly    = "clone-only"
	ChangeListScopeHomeAndClone = "home-and-clone"
	ChangeListScopeWatchOnly    = "watch-only"
)

// ScopeOptions identifies every input used to choose the file snapshot scope.
type ScopeOptions struct {
	StateDir         string
	WorkingDirectory string
	HomeDirectory    string
	Watch            []string
	WatchOnly        []string
}

// Scope is the resolved set of watched roots, physically resolved exclusions,
// and explicit watches that those exclusions prevent from being captured.
type Scope struct {
	Watched           []string
	Excluded          []string
	Uncaptured        []CaptureFailure
	ChangeListScope   string
	ChangeListRoots   []string
	ScanRoot          string
	ScanExcluded      []string
	ScanExcludedNames []string
	ScanError         string
}

type scopeConfigError struct {
	err error
}

func (err *scopeConfigError) Error() string { return err.err.Error() }
func (err *scopeConfigError) Unwrap() error { return err.err }

// IsScopeConfigError reports whether scope resolution failed because the
// user's config document or one of its paths is invalid.
func IsScopeConfigError(err error) bool {
	var configErr *scopeConfigError
	return errors.As(err, &configErr)
}

type scopePreflightError struct {
	failures []CaptureFailure
}

func (err *scopePreflightError) Error() string {
	return formatFailures("explicitly watched paths are unavailable", err.failures)
}

// ScopePreflightFailures returns the explicit watched paths that made scope
// resolution refuse to start. The returned failures are detached from the
// error and are suitable for the durable audit record.
func ScopePreflightFailures(err error) []CaptureFailure {
	var preflightErr *scopePreflightError
	if !errors.As(err, &preflightErr) {
		return nil
	}
	return append([]CaptureFailure(nil), preflightErr.failures...)
}

type scopeEnvironmentError struct {
	err error
}

func (err *scopeEnvironmentError) Error() string { return err.err.Error() }
func (err *scopeEnvironmentError) Unwrap() error { return err.err }

func classifyConfigValidationError(err error) error {
	var environmentErr *scopeEnvironmentError
	if errors.As(err, &environmentErr) {
		return err
	}
	return &scopeConfigError{err: err}
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
	config, err := loadScopeConfig(options.StateDir, options.HomeDirectory, len(options.WatchOnly) == 0)
	if err != nil {
		return Scope{}, err
	}

	homeDirectory := options.HomeDirectory
	var scanRoot string
	var scanError string
	if len(options.WatchOnly) == 0 {
		if homeDirectory == "" {
			homeDirectory, err = os.UserHomeDir()
		}
		if err != nil {
			scanError = "find home directory for widened change-list scan: " + err.Error()
			err = nil
		} else {
			scanRoot, err = resolveExistingPath(homeDirectory)
			if err != nil {
				scanError = "resolve home directory for widened change-list scan: " + err.Error()
				err = nil
			}
		}
	}

	var watched []string
	var explicit []string
	if len(options.WatchOnly) > 0 {
		explicit = append(explicit, options.WatchOnly...)
		watched = append(watched, explicit...)
	} else {
		if homeDirectory == "" {
			return Scope{}, fmt.Errorf("find home directory for default snapshot scope: %s", scanError)
		}
		defaults, err := DefaultWatchPaths(options.WorkingDirectory, homeDirectory)
		if err != nil {
			return Scope{}, fmt.Errorf("choose default watched paths: %w", err)
		}
		for _, path := range defaults {
			if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
				continue
			}
			watched = append(watched, path)
		}
		explicit = append(explicit, config.Watch...)
		explicit = append(explicit, options.Watch...)
		watched = append(watched, explicit...)
	}

	watched, err = normalizeRoots(watched)
	if err != nil {
		return Scope{}, fmt.Errorf("resolve watched paths: %w", err)
	}
	excluded, err := normalizeExclusions(config.Exclude)
	if err != nil {
		return Scope{}, fmt.Errorf("resolve excluded paths: %w", err)
	}

	explicit, err = absoluteUniquePaths(explicit)
	if err != nil {
		return Scope{}, fmt.Errorf("resolve explicitly watched paths: %w", err)
	}
	var missingExplicit []CaptureFailure
	for _, path := range explicit {
		if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
			missingExplicit = append(missingExplicit, CaptureFailure{
				Path: path, Error: "watched path does not exist",
			})
		}
	}
	if len(missingExplicit) > 0 {
		return Scope{}, &scopeConfigError{err: &scopePreflightError{failures: missingExplicit}}
	}
	var uncaptured []CaptureFailure
	for _, path := range explicit {
		resolved, resolveErr := resolvePathAllowMissing(path)
		if resolveErr != nil {
			continue
		}
		if exclusion, excluded := coveringExclusion(resolved, excluded); excluded {
			message := "explicitly watched path is excluded by config: " + exclusion
			if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
				message = "watched path does not exist; " + message
			}
			uncaptured = append(uncaptured, CaptureFailure{
				Path: path, Error: message,
			})
		}
	}

	filtered := watched[:0]
	for _, root := range watched {
		resolved, resolveErr := resolvePathAllowMissing(root)
		if resolveErr == nil && isExcluded(resolved, excluded) && !containsFailure(root, uncaptured) {
			continue
		}
		filtered = append(filtered, root)
	}
	var scanExcluded []string
	var scanExcludedNames []string
	if scanRoot != "" {
		scanExcluded = append(scanExcluded, excluded...)
		scanExcluded = append(scanExcluded,
			filepath.Join(scanRoot, "Library"),
			filepath.Join(scanRoot, "go", "pkg"),
		)
		scanExcluded, err = normalizeExclusions(scanExcluded)
		if err != nil {
			return Scope{}, fmt.Errorf("resolve widened scan exclusions: %w", err)
		}
		scanExcludedNames = []string{"node_modules", ".git", ".cache"}
	}
	changeListScope := ChangeListScopeCloneOnly
	var changeListRoots []string
	if scanRoot != "" {
		changeListScope = ChangeListScopeHomeAndClone
		changeListRoots = append(changeListRoots, scanRoot)
	}
	changeListRoots = append(changeListRoots, filtered...)
	if len(options.WatchOnly) > 0 {
		changeListScope = ChangeListScopeWatchOnly
		changeListRoots = append([]string(nil), filtered...)
	}
	return Scope{
		Watched: filtered, Excluded: excluded, Uncaptured: uncaptured,
		ChangeListScope: changeListScope, ChangeListRoots: changeListRoots,
		ScanRoot: scanRoot, ScanExcluded: scanExcluded,
		ScanExcludedNames: scanExcludedNames,
		ScanError:         scanError,
	}, nil
}

func loadScopeConfig(stateDir, homeDirectory string, includeWatch bool) (scopeConfig, error) {
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
		return scopeConfig{}, &scopeConfigError{err: fmt.Errorf("load snapshot scope config %s: decode YAML: %w", filename, err)}
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return scopeConfig{}, &scopeConfigError{err: fmt.Errorf("load snapshot scope config %s: multiple YAML documents are not allowed", filename)}
		}
		return scopeConfig{}, &scopeConfigError{err: fmt.Errorf("load snapshot scope config %s: decode trailing YAML: %w", filename, err)}
	}

	if includeWatch {
		config.Watch, err = validateConfigPaths(filename, "watch", config.Watch, homeDirectory)
		if err != nil {
			return scopeConfig{}, classifyConfigValidationError(err)
		}
	} else {
		if err := validateConfigPathForms(filename, "watch", config.Watch); err != nil {
			return scopeConfig{}, &scopeConfigError{err: err}
		}
		config.Watch = nil
	}
	config.Exclude, err = validateConfigPaths(filename, "exclude", config.Exclude, homeDirectory)
	if err != nil {
		return scopeConfig{}, classifyConfigValidationError(err)
	}
	return config, nil
}

func validateConfigPathForms(filename, field string, paths []string) error {
	for index, path := range paths {
		if strings.TrimSpace(path) == "" {
			return fmt.Errorf("load snapshot scope config %s: %s[%d] cannot be empty", filename, field, index)
		}
		if path == "~" || strings.HasPrefix(path, "~"+string(os.PathSeparator)) {
			continue
		}
		if strings.HasPrefix(path, "~") {
			return fmt.Errorf("load snapshot scope config %s: %s[%d]: path %q uses unsupported user-home expansion", filename, field, index, path)
		}
		if !filepath.IsAbs(path) {
			return fmt.Errorf("load snapshot scope config %s: %s[%d] path %q must be absolute", filename, field, index, path)
		}
	}
	return nil
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
		var err error
		homeDirectory, err = os.UserHomeDir()
		if err != nil {
			return "", &scopeEnvironmentError{err: fmt.Errorf("expand %q: find home directory: %w", path, err)}
		}
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

func absoluteUniquePaths(paths []string) ([]string, error) {
	unique := make(map[string]bool)
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		absolute = filepath.Clean(absolute)
		if !unique[absolute] {
			unique[absolute] = true
			result = append(result, absolute)
		}
	}
	sort.Strings(result)
	return result, nil
}

func coveringExclusion(path string, excluded []string) (string, bool) {
	for _, exclusion := range excluded {
		if path == exclusion || strings.HasPrefix(path, exclusion+string(os.PathSeparator)) {
			return exclusion, true
		}
	}
	return "", false
}

func containsFailure(root string, failures []CaptureFailure) bool {
	for _, failure := range failures {
		if failure.Path == root || strings.HasPrefix(failure.Path, root+string(os.PathSeparator)) {
			return true
		}
	}
	return false
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

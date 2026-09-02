package edc

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// remoteCandidateLimit는 디렉터리에 YAML이 많아도 선택기가 길어지지 않게 한다.
const remoteCandidateLimit = 30

// remoteYAMLFiles는 탐색 디렉터리의 YAML 파일 경로를 모은다. cwd가 config 디렉터리보다 앞선다.
func remoteYAMLFiles(cwd, configDir string) []string {
	directories := []string{cwd}
	if configDir != "" {
		directories = append(directories, filepath.Join(configDir, "edc"))
	}
	var paths []string
	seen := make(map[string]struct{})
	for _, directory := range directories {
		entries, err := os.ReadDir(directory)
		if err != nil {
			continue
		}
		var names []string
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			if extension := strings.ToLower(filepath.Ext(entry.Name())); extension != ".yaml" && extension != ".yml" {
				continue
			}
			names = append(names, entry.Name())
		}
		sort.Strings(names)
		for _, name := range names {
			path := filepath.Join(directory, name)
			if _, exists := seen[path]; exists {
				continue
			}
			seen[path] = struct{}{}
			paths = append(paths, path)
			if len(paths) >= remoteCandidateLimit {
				return paths
			}
		}
	}
	return paths
}

// remoteInventoryCandidates는 실제로 inventory로 읽히는 파일만 남긴다. 읽히지 않는 YAML은 목록에서 뺀다.
func remoteInventoryCandidates(cwd, configDir string) []selectItem {
	var items []selectItem
	for _, path := range remoteYAMLFiles(cwd, configDir) {
		inventory, err := loadRemoteInventory(path)
		if err != nil {
			continue
		}
		items = append(items, selectItem{
			label: T("remote.label.inventory_candidate", remoteDisplayPath(cwd, path), len(inventory.Groups), len(inventory.Hosts)),
			value: path,
		})
	}
	return items
}

// remoteRecipeCandidates는 실제로 recipe로 읽히는 파일만 남긴다.
func remoteRecipeCandidates(cwd, configDir string, defaultTimeout time.Duration) []selectItem {
	var items []selectItem
	for _, path := range remoteYAMLFiles(cwd, configDir) {
		recipe, err := loadRemoteRecipe(path, defaultTimeout)
		if err != nil {
			continue
		}
		items = append(items, selectItem{
			label: T("remote.label.recipe_candidate", remoteDisplayPath(cwd, path), recipe.Name, len(recipe.Steps)),
			value: path,
		})
	}
	return items
}

// remoteDisplayPath는 현재 디렉터리 안의 파일을 짧게 보여 준다.
func remoteDisplayPath(cwd, path string) string {
	if relative, err := filepath.Rel(cwd, path); err == nil && !strings.HasPrefix(relative, "..") {
		return relative
	}
	return path
}

package edc

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// routeExit은 .edc/exits.yaml의 항목 하나다. switch --to는 이 이름만 받는다(R17). 자유 입력
// next-hop을 받으면 오타가 있어도 ip route replace가 exit 0을 돌려주기 때문이다.
type routeExit struct {
	Name           string `yaml:"name"`
	Via            string `yaml:"via"`
	Dev            string `yaml:"dev"`
	ExpectPublicIP string `yaml:"expect_public_ip"`
}

// routeExitsFile은 .edc/exits.yaml 전체다. dest가 비어 있으면 routeDefaultDest를 쓴다.
type routeExitsFile struct {
	Dest  string      `yaml:"dest"`
	Exits []routeExit `yaml:"exits"`
}

// loadRouteExits는 overridePath가 있으면 그 경로를, 없으면 discoverRemoteFile(cwd, configDir,
// "exits.yaml")로 찾은 경로를 읽는다. remote가 이미 쓰는 탐색 관례를 그대로 재사용한다(A01).
func loadRouteExits(cwd, configDir, overridePath string) (routeExitsFile, string, error) {
	path := overridePath
	if path == "" {
		found, ok := discoverRemoteFile(cwd, configDir, "exits.yaml")
		if !ok {
			return routeExitsFile{}, "", fmt.Errorf("%s", T("route.exits.error.not_found"))
		}
		path = found
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return routeExitsFile{}, path, err
	}
	var file routeExitsFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return routeExitsFile{}, path, fmt.Errorf("%s: %w", path, err)
	}
	if file.Dest == "" {
		file.Dest = routeDefaultDest
	}
	for _, exit := range file.Exits {
		if exit.Name == "" || exit.Via == "" || exit.Dev == "" {
			return routeExitsFile{}, path, fmt.Errorf("%s", T("route.exits.error.incomplete_entry", path))
		}
	}
	return file, path, nil
}

// findRouteExit은 이름으로 출구 하나를 고른다.
func findRouteExit(file routeExitsFile, name string) (routeExit, bool) {
	for _, exit := range file.Exits {
		if exit.Name == name {
			return exit, true
		}
	}
	return routeExit{}, false
}

// routeExitNames는 --to가 틀렸을 때 사용 가능한 이름을 보여 주는 데 쓴다.
func routeExitNames(file routeExitsFile) []string {
	names := make([]string, 0, len(file.Exits))
	for _, exit := range file.Exits {
		names = append(names, exit.Name)
	}
	return names
}

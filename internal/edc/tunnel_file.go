package edc

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

func defaultTunnelFile(cwd string) string {
	return filepath.Join(cwd, remoteProjectDirectory, tunnelFileName)
}

// editTunnelFile은 yaml.Node 나무를 고쳐 쓰므로 사용자가 적은 주석과 순서가 남는다.
func editTunnelFile(path string, edit func(tunnels *yaml.Node) error) error {
	var document yaml.Node
	mode := os.FileMode(0o600)
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if info, statErr := os.Stat(path); statErr == nil {
			mode = info.Mode().Perm()
		}
		if len(bytes.TrimSpace(data)) > 0 {
			if err := yaml.Unmarshal(data, &document); err != nil {
				return err
			}
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	if document.Kind == 0 {
		document = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return tunnelError("tunnel.error.file_shape", path)
	}
	var tunnels *yaml.Node
	for index := 0; index+1 < len(root.Content); index += 2 {
		if root.Content[index].Value == "tunnels" {
			tunnels = root.Content[index+1]
		}
	}
	if tunnels == nil {
		tunnels = &yaml.Node{Kind: yaml.MappingNode}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "tunnels"}, tunnels)
	}
	if tunnels.Kind != yaml.MappingNode {
		if tunnels.Tag != "!!null" {
			return tunnelError("tunnel.error.file_shape", path)
		}
		*tunnels = yaml.Node{Kind: yaml.MappingNode}
	}
	if err := edit(tunnels); err != nil {
		return err
	}
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		return err
	}
	if err := encoder.Close(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(path, output.Bytes(), mode)
}

func findTunnelEntry(tunnels *yaml.Node, name string) int {
	for index := 0; index+1 < len(tunnels.Content); index += 2 {
		if tunnels.Content[index].Value == name {
			return index
		}
	}
	return -1
}

func saveTunnelDefinition(path, name string, spec tunnelSpec) error {
	return editTunnelFile(path, func(tunnels *yaml.Node) error {
		if findTunnelEntry(tunnels, name) >= 0 {
			return tunnelError("tunnel.error.duplicate", name, path)
		}
		var value yaml.Node
		if err := value.Encode(spec); err != nil {
			return err
		}
		key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}
		tunnels.Content = append(tunnels.Content, key, &value)
		return nil
	})
}

func removeTunnelDefinition(path, name string) error {
	return editTunnelFile(path, func(tunnels *yaml.Node) error {
		index := findTunnelEntry(tunnels, name)
		if index < 0 {
			return tunnelError("tunnel.error.unknown", name)
		}
		tunnels.Content = append(tunnels.Content[:index], tunnels.Content[index+2:]...)
		return nil
	})
}

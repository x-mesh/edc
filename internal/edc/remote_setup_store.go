package edc

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"

	"gopkg.in/yaml.v3"

	"github.com/x-mesh/edc/internal/remotesetup"
)

// remoteInventoryStore keeps an inventory file as a yaml.Node tree. Saving edits that tree,
// so the comments, quoting, and order the user wrote survive an edit.
type remoteInventoryStore struct {
	path string
	root *yaml.Node
}

type remoteInventoryRevision struct{ snapshot configSnapshot }

var remoteSetupChown = os.Chown
var remoteSetupEUID = os.Geteuid

// openRemoteFile reads an inventory or recipe for editing. data is nil when the file is missing or blank.
func openRemoteFile(path string) (data []byte, snapshot configSnapshot, err error) {
	snapshot, err = configFileSnapshot(path)
	if err != nil || !snapshot.exists {
		return nil, snapshot, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, snapshot, err
	}
	if !info.Mode().IsRegular() {
		return nil, snapshot, errors.New(T("remotesetup.error.not_regular", path))
	}
	data, err = readRemoteFile(path)
	if err != nil {
		return nil, snapshot, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		data = nil
	}
	return data, snapshot, nil
}

func readRemoteFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(&io.LimitedReader{R: file, N: remoteConfigLimit + 1})
	if err != nil {
		return nil, err
	}
	if len(data) > remoteConfigLimit {
		return nil, errors.New(T("remote.error.file_too_large", remoteConfigLimit))
	}
	return data, nil
}

func encodeRemoteDocument(document *yaml.Node) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err := encoder.Encode(document); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func (store *remoteInventoryStore) Load() (remotesetup.Inventory, remotesetup.Revision, error) {
	store.root = nil
	data, snapshot, err := openRemoteFile(store.path)
	if err != nil {
		return remotesetup.Inventory{}, nil, err
	}
	revision := remoteInventoryRevision{snapshot: snapshot}
	if data == nil {
		return remotesetup.Inventory{}, revision, nil
	}
	inventory, root, err := parseRemoteInventory(data)
	if err != nil {
		return remotesetup.Inventory{}, nil, fmt.Errorf("inventory %q: %w", store.path, err)
	}
	store.root = root
	return inventory, revision, nil
}

// parseRemoteInventory reads the file strictly, as edc remote does, and keeps the node tree and the group order.
func parseRemoteInventory(data []byte) (remotesetup.Inventory, *yaml.Node, error) {
	var parsed remoteInventory
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&parsed); err != nil {
		return remotesetup.Inventory{}, nil, err
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return remotesetup.Inventory{}, nil, errors.New(T("remote.error.single_document"))
		}
		return remotesetup.Inventory{}, nil, err
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return remotesetup.Inventory{}, nil, err
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return remotesetup.Inventory{}, nil, errors.New(T("remotesetup.error.not_mapping"))
	}
	inventory := remotesetup.Inventory{Parallel: parsed.Parallel}
	for _, host := range parsed.Hosts {
		inventory.Hosts = append(inventory.Hosts, remotesetup.Host{Name: host.Name, Target: host.Target, Tags: host.Tags})
	}
	if groups := mappingValue(document.Content[0], "groups"); groups != nil && groups.Kind == yaml.MappingNode {
		for index := 0; index+1 < len(groups.Content); index += 2 {
			name := groups.Content[index].Value
			inventory.Groups = append(inventory.Groups, remotesetup.Group{Name: name, Members: parsed.Groups[name]})
		}
	}
	for name, options := range parsed.Options {
		if inventory.GroupParallel == nil {
			inventory.GroupParallel = map[string]int{}
		}
		inventory.GroupParallel[name] = options.Parallel
	}
	return inventory, &document, nil
}

func (store *remoteInventoryStore) Save(inventory remotesetup.Inventory, revision remotesetup.Revision) error {
	loaded, ok := revision.(remoteInventoryRevision)
	if !ok {
		return errors.New("remote inventory store: unknown revision")
	}
	document := store.root
	if document == nil {
		document = &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	updateRemoteInventoryNode(document.Content[0], inventory)
	data, err := encodeRemoteDocument(document)
	if err != nil {
		return err
	}
	// The edited tree must read back as exactly the inventory that was asked for, or nothing is written.
	written, _, err := parseRemoteInventory(data)
	if err != nil || !reflect.DeepEqual(canonicalRemoteInventory(written), canonicalRemoteInventory(inventory)) {
		return errors.New(T("remotesetup.error.encode_mismatch"))
	}
	return writeRemoteFileIfUnchanged(store.path, data, loaded.snapshot)
}

// canonicalRemoteInventory makes nil and empty collections compare equal.
func canonicalRemoteInventory(inventory remotesetup.Inventory) remotesetup.Inventory {
	canonical := inventory.Clone()
	for index := range canonical.Hosts {
		if len(canonical.Hosts[index].Tags) == 0 {
			canonical.Hosts[index].Tags = nil
		}
	}
	if len(canonical.GroupParallel) == 0 {
		canonical.GroupParallel = nil
	}
	return canonical
}

// writeRemoteFileIfUnchanged replaces an inventory or recipe file atomically with mode 0600 for a new file and
// the existing mode for an existing one. Unlike the config writer it never changes the directory mode,
// because these files often sit in a project directory.
func writeRemoteFileIfUnchanged(path string, data []byte, original configSnapshot) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	var owner remoteFileOwner
	if original.exists {
		mode = original.mode.Perm()
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		owner = remoteOwnerOf(info)
	}
	file, err := os.CreateTemp(directory, ".remote-*.yaml")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	// Only root can hand a file back to another owner; a non-root save keeps the
	// usual rename semantics so a group-shared file stays editable.
	if owner.ok && remoteSetupEUID() == 0 && owner.uid != 0 {
		if err := remoteSetupChown(temporary, owner.uid, owner.gid); err != nil {
			file.Close()
			return err
		}
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	current, err := configFileSnapshot(path)
	if err != nil {
		return err
	}
	if current != original {
		return remotesetup.ErrStale
	}
	return os.Rename(temporary, path)
}

// updateRemoteInventoryNode makes the mapping describe inventory and keeps every node that already says the same thing.
func updateRemoteInventoryNode(root *yaml.Node, inventory remotesetup.Inventory) {
	updateHostsNode(root, inventory.Hosts)
	updateGroupsNode(root, inventory.Groups)
	setIntKey(root, "parallel", inventory.Parallel)
	updateGroupOptionsNode(root, inventory)
}

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	_, value := mappingEntry(mapping, key)
	return value
}

func mappingEntry(mapping *yaml.Node, key string) (int, *yaml.Node) {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return index, mapping.Content[index+1]
		}
	}
	return -1, nil
}

func setMappingValue(mapping *yaml.Node, key string, value *yaml.Node) {
	if index, _ := mappingEntry(mapping, key); index >= 0 {
		mapping.Content[index+1] = value
		return
	}
	mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

func deleteMappingKey(mapping *yaml.Node, key string) {
	if index, _ := mappingEntry(mapping, key); index >= 0 {
		mapping.Content = append(mapping.Content[:index], mapping.Content[index+2:]...)
	}
}

func stringNode(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

func stringList(node *yaml.Node) []string {
	if node == nil || node.Kind != yaml.SequenceNode {
		return nil
	}
	values := make([]string, 0, len(node.Content))
	for _, item := range node.Content {
		values = append(values, item.Value)
	}
	return values
}

func sameStrings(left, right []string) bool {
	return len(left) == len(right) && (len(left) == 0 || reflect.DeepEqual(left, right))
}

// stringListNode keeps the nodes of the old list for values that stay, so their comments stay.
func stringListNode(values []string, previous *yaml.Node, style yaml.Style) *yaml.Node {
	existing := map[string]*yaml.Node{}
	if previous != nil && previous.Kind == yaml.SequenceNode {
		style = previous.Style
		for _, item := range previous.Content {
			existing[item.Value] = item
		}
	}
	list := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: style}
	for _, value := range values {
		if item, ok := existing[value]; ok {
			list.Content = append(list.Content, item)
			continue
		}
		list.Content = append(list.Content, stringNode(value))
	}
	return list
}

func setStringKey(mapping *yaml.Node, key, value string) {
	if value == "" {
		deleteMappingKey(mapping, key)
		return
	}
	if current := mappingValue(mapping, key); current != nil && current.Value == value {
		return
	}
	if current := mappingValue(mapping, key); current != nil {
		current.Kind, current.Tag, current.Value, current.Style = yaml.ScalarNode, "!!str", value, 0
		return
	}
	setMappingValue(mapping, key, stringNode(value))
}

func setIntKey(mapping *yaml.Node, key string, value int) {
	if current := mappingValue(mapping, key); current != nil {
		if parsed, err := strconv.Atoi(current.Value); err == nil && parsed == value {
			return
		}
	}
	if value == 0 {
		deleteMappingKey(mapping, key)
		return
	}
	setMappingValue(mapping, key, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(value)})
}

// setTagsKey keeps an explicit empty list and a list that already says the same thing.
func setTagsKey(item *yaml.Node, tags []string) {
	current := mappingValue(item, "tags")
	switch {
	case len(tags) == 0:
		if current != nil && len(current.Content) > 0 {
			deleteMappingKey(item, "tags")
		}
	case current == nil || !sameStrings(stringList(current), tags):
		setMappingValue(item, "tags", stringListNode(tags, current, yaml.FlowStyle))
	}
}

func updateHostsNode(root *yaml.Node, hosts []remotesetup.Host) {
	previous := mappingValue(root, "hosts")
	existing := map[string]*yaml.Node{}
	if previous != nil && previous.Kind == yaml.SequenceNode {
		for _, item := range previous.Content {
			if item.Kind == yaml.MappingNode {
				if name := mappingValue(item, "name"); name != nil {
					existing[name.Value] = item
				}
			}
		}
	}
	list := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	if previous != nil {
		list.HeadComment, list.LineComment, list.FootComment = previous.HeadComment, previous.LineComment, previous.FootComment
	}
	for _, host := range hosts {
		item := existing[host.Name]
		if item == nil {
			item = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setMappingValue(item, "name", stringNode(host.Name))
		}
		setStringKey(item, "target", host.Target)
		setTagsKey(item, host.Tags)
		list.Content = append(list.Content, item)
	}
	setMappingValue(root, "hosts", list)
}

func updateGroupsNode(root *yaml.Node, groups []remotesetup.Group) {
	previous := mappingValue(root, "groups")
	existingKeys := map[string]*yaml.Node{}
	existingValues := map[string]*yaml.Node{}
	if previous != nil && previous.Kind == yaml.MappingNode {
		for index := 0; index+1 < len(previous.Content); index += 2 {
			existingKeys[previous.Content[index].Value] = previous.Content[index]
			existingValues[previous.Content[index].Value] = previous.Content[index+1]
		}
	}
	mapping := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if previous != nil {
		mapping.HeadComment, mapping.LineComment, mapping.FootComment = previous.HeadComment, previous.LineComment, previous.FootComment
	}
	for _, group := range groups {
		key := existingKeys[group.Name]
		if key == nil {
			key = stringNode(group.Name)
		}
		value := existingValues[group.Name]
		if value == nil || !sameStrings(stringList(value), group.Members) {
			replacement := stringListNode(group.Members, value, 0)
			if value != nil {
				replacement.HeadComment, replacement.LineComment, replacement.FootComment = value.HeadComment, value.LineComment, value.FootComment
			}
			value = replacement
		}
		mapping.Content = append(mapping.Content, key, value)
	}
	setMappingValue(root, "groups", mapping)
}

func updateGroupOptionsNode(root *yaml.Node, inventory remotesetup.Inventory) {
	var desired []remotesetup.Group
	for _, group := range inventory.Groups {
		if inventory.GroupParallel[group.Name] > 0 {
			desired = append(desired, group)
		}
	}
	if len(desired) == 0 {
		deleteMappingKey(root, "group_options")
		return
	}
	previous := mappingValue(root, "group_options")
	existing := map[string]*yaml.Node{}
	existingKeys := map[string]*yaml.Node{}
	if previous != nil && previous.Kind == yaml.MappingNode {
		for index := 0; index+1 < len(previous.Content); index += 2 {
			existingKeys[previous.Content[index].Value] = previous.Content[index]
			existing[previous.Content[index].Value] = previous.Content[index+1]
		}
	}
	mapping := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if previous != nil {
		mapping.HeadComment, mapping.LineComment, mapping.FootComment = previous.HeadComment, previous.LineComment, previous.FootComment
	}
	for _, group := range desired {
		key := existingKeys[group.Name]
		if key == nil {
			key = stringNode(group.Name)
		}
		options := existing[group.Name]
		if options == nil || options.Kind != yaml.MappingNode {
			options = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		}
		setIntKey(options, "parallel", inventory.GroupParallel[group.Name])
		mapping.Content = append(mapping.Content, key, options)
	}
	setMappingValue(root, "group_options", mapping)
}

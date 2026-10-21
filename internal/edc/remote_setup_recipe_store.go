package edc

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/x-mesh/edc/internal/remotesetup"
)

// remoteRecipeStore is the recipe counterpart of remoteInventoryStore: it edits the yaml.Node tree
// so the comments the user wrote survive.
type remoteRecipeStore struct {
	path string
	root *yaml.Node
}

func (store *remoteRecipeStore) Load() (remotesetup.Recipe, remotesetup.Revision, error) {
	store.root = nil
	data, snapshot, err := openRemoteFile(store.path)
	if err != nil {
		return remotesetup.Recipe{}, nil, err
	}
	revision := remoteInventoryRevision{snapshot: snapshot}
	if data == nil {
		return remotesetup.Recipe{}, revision, nil
	}
	recipe, root, err := parseRemoteRecipe(data)
	if err != nil {
		return remotesetup.Recipe{}, nil, fmt.Errorf("recipe %q: %w", store.path, err)
	}
	store.root = root
	return recipe, revision, nil
}

func parseRemoteRecipe(data []byte) (remotesetup.Recipe, *yaml.Node, error) {
	var parsed remoteRecipeFile
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&parsed); err != nil {
		return remotesetup.Recipe{}, nil, err
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return remotesetup.Recipe{}, nil, errors.New(T("remote.error.single_document"))
		}
		return remotesetup.Recipe{}, nil, err
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return remotesetup.Recipe{}, nil, err
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return remotesetup.Recipe{}, nil, errors.New(T("remotesetup.error.recipe_not_mapping"))
	}
	recipe := remotesetup.Recipe{Name: parsed.Name}
	for _, value := range parsed.Steps {
		step := remotesetup.Step{Name: value.Name, Command: value.Command, Verify: value.Verify, Timeout: value.Timeout, Tags: value.Tags}
		if value.Upload != nil {
			step.Upload = &remotesetup.Upload{Source: value.Upload.Source, Destination: value.Upload.Destination, Mode: value.Upload.Mode}
		}
		recipe.Steps = append(recipe.Steps, step)
	}
	return recipe, &document, nil
}

func (store *remoteRecipeStore) Save(recipe remotesetup.Recipe, revision remotesetup.Revision) error {
	loaded, ok := revision.(remoteInventoryRevision)
	if !ok {
		return errors.New("remote recipe store: unknown revision")
	}
	document := store.root
	if document == nil {
		document = &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	updateRemoteRecipeNode(document.Content[0], recipe)
	data, err := encodeRemoteDocument(document)
	if err != nil {
		return err
	}
	// The edited tree must read back as exactly the recipe that was asked for, or nothing is written.
	written, _, err := parseRemoteRecipe(data)
	if err != nil || !reflect.DeepEqual(canonicalRemoteRecipe(written), canonicalRemoteRecipe(recipe)) {
		return errors.New(T("remotesetup.error.encode_mismatch"))
	}
	return writeRemoteFileIfUnchanged(store.path, data, loaded.snapshot)
}

func canonicalRemoteRecipe(recipe remotesetup.Recipe) remotesetup.Recipe {
	canonical := recipe.Clone()
	for index := range canonical.Steps {
		if len(canonical.Steps[index].Tags) == 0 {
			canonical.Steps[index].Tags = nil
		}
	}
	return canonical
}

func updateRemoteRecipeNode(root *yaml.Node, recipe remotesetup.Recipe) {
	setStringKey(root, "name", recipe.Name)
	previous := mappingValue(root, "steps")
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
	for _, step := range recipe.Steps {
		item := existing[step.Name]
		if item == nil {
			item = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setMappingValue(item, "name", stringNode(step.Name))
		}
		setStringKey(item, "command", step.Command)
		updateUploadNode(item, step.Upload)
		setStringKey(item, "verify", step.Verify)
		setStringKey(item, "timeout", step.Timeout)
		setTagsKey(item, step.Tags)
		list.Content = append(list.Content, item)
	}
	setMappingValue(root, "steps", list)
}

func updateUploadNode(item *yaml.Node, upload *remotesetup.Upload) {
	if upload == nil {
		deleteMappingKey(item, "upload")
		return
	}
	node := mappingValue(item, "upload")
	if node == nil || node.Kind != yaml.MappingNode {
		node = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setMappingValue(item, "upload", node)
	}
	setStringKey(node, "source", upload.Source)
	setStringKey(node, "destination", upload.Destination)
	setStringKey(node, "mode", upload.Mode)
}

// remoteSetupRecipeValidator applies the rules of edc remote to the recipe.
type remoteSetupRecipeValidator struct{}

func remoteRecipeFileOf(recipe remotesetup.Recipe) remoteRecipeFile {
	file := remoteRecipeFile{Name: recipe.Name}
	for _, step := range recipe.Steps {
		value := remoteStepFile{Name: step.Name, Command: step.Command, Verify: step.Verify, Timeout: step.Timeout, Tags: append([]string(nil), step.Tags...)}
		if step.Upload != nil {
			value.Upload = &remoteUploadFile{Source: step.Upload.Source, Destination: step.Upload.Destination, Mode: step.Upload.Mode}
		}
		file.Steps = append(file.Steps, value)
	}
	return file
}

func (remoteSetupRecipeValidator) ValidateStep(step remotesetup.Step) *remotesetup.Issue {
	_, err := buildRemoteRecipe(remoteRecipeFileOf(remotesetup.Recipe{Name: "step", Steps: []remotesetup.Step{step}}), time.Minute)
	return remoteSetupIssue(err)
}

func (remoteSetupRecipeValidator) ValidateRecipe(recipe remotesetup.Recipe) []remotesetup.Issue {
	if issue := remoteSetupIssue(func() error {
		_, err := buildRemoteRecipe(remoteRecipeFileOf(recipe), time.Minute)
		return err
	}()); issue != nil {
		return []remotesetup.Issue{*issue}
	}
	return nil
}

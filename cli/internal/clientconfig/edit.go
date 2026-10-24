package clientconfig

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"gopkg.in/yaml.v3"
)

// ErrNoFile is an edit with nowhere to write: FileVar set empty, which reads
// no file (ADR 26-10-14-894 §3).
var ErrNoFile = fmt.Errorf("%s is set empty, so no client.yaml is read or written", FileVar)

// AddSkill appends entry to run.skills in the file Path names, and returns the
// file. The file is edited in place, so whatever else it holds, its comments
// among it, stays as it was. A file that is not there is created, and so are
// run and its skills; a file that still says new has it renamed run. An entry already there, as a create reads it, is
// refused.
//
// entry is written as given, so a directory should already be absolute: a
// relative one in the file is relative to the file.
func AddSkill(entry string) (string, error) {
	return editSkills(func(path string, skills []string) ([]string, error) {
		key, err := SkillKey(filepath.Dir(path), entry)
		if err != nil {
			return nil, fmt.Errorf("run.skills entry %q %w", entry, err)
		}
		for _, existing := range skills {
			if same, err := SkillKey(filepath.Dir(path), existing); err == nil && same == key {
				return nil, fmt.Errorf("%s already names %s in run.skills", path, entry)
			}
		}
		return append(skills, entry), nil
	})
}

// RemoveSkill takes every run.skills entry that names entry, as a create reads
// both, out of the file Path names, and returns the file. One that names
// nothing there is refused.
func RemoveSkill(entry string) (string, error) {
	return editSkills(func(path string, skills []string) ([]string, error) {
		key, err := SkillKey(filepath.Dir(path), entry)
		if err != nil {
			return nil, fmt.Errorf("run.skills entry %q %w", entry, err)
		}
		kept := slices.DeleteFunc(slices.Clone(skills), func(existing string) bool {
			same, err := SkillKey(filepath.Dir(path), existing)
			return err == nil && same == key
		})
		if len(kept) == len(skills) {
			return nil, fmt.Errorf("%s names no %s in run.skills", path, entry)
		}
		return kept, nil
	})
}

// editSkills rewrites run.skills in the file Path names with what edit makes of
// it, through the file's YAML node tree so nothing else in it moves. What it
// writes is decoded the way Load decodes it before it replaces the file, so an
// edit never leaves a file the next create refuses.
func editSkills(edit func(path string, skills []string) ([]string, error)) (string, error) {
	path := Path()
	if path == "" {
		return "", ErrNoFile
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	mode := os.FileMode(0o644)
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode().Perm()
	}
	// The file is checked as Load checks it first: an edit of a file that does
	// not parse would write over whatever the person was in the middle of.
	if len(bytes.TrimSpace(data)) > 0 {
		if err := decode(&Config{}, data, path); err != nil {
			return "", err
		}
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	// A file of comments alone — client.example.yaml copied into place — is
	// no document to the parser, and its comments are in no node. They are
	// kept by writing them back ahead of the settings this edit adds.
	var kept []byte
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
		if len(bytes.TrimSpace(data)) > 0 {
			kept = append(bytes.TrimRight(data, "\n"), '\n')
		}
	}
	root := doc.Content[0]
	if root.Kind == yaml.ScalarNode && root.Tag == "!!null" {
		*root = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", HeadComment: root.HeadComment, FootComment: root.FootComment}
	}
	renameLegacySection(root)
	runSection, err := mappingValue(root, "run", yaml.MappingNode)
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	skillsNode, err := mappingValue(runSection, "skills", yaml.SequenceNode)
	if err != nil {
		return "", fmt.Errorf("%s: run: %w", path, err)
	}
	var skills []string
	for _, item := range skillsNode.Content {
		skills = append(skills, item.Value)
	}
	skills, err = edit(path, skills)
	if err != nil {
		return "", err
	}
	// Entries that stay keep their nodes, comments and all; a new one is
	// written the way the rest of the list is.
	byValue := map[string]*yaml.Node{}
	for _, item := range skillsNode.Content {
		byValue[item.Value] = item
	}
	content := make([]*yaml.Node, 0, len(skills))
	for _, skill := range skills {
		if node, ok := byValue[skill]; ok {
			content = append(content, node)
			delete(byValue, skill)
			continue
		}
		content = append(content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: skill})
	}
	skillsNode.Content = content
	// An empty list is written [], and any other one a line per entry.
	skillsNode.Style = 0
	if len(content) == 0 {
		skillsNode.Style = yaml.FlowStyle
	}
	out := bytes.NewBuffer(kept)
	encoder := yaml.NewEncoder(out)
	encoder.SetIndent(2)
	if err := encoder.Encode(&doc); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	if err := encoder.Close(); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	if err := decode(&Config{}, out.Bytes(), path); err != nil {
		return "", fmt.Errorf("the edited %s would not be read back: %w", path, err)
	}
	if err := writeFileReplacing(path, out.Bytes(), mode); err != nil {
		return "", err
	}
	return path, nil
}

// renameLegacySection renames a top-level new to run, the key alone, so the
// section keeps its place and its comments. A file naming both is left as it
// is, for the decode beside the edit to refuse.
func renameLegacySection(root *yaml.Node) {
	if root.Kind != yaml.MappingNode {
		return
	}
	var legacy *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		switch root.Content[i].Value {
		case "run":
			return
		case legacySection:
			legacy = root.Content[i]
		}
	}
	if legacy != nil {
		legacy.Value = "run"
	}
}

// mappingValue is the value of key in mapping, added at the end as an empty
// node of kind when the mapping has no such key, and replaced by one when it
// holds null (`skills:` with nothing after it).
func mappingValue(mapping *yaml.Node, key string, kind yaml.Kind) (*yaml.Node, error) {
	if mapping.Kind != yaml.MappingNode {
		return nil, errors.New("is not a mapping")
	}
	tag := "!!map"
	if kind == yaml.SequenceNode {
		tag = "!!seq"
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != key {
			continue
		}
		value := mapping.Content[i+1]
		if value.Kind == yaml.ScalarNode && value.Tag == "!!null" {
			*value = yaml.Node{Kind: kind, Tag: tag, LineComment: value.LineComment}
		}
		if value.Kind != kind {
			return nil, fmt.Errorf("%s is not a %s", key, map[yaml.Kind]string{yaml.MappingNode: "mapping", yaml.SequenceNode: "list"}[kind])
		}
		return value, nil
	}
	value := &yaml.Node{Kind: kind, Tag: tag}
	mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
	return value, nil
}

// writeFileReplacing writes data to path through a file beside it and a
// rename, so a reader never sees half of it.
func writeFileReplacing(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

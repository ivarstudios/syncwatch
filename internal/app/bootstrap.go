package app

import (
	"bytes"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// secretKeys are the YAML keys holding secrets, by the top-level section they
// are in (for "servers", in each list item).
var secretKeys = map[string][]string{
	"servers":       {"api_key"},
	"notifications": {"discord_webhook"},
	"api":           {"token"},
}

const scrubbedNote = "imported into syncwatch's encrypted store and removed from this file"

// walkSecrets calls fn for every non-empty secret value in a YAML document.
func walkSecrets(doc *yaml.Node, fn func(v *yaml.Node)) {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return
	}
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		keys := secretKeys[root.Content[i].Value]
		val := root.Content[i+1]
		var maps []*yaml.Node
		switch val.Kind {
		case yaml.MappingNode:
			maps = []*yaml.Node{val}
		case yaml.SequenceNode:
			maps = val.Content
		}
		for _, m := range maps {
			if m.Kind != yaml.MappingNode {
				continue
			}
			for j := 0; j+1 < len(m.Content); j += 2 {
				for _, k := range keys {
					if v := m.Content[j+1]; m.Content[j].Value == k && (v.Kind != yaml.ScalarNode || v.Value != "") {
						fn(v)
					}
				}
			}
		}
	}
}

func parseNode(path string) (*yaml.Node, os.FileInfo, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, nil, err
	}
	return &doc, fi, nil
}

// bootstrapHasSecrets reports whether a bootstrap file still contains secrets.
func bootstrapHasSecrets(path string) bool {
	doc, _, err := parseNode(path)
	if err != nil {
		return false
	}
	found := false
	walkSecrets(doc, func(*yaml.Node) { found = true })
	return found
}

// scrubBootstrap removes the secrets from a bootstrap file once they are in
// the encrypted store, so API keys don't stay on disk in plaintext. The rest
// of the file, comments included, is kept.
func scrubBootstrap(path string) error {
	doc, fi, err := parseNode(path)
	if err != nil {
		return err
	}
	changed := false
	walkSecrets(doc, func(v *yaml.Node) {
		*v = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "", Style: yaml.DoubleQuotedStyle, LineComment: scrubbedNote}
		changed = true
	})
	if !changed {
		return nil
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".syncwatch-bootstrap-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), fi.Mode().Perm()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

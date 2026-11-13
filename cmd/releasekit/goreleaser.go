package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

// The part of .goreleaser.yaml the macOS half of a release reads. goreleaser
// builds and archives linux and windows in CI; the darwin binaries are built
// on the owner's Mac (scripts/release/macos.sh), and releasekit builds them
// from the same build entries (main, binary, env, flags, ldflags,
// mod_timestamp, goarch) and packs them into the same archive (name template
// and files). Every key of those entries it does not implement is refused,
// so a config change that would make the darwin build differ from the others
// fails here instead of shipping.

// grConfig is the subset of .goreleaser.yaml releasekit reads.
type grConfig struct {
	ProjectName string
	Builds      []grBuild
	Archive     grArchive
}

type grBuild struct {
	ID           string   `yaml:"id"`
	Main         string   `yaml:"main"`
	Binary       string   `yaml:"binary"`
	Env          []string `yaml:"env"`
	Goos         []string `yaml:"goos"`
	Goarch       []string `yaml:"goarch"`
	Flags        []string `yaml:"flags"`
	Ldflags      []string `yaml:"ldflags"`
	ModTimestamp string   `yaml:"mod_timestamp"`
}

type grArchive struct {
	Formats         []string           `yaml:"formats"`
	FormatOverrides []grFormatOverride `yaml:"format_overrides"`
	NameTemplate    string             `yaml:"name_template"`
	Files           []grFile           `yaml:"-"`
}

type grFormatOverride struct {
	Goos    string   `yaml:"goos"`
	Formats []string `yaml:"formats"`
}

// grFile is one archives[].files entry: a bare path (or glob), or src with
// an optional dst and strip_parent.
type grFile struct {
	Src         string `yaml:"src"`
	Dst         string `yaml:"dst"`
	StripParent bool   `yaml:"strip_parent"`
}

// darwinArches are the architectures a darwin build may name.
var darwinArches = []string{"amd64", "arm64"}

// loadGoreleaser reads root/.goreleaser.yaml.
func loadGoreleaser(root string) (grConfig, error) {
	raw, err := os.ReadFile(filepath.Join(root, ".goreleaser.yaml"))
	if err != nil {
		return grConfig{}, err
	}
	return parseGoreleaser(raw)
}

func parseGoreleaser(raw []byte) (grConfig, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return grConfig{}, fmt.Errorf(".goreleaser.yaml: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return grConfig{}, errors.New(".goreleaser.yaml: not a mapping")
	}
	top := doc.Content[0]
	var cfg grConfig
	if n := mappingValue(top, "project_name"); n != nil {
		if err := n.Decode(&cfg.ProjectName); err != nil {
			return grConfig{}, fmt.Errorf(".goreleaser.yaml project_name: %w", err)
		}
	}
	if cfg.ProjectName == "" {
		return grConfig{}, errors.New(".goreleaser.yaml: no project_name")
	}

	builds := mappingValue(top, "builds")
	if builds == nil || builds.Kind != yaml.SequenceNode || len(builds.Content) == 0 {
		return grConfig{}, errors.New(".goreleaser.yaml: no builds")
	}
	for i, n := range builds.Content {
		if err := onlyKeys(n, "id", "main", "binary", "env", "goos", "goarch", "flags", "ldflags", "mod_timestamp"); err != nil {
			return grConfig{}, fmt.Errorf(".goreleaser.yaml builds[%d]: %w", i, err)
		}
		var b grBuild
		if err := n.Decode(&b); err != nil {
			return grConfig{}, fmt.Errorf(".goreleaser.yaml builds[%d]: %w", i, err)
		}
		if b.ID == "" || b.Main == "" || b.Binary == "" {
			return grConfig{}, fmt.Errorf(".goreleaser.yaml builds[%d]: id, main and binary are all needed", i)
		}
		if slices.Contains(b.Goos, "darwin") {
			return grConfig{}, fmt.Errorf(".goreleaser.yaml builds[%d] (%s) builds darwin itself; the darwin binaries come from scripts/release/macos.sh", i, b.ID)
		}
		for _, arch := range b.Goarch {
			if !slices.Contains(darwinArches, arch) {
				return grConfig{}, fmt.Errorf(".goreleaser.yaml builds[%d] (%s): goarch %s has no darwin build", i, b.ID, arch)
			}
		}
		cfg.Builds = append(cfg.Builds, b)
	}

	archives := mappingValue(top, "archives")
	if archives == nil || archives.Kind != yaml.SequenceNode || len(archives.Content) != 1 {
		return grConfig{}, errors.New(".goreleaser.yaml: want exactly one archives entry")
	}
	an := archives.Content[0]
	if err := onlyKeys(an, "formats", "format_overrides", "name_template", "files"); err != nil {
		return grConfig{}, fmt.Errorf(".goreleaser.yaml archives[0]: %w", err)
	}
	if err := an.Decode(&cfg.Archive); err != nil {
		return grConfig{}, fmt.Errorf(".goreleaser.yaml archives[0]: %w", err)
	}
	if !slices.Equal(cfg.Archive.Formats, []string{"tar.gz"}) {
		return grConfig{}, fmt.Errorf(".goreleaser.yaml archives[0]: formats %v, want [tar.gz] (what the darwin archives are)", cfg.Archive.Formats)
	}
	for _, o := range cfg.Archive.FormatOverrides {
		if o.Goos == "darwin" {
			return grConfig{}, errors.New(".goreleaser.yaml archives[0]: a darwin format override is not implemented for the darwin archives")
		}
	}
	if cfg.Archive.NameTemplate == "" {
		return grConfig{}, errors.New(".goreleaser.yaml archives[0]: no name_template")
	}
	if files := mappingValue(an, "files"); files != nil {
		for i, f := range files.Content {
			var entry grFile
			switch f.Kind {
			case yaml.ScalarNode:
				entry.Src = f.Value
			case yaml.MappingNode:
				if err := onlyKeys(f, "src", "dst", "strip_parent"); err != nil {
					return grConfig{}, fmt.Errorf(".goreleaser.yaml archives[0].files[%d]: %w", i, err)
				}
				if err := f.Decode(&entry); err != nil {
					return grConfig{}, fmt.Errorf(".goreleaser.yaml archives[0].files[%d]: %w", i, err)
				}
			default:
				return grConfig{}, fmt.Errorf(".goreleaser.yaml archives[0].files[%d]: neither a path nor a src entry", i)
			}
			if entry.Src == "" {
				return grConfig{}, fmt.Errorf(".goreleaser.yaml archives[0].files[%d]: no src", i)
			}
			cfg.Archive.Files = append(cfg.Archive.Files, entry)
		}
	}
	return cfg, nil
}

// mappingValue is the value node of key in mapping m, or nil.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// onlyKeys refuses a mapping holding a key outside allowed: a setting
// releasekit does not implement would make its build differ from
// goreleaser's.
func onlyKeys(m *yaml.Node, allowed ...string) error {
	if m.Kind != yaml.MappingNode {
		return errors.New("not a mapping")
	}
	for i := 0; i < len(m.Content); i += 2 {
		if k := m.Content[i].Value; !slices.Contains(allowed, k) {
			return fmt.Errorf("%q is not implemented for the darwin build (releasekit knows %s)", k, strings.Join(allowed, ", "))
		}
	}
	return nil
}

// render executes a goreleaser template over data. A field data lacks, or
// any function, fails: releasekit implements only the plain fields.
func render(name, text string, data map[string]string) (string, error) {
	t, err := template.New(name).Option("missingkey=error").Parse(text)
	if err != nil {
		return "", fmt.Errorf("template %s: %w", name, err)
	}
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return "", fmt.Errorf("template %s: %w", name, err)
	}
	return b.String(), nil
}

// archiveName is the darwin (or any os's) archive file name for arch, as the
// name template spells it, with the format's extension.
func (c grConfig) archiveName(version, goos, arch string) (string, error) {
	base, err := render("name_template", c.Archive.NameTemplate, map[string]string{
		"ProjectName": c.ProjectName,
		"Version":     version,
		"Os":          goos,
		"Arch":        arch,
	})
	if err != nil {
		return "", err
	}
	return base + ".tar.gz", nil
}

// darwinArchiveNames are the darwin archives of version, one per architecture
// any build names, in darwinArches order.
func (c grConfig) darwinArchiveNames(version string) ([]string, error) {
	var names []string
	for _, arch := range darwinArches {
		if !c.buildsArch(arch) {
			continue
		}
		name, err := c.archiveName(version, "darwin", arch)
		if err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil, errors.New(".goreleaser.yaml: no build names a darwin architecture")
	}
	return names, nil
}

func (c grConfig) buildsArch(arch string) bool {
	for _, b := range c.Builds {
		if slices.Contains(b.Goarch, arch) {
			return true
		}
	}
	return false
}

// approverAsset is the Touch ID approver's release asset for version (the
// name internal/update.ApproverAsset gives a clean version; this one also
// spells a prerelease).
func approverAsset(version string) string {
	return "seamless-approve_" + version + "_darwin.zip"
}

package main

// The configs seamlessd peer-auth setup writes: the daemon's (the owner's
// server config with data_dir, the TLS pair and the peer_auth block
// changed, every other key as it was, the LLM keys and the bearer key
// among them), and the owner's two client configs -- the one every agent
// uses, on the socket, and the console's, which keeps the key.

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// pasDaemonConfig is the daemon's config: raw, the owner's server config,
// with data_dir moved, the TLS pair pointed at the daemon's copies, and the
// peer_auth block turned on for the owner on the socket. Comments and every
// other key are kept as they were.
func pasDaemonConfig(raw []byte, p pasPlan, now time.Time) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p.OldConfig, err)
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s is not a mapping of keys", p.OldConfig)
	}
	root := doc.Content[0]
	pasSetScalar(root, "data_dir", pasData, "")
	if p.TLSCert != "" {
		tls, err := pasMapping(root, "tls")
		if err != nil {
			return nil, err
		}
		pasSetScalar(tls, "cert_file", filepath.Join(pasTLSDir, filepath.Base(p.TLSCert)), "")
		pasSetScalar(tls, "key_file", filepath.Join(pasTLSDir, filepath.Base(p.TLSKey)), "")
	}
	pa, err := pasMapping(root, "peer_auth")
	if err != nil {
		return nil, err
	}
	pasSetScalar(pa, "enabled", "true", "!!bool")
	pasSetScalar(pa, "owner", p.Owner, "")
	sock, err := pasMapping(pa, "socket")
	if err != nil {
		return nil, err
	}
	pasSetScalar(sock, "path", pasSocket, "")
	if pasLookup(sock, "mode") == nil {
		// Who may knock; the kernel still names each caller (the guide's).
		pasSetScalar(sock, "mode", "0666", "!!str")
	}
	head := "Written by seamlessd peer-auth setup on " + now.UTC().Format("2006-01-02") + " from " + p.OldConfig +
		",\nkept beside it as " + filepath.Base(p.OldConfig) + pasKeptSuffix + ": the daemon's config, root:" + p.Account + " 0640."
	if doc.HeadComment != "" {
		head += "\n\n" + doc.HeadComment
	}
	doc.HeadComment = head
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// pasLookup is key's value node in mapping m, nil when absent.
func pasLookup(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// pasSetScalar sets key in mapping m to value (tag "" lets the encoder pick,
// a string unless value reads as something else), keeping the key's comments.
func pasSetScalar(m *yaml.Node, key, value, tag string) {
	style := yaml.Style(0)
	if tag == "!!str" {
		style = yaml.DoubleQuotedStyle
	}
	if v := pasLookup(m, key); v != nil {
		v.Kind, v.Tag, v.Value, v.Style, v.Content, v.Alias = yaml.ScalarNode, tag, value, style, nil, nil
		return
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value, Style: style})
}

// pasMapping is key's mapping in m, made when absent.
func pasMapping(m *yaml.Node, key string) (*yaml.Node, error) {
	v := pasLookup(m, key)
	if v == nil {
		v = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
		return v, nil
	}
	if v.Kind == yaml.ScalarNode && (v.Tag == "!!null" || v.Value == "") {
		v.Kind, v.Tag, v.Value = yaml.MappingNode, "!!map", ""
		return v, nil
	}
	if v.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s in your config is not a block of keys", key)
	}
	return v, nil
}

// pasFileKey reads the bearer key and the extra CA from the config file
// itself: the daemon's config and the console's carry the file's key, and
// a key only the environment holds would reach neither.
func pasFileKey(raw []byte) (key, caFile string, err error) {
	var f struct {
		MCP struct {
			APIKey string `yaml:"api_key"`
		} `yaml:"mcp"`
		TLS struct {
			CAFile string `yaml:"ca_file"`
		} `yaml:"tls"`
	}
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return "", "", err
	}
	key = strings.TrimSpace(f.MCP.APIKey)
	if key == "" || strings.ContainsAny(key, "\r\n") {
		return "", "", errors.New("your config file holds no mcp.api_key (it comes from SEAMLESS_MCP_API_KEY, or is empty): " +
			"put the key in the file first, since the daemon reads only its file")
	}
	return key, strings.TrimSpace(f.TLS.CAFile), nil
}

// pasYAMLString is s as a YAML double-quoted scalar: Go's quoting uses only
// escapes YAML's double-quoted style reads (\", \\, \n, \xXX, \uXXXX, ...).
func pasYAMLString(s string) string { return strconv.Quote(s) }

// pasClientConfig is the owner's config once the daemon runs as its own
// account: a client of the socket, with no key -- the daemon names the
// caller by its uid.
func pasClientConfig() string {
	return "# Written by seamlessd peer-auth setup: the daemon runs as its own account\n" +
		"# (peer_auth), and every agent of yours dials its socket, where the kernel\n" +
		"# names the caller. Your server config is kept beside this file (" + pasKeptSuffix + ").\n" +
		"# No mcp.api_key: the console's own config, console.yaml, keeps the key.\n" +
		"role: client\n" +
		"server_url: " + pasYAMLString("unix://"+pasSocket) + "\n"
}

// pasConsoleConfigContent is the console's own config: the daemon's TCP
// address and its key, for SEAMLESS_CONFIG=<it> seamlessd console-open.
func pasConsoleConfigContent(serverURL, key, caFile string) string {
	s := "# Written by seamlessd peer-auth setup, for the console only: a browser cannot\n" +
		"# dial the socket. Open it with SEAMLESS_CONFIG=<this file> seamlessd console-open.\n" +
		"# The key is readable by every process of yours: seamlessd setup's \"Sign in with\n" +
		"# passkeys and retire the key\" retires it.\n" +
		"role: client\n" +
		"server_url: " + pasYAMLString(serverURL) + "\n"
	if caFile != "" {
		s += "tls:\n  ca_file: " + pasYAMLString(caFile) + "\n"
	}
	return s + "mcp:\n  api_key: " + pasYAMLString(key) + "\n"
}

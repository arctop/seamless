package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// secretField is one config key whose value can be a credential.
type secretField struct {
	// key is the dotted YAML path, as messages and the docs name it.
	key string
	// value reads the key's raw value.
	value func(Config) string
	// url says the value is a URL, which holds a secret only in its userinfo
	// (https://user:password@host): a bare URL is an address, not a credential.
	url bool
}

// secretFields is every config key that can hold a credential. A file that
// sets none of them can be readable by every account on the machine, which is
// what lets one root-owned client config serve several accounts (seamlessd
// doctor's "config permissions" row). secrets_test.go walks Config and fails on
// a key-, token-, secret-, password- or url-named string this list misses.
var secretFields = []secretField{
	{key: "mcp.api_key", value: func(c Config) string { return c.MCP.APIKey }},
	{key: "llm.openai.api_key", value: func(c Config) string { return c.LLM.OpenAI.APIKey }},
	{key: "llm.anthropic.api_key", value: func(c Config) string { return c.LLM.Anthropic.APIKey }},
	{key: "server_url", value: func(c Config) string { return c.AdvertisedURL }, url: true},
	{key: "llm.openai.base_url", value: func(c Config) string { return c.LLM.OpenAI.BaseURL }, url: true},
	{key: "llm.ollama.base_url", value: func(c Config) string { return c.LLM.Ollama.BaseURL }, url: true},
	{key: "llm.anthropic.base_url", value: func(c Config) string { return c.LLM.Anthropic.BaseURL }, url: true},
}

// secretKeys names the keys of c that hold a credential, in secretFields order.
func secretKeys(c Config) []string {
	var keys []string
	for _, f := range secretFields {
		v := strings.TrimSpace(f.value(c))
		if v == "" {
			continue
		}
		if f.url && !urlCarriesCredential(v) {
			continue
		}
		keys = append(keys, f.key)
	}
	return keys
}

// urlCarriesCredential reports whether a URL value carries userinfo. A value
// that does not parse is counted as carrying one when it holds an "@": the
// question is whether the file could leak a secret, and an unreadable URL is
// not evidence that it cannot.
func urlCarriesCredential(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return strings.Contains(raw, "@")
	}
	return u.User != nil
}

// FileSecrets reads the config file at path on its own -- no defaults, no
// SEAMLESS_* environment -- and names the keys it gives a credential: an API
// key, or a URL with userinfo. An empty result says the file itself holds no
// secret, whatever the environment adds when it is loaded.
//
// It decodes the way LoadFrom does (unknown keys and appended documents are
// errors), so a file it cannot judge is an error rather than a clean answer.
func FileSecrets(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config.FileSecrets: read %s: %w", path, err)
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("config.FileSecrets: parse %s: %w", path, err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, fmt.Errorf("config.FileSecrets: parse %s: %w", path, err)
		}
		return nil, fmt.Errorf("config.FileSecrets: parse %s: multiple YAML documents are not allowed", path)
	}
	return secretKeys(c), nil
}

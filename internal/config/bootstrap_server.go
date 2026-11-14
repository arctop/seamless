package config

// A Server's first-run config. `seamlessd setup`'s address step decides where
// the other computers reach this one -- the bind, the URL they dial, the names
// it answers to, its TLS pair -- before the installer installs anything, so a
// first install has no config yet: it is written here, with the bearer key
// EnsureAPIKey would have generated. An existing config file is never edited,
// exactly as EnsureAPIKey and EnsureClientConfig never edit one.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ServerAddress is where a Server is reached: the keys setup's address step
// writes.
type ServerAddress struct {
	// Addr is the bind (addr): an address of the name, or the wildcard.
	Addr string
	// ServerURL is the base URL the other computers dial (server_url).
	ServerURL string
	// AllowedHosts are names the daemon answers to beyond ServerURL's host and
	// its own addresses (allowed_hosts).
	AllowedHosts []string
	// CertFile and KeyFile are the TLS pair (tls.cert_file, tls.key_file).
	CertFile, KeyFile string
}

// Lines renders a as config file lines, each prefixed with indent: addr,
// server_url, allowed_hosts when it names any, and the tls pair -- every value
// a double-quoted scalar. It is what the owner adds to a config that exists.
func (a ServerAddress) Lines(indent string) string {
	var b strings.Builder
	line := func(s string) { b.WriteString(indent + s + "\n") }
	line("addr: " + yamlString(a.Addr))
	line("server_url: " + yamlString(a.ServerURL))
	if len(a.AllowedHosts) > 0 {
		line("allowed_hosts:")
		for _, h := range a.AllowedHosts {
			line("  - " + yamlString(h))
		}
	}
	line("tls:")
	line("  cert_file: " + yamlString(a.CertFile))
	line("  key_file: " + yamlString(a.KeyFile))
	return b.String()
}

// validate refuses an address a Server's setup could not have meant: the
// config's own rules, a TLS pair (setup serves the Server over https only),
// and an https server_url.
func (a ServerAddress) validate() error {
	cfg := Defaults()
	cfg.Addr = a.Addr
	cfg.AdvertisedURL = strings.TrimSuffix(strings.TrimSpace(a.ServerURL), "/")
	cfg.AllowedHosts = a.AllowedHosts
	cfg.TLS.CertFile, cfg.TLS.KeyFile = a.CertFile, a.KeyFile
	if err := cfg.Validate(); err != nil {
		return err
	}
	switch {
	case !cfg.TLSEnabled():
		return errors.New("the address needs its TLS pair: tls.cert_file and tls.key_file")
	case cfg.SocketPath() != "" || !strings.HasPrefix(cfg.AdvertisedURL, "https://"):
		return fmt.Errorf("server_url %q must be an https:// URL", a.ServerURL)
	}
	return nil
}

// HasServerAddress reports whether c already says exactly what a does: it runs
// the daemon here, bound and named as a, with a's TLS pair.
func (c Config) HasServerAddress(a ServerAddress) bool {
	same := func(file, want string) bool {
		w, err := expandHome(want)
		return err == nil && file == w
	}
	hosts := len(c.AllowedHosts)+len(a.AllowedHosts) == 0 || slices.Equal(c.AllowedHosts, a.AllowedHosts)
	return !c.IsClient() && c.SocketPath() == "" && c.Addr == a.Addr &&
		c.ServerURL() == strings.TrimSuffix(strings.TrimSpace(a.ServerURL), "/") &&
		same(c.TLS.CertFile, a.CertFile) && same(c.TLS.KeyFile, a.KeyFile) && hosts
}

// EnsureServerConfig writes the config file that makes THIS machine a Server
// reached at a: a's keys, and a bearer key -- SEAMLESS_MCP_API_KEY's when it
// is set, since that is the key everything installed beside it will present,
// else a generated one, as EnsureAPIKey generates it. It is setup's write on a
// first install, and returns the loaded result plus the path it wrote ("" when
// it wrote nothing).
//
// It refuses as EnsureClientConfig does, for the same reason:
//
//   - A config file anywhere in the search order is NEVER edited. One that
//     already says exactly this is adopted (nothing written, no complaint),
//     and any other is an error that names the file and the lines to add.
//   - The create is O_EXCL 0600, so a process that bootstrapped between the
//     search and the create keeps its file.
//   - A set-but-empty SEAMLESS_MCP_API_KEY is an error, never a config with
//     no key: env wins over file at every later Load.
func EnsureServerConfig(a ServerAddress) (Config, string, error) {
	a.ServerURL = strings.TrimSuffix(strings.TrimSpace(a.ServerURL), "/")
	if err := a.validate(); err != nil {
		return Config{}, "", fmt.Errorf("config.EnsureServerConfig: %w", err)
	}
	key, err := serverAPIKey()
	if err != nil {
		return Config{}, "", fmt.Errorf("config.EnsureServerConfig: %w", err)
	}
	existing, _, err := findConfigFile(true)
	if err != nil {
		return Config{}, "", fmt.Errorf("config.EnsureServerConfig: %w", err)
	}
	if existing != "" {
		loaded, aerr := adoptServerConfig(existing, a)
		return loaded, "", aerr
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, "", fmt.Errorf("config.EnsureServerConfig: %w", err)
	}
	path := filepath.Join(home, ".config", "seamless", "seamless.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Config{}, "", fmt.Errorf("config.EnsureServerConfig: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			loaded, aerr := adoptServerConfig(path, a)
			return loaded, "", aerr
		}
		return Config{}, "", fmt.Errorf("config.EnsureServerConfig: %w", err)
	}
	if _, err := f.WriteString(serverConfigContent(a, key)); err != nil {
		_ = f.Close()
		return Config{}, "", fmt.Errorf("config.EnsureServerConfig: write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return Config{}, "", fmt.Errorf("config.EnsureServerConfig: sync %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return Config{}, "", fmt.Errorf("config.EnsureServerConfig: close %s: %w", path, err)
	}
	loaded, err := LoadFrom(path)
	if err != nil {
		return Config{}, "", fmt.Errorf("config.EnsureServerConfig: %w", err)
	}
	return loaded, path, nil
}

// serverAPIKey is the bearer key a Server's first config carries:
// SEAMLESS_MCP_API_KEY's, else a new one. Set but empty is an error.
func serverAPIKey() (string, error) {
	if env, ok := os.LookupEnv(envMCPAPIKey); ok {
		if env = strings.TrimSpace(env); env == "" {
			return "", fmt.Errorf("%s is set but empty: unset it, and a key is generated", envMCPAPIKey)
		}
		return env, nil
	}
	return generateAPIKey()
}

// adoptServerConfig decides what a config file that is already there means: one
// that already says a is adopted, and anything else is the refusal.
func adoptServerConfig(path string, a ServerAddress) (Config, error) {
	loaded, err := LoadFrom(path)
	if err != nil {
		return Config{}, fmt.Errorf("config.EnsureServerConfig: %w", err)
	}
	if loaded.HasServerAddress(a) {
		return loaded, nil
	}
	return Config{}, fmt.Errorf("config.EnsureServerConfig: %s already exists and is never edited on your behalf; "+
		"put these settings in it yourself, replacing any it has:\n%s", path, strings.TrimSuffix(a.Lines("  "), "\n"))
}

// serverConfigContent is a Server's first config file.
func serverConfigContent(a ServerAddress, key string) string {
	var b strings.Builder
	b.WriteString("# Generated by seamlessd setup on a Server's first install. Every key, its\n" +
		"# default, and its SEAMLESS_* env override: see seamless.yaml.example in the\n" +
		"# repo or /docs/reference/configuration/ on the docs site.\n" +
		"#\n" +
		"# Where the daemon listens: it holds every address server_url's host\n" +
		"# resolves to here, so no other process can answer for that name.\n" +
		"# env: SEAMLESS_ADDR\n" +
		"addr: " + yamlString(a.Addr) + "\n" +
		"# The base URL the other computers dial.\n" +
		"# env: SEAMLESS_SERVER_URL\n" +
		"server_url: " + yamlString(a.ServerURL) + "\n" +
		"# Other names the daemon answers to: server_url's host and its own\n" +
		"# addresses already are, and a request naming any other is refused (421).\n" +
		"# env: SEAMLESS_ALLOWED_HOSTS\n")
	if len(a.AllowedHosts) == 0 {
		b.WriteString("allowed_hosts: []\n")
	} else {
		b.WriteString("allowed_hosts:\n")
		for _, h := range a.AllowedHosts {
			b.WriteString("  - " + yamlString(h) + "\n")
		}
	}
	b.WriteString("tls:\n" +
		"  # The certificate for server_url's host, and its private key.\n" +
		"  # env: SEAMLESS_TLS_CERT_FILE\n" +
		"  cert_file: " + yamlString(a.CertFile) + "\n" +
		"  # env: SEAMLESS_TLS_KEY_FILE\n" +
		"  key_file: " + yamlString(a.KeyFile) + "\n" +
		"mcp:\n" +
		"  # Static bearer key protecting /api/mcp and the console.\n" +
		"  # env: SEAMLESS_MCP_API_KEY\n" +
		"  api_key: " + yamlString(key) + "\n")
	return b.String()
}

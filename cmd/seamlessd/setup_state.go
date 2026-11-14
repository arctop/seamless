package main

// The state file: setup's decision, handed from its first half to the
// installer and to its own --finish. The installers read two of its lines and
// nothing else -- role= (server: run the daemon here; client: dial
// server_url) and server_url= -- and a first line of setupStateHeader, so the
// rest is setup's to extend. It lives in the installer's own temporary
// directory and dies with it; it never holds a key.

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/devices"
)

// setupKindByEnv is a kind by its SEAMLESS_SETUP spelling, setupNone for none.
func setupKindByEnv(v string) (setupKind, bool) {
	if v == "none" {
		return setupNone, true
	}
	for k, w := range setupWords {
		if w.env == v {
			return k, true
		}
	}
	return setupNone, false
}

// setupKindEnv is a kind's SEAMLESS_SETUP spelling, "none" for setupNone.
func setupKindEnv(k setupKind) string {
	if w, ok := setupWords[k]; ok {
		return w.env
	}
	return "none"
}

// marshalSetupState renders st: the header, then one key=value per line.
func marshalSetupState(st setupState) ([]byte, error) {
	for _, v := range []string{st.serverURL, st.change, st.device, st.pairServer, st.fingerprint, st.caFile, st.binarySHA} {
		if strings.ContainsFunc(v, setupControl) {
			return nil, fmt.Errorf("seamlessd.setup: state value %q holds a control character", v)
		}
	}
	var b bytes.Buffer
	b.WriteString(setupStateHeader + "\n")
	fmt.Fprintf(&b, "role=%s\n", st.role)
	if st.serverURL != "" {
		fmt.Fprintf(&b, "server_url=%s\n", st.serverURL)
	}
	fmt.Fprintf(&b, "kind=%s\n", setupKindEnv(st.kind))
	fmt.Fprintf(&b, "installed=%s\n", setupKindEnv(st.installed))
	fmt.Fprintf(&b, "locked=%t\n", st.locked)
	fmt.Fprintf(&b, "answer=%s\n", st.answer)
	if st.change != "" {
		fmt.Fprintf(&b, "change=%s\n", st.change)
	}
	// A pairing Client's (setup_client.go): none of it is a secret.
	if st.pairing {
		b.WriteString("pairing=true\n")
	}
	for _, kv := range [][2]string{{"device", st.device}, {"pair_server", st.pairServer}, {"fingerprint", st.fingerprint},
		{"ca_file", st.caFile}, {"binary_sha256", st.binarySHA}} {
		if kv[1] != "" {
			fmt.Fprintf(&b, "%s=%s\n", kv[0], kv[1])
		}
	}
	return b.Bytes(), nil
}

// writeSetupState writes st to path, 0600.
func writeSetupState(path string, st setupState) error {
	data, err := marshalSetupState(st)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("seamlessd.setup: write the state: %w", err)
	}
	return nil
}

// readSetupState reads what writeSetupState wrote. The file is this release's
// own, written moments before by the same binary, so anything it does not
// know is an error rather than something to skip.
func readSetupState(path string) (setupState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return setupState{}, fmt.Errorf("seamlessd.setup: read the state: %w", err)
	}
	return parseSetupState(data)
}

// parseSetupState is readSetupState over the file's bytes. Every value is
// checked against its format again (setupStateValueProblem), and a key given
// twice is refused: the relay's root step prints what it reads.
func parseSetupState(data []byte) (setupState, error) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	if !sc.Scan() || sc.Text() != setupStateHeader {
		return setupState{}, fmt.Errorf("seamlessd.setup: the state file does not start with %q", setupStateHeader)
	}
	var st setupState
	seen := map[string]bool{}
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			return setupState{}, fmt.Errorf("seamlessd.setup: state line %q is not key=value", sc.Text())
		}
		if seen[key] {
			return setupState{}, fmt.Errorf("seamlessd.setup: state key %q is given twice", key)
		}
		seen[key] = true
		if why := setupStateValueProblem(key, val); why != "" {
			return setupState{}, fmt.Errorf("seamlessd.setup: state %s %q: %s", key, val, why)
		}
		switch key {
		case "role":
			if val != config.RoleServer && val != config.RoleClient {
				return setupState{}, fmt.Errorf("seamlessd.setup: state role %q", val)
			}
			st.role = val
		case "server_url":
			st.serverURL = val
		case "kind", "installed":
			k, known := setupKindByEnv(val)
			if !known {
				return setupState{}, fmt.Errorf("seamlessd.setup: state %s %q", key, val)
			}
			if key == "kind" {
				st.kind = k
			} else {
				st.installed = k
			}
		case "locked":
			b, perr := strconv.ParseBool(val)
			if perr != nil {
				return setupState{}, fmt.Errorf("seamlessd.setup: state locked %q", val)
			}
			st.locked = b
		case "answer":
			switch a := setupAnswer(val); a {
			case setupAsked, setupFromEnv, setupKept, setupDefault:
				st.answer = a
			default:
				return setupState{}, fmt.Errorf("seamlessd.setup: state answer %q", val)
			}
		case "change":
			st.change = val
		case "pairing":
			b, perr := strconv.ParseBool(val)
			if perr != nil {
				return setupState{}, fmt.Errorf("seamlessd.setup: state pairing %q", val)
			}
			st.pairing = b
		case "device":
			if val != setupDeviceRelay && val != setupDeviceKey {
				return setupState{}, fmt.Errorf("seamlessd.setup: state device %q", val)
			}
			st.device = val
		case "pair_server":
			st.pairServer = val
		case "fingerprint":
			st.fingerprint = val
		case "ca_file":
			st.caFile = val
		case "binary_sha256":
			st.binarySHA = val
		default:
			return setupState{}, fmt.Errorf("seamlessd.setup: state key %q", key)
		}
	}
	if err := sc.Err(); err != nil {
		return setupState{}, fmt.Errorf("seamlessd.setup: read the state: %w", err)
	}
	if st.role == "" || st.kind == setupNone {
		return setupState{}, errors.New("seamlessd.setup: the state names no role or kind")
	}
	return st, nil
}

// setupStateCAName is the name of the certificate copy setupCompareCert keeps
// beside the Client config: the one name a state's ca_file may end in.
const setupStateCAName = "server-ca.crt"

// setupStateValueProblem says what is wrong with a state value of key, ""
// when nothing is: each in the format setup writes it in. The keys parsed by
// their own cases (role, kind, installed, locked, answer, pairing, device) are
// judged there.
func setupStateValueProblem(key, val string) string {
	if strings.ContainsFunc(val, setupControl) {
		return "it holds a control character"
	}
	switch key {
	case "server_url":
		if val == "" {
			return ""
		}
		cfg := config.Defaults()
		cfg.Role, cfg.AdvertisedURL = config.RoleClient, val
		if err := cfg.Validate(); err != nil {
			return strings.TrimPrefix(err.Error(), "config: ")
		}
	case "change":
		if val == "" || strings.Trim(val, "abcdefghijklmnopqrstuvwxyz-") != "" {
			return "not a change setup offers"
		}
	case "pair_server":
		return setupServerURLProblem(val)
	case "fingerprint":
		if !devices.ValidCertFingerprint(val) {
			return "not a certificate's SHA-256 fingerprint"
		}
	case "ca_file":
		// Rooted rather than filepath.IsAbs alone: a Windows path setup
		// joined from a rooted one has no drive.
		rooted := filepath.IsAbs(val) || strings.HasPrefix(val, string(filepath.Separator))
		if !rooted || filepath.Clean(val) != val || filepath.Base(val) != setupStateCAName {
			return "not the full path of the " + setupStateCAName + " setup keeps"
		}
	case "binary_sha256":
		if !rootstepSHA256(val) {
			return "not a SHA-256 (64 lower-case hex digits)"
		}
	}
	return ""
}

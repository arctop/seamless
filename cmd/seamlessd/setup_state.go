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
	"strconv"
	"strings"

	"github.com/arctop/seamless/internal/config"
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
	for _, v := range []string{st.serverURL, st.change} {
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

// parseSetupState is readSetupState over the file's bytes.
func parseSetupState(data []byte) (setupState, error) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	if !sc.Scan() || sc.Text() != setupStateHeader {
		return setupState{}, fmt.Errorf("seamlessd.setup: the state file does not start with %q", setupStateHeader)
	}
	var st setupState
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			return setupState{}, fmt.Errorf("seamlessd.setup: state line %q is not key=value", sc.Text())
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

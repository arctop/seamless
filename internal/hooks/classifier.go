package hooks

import (
	"maps"
	pathpkg "path"
	"path/filepath"
	"strings"
	"unicode"
)

// hookDefinitionClass separates operational correctness from ownership. The
// managed marker is authoritative ownership evidence, but a marked definition
// is current only when its functional shape exactly matches the desired one.
type hookDefinitionClass uint8

const (
	hookDefinitionForeign hookDefinitionClass = iota
	hookDefinitionCurrent
	hookDefinitionManagedStale
	hookDefinitionLegacy
)

// classifyHookDefinition is the single ownership and correctness classifier
// used by install, status, and uninstall.
func classifyHookDefinition(client Client, entry, desired any, hs hookSpec, desiredURL string) hookDefinitionClass {
	if sameHookDefinition(entry, desired) {
		return hookDefinitionCurrent
	}
	if isManaged(entry) {
		return hookDefinitionManagedStale
	}
	if isLegacySeamlessDefinition(client, entry, hs, desiredURL) {
		return hookDefinitionLegacy
	}
	return hookDefinitionForeign
}

// sameHookDefinition compares functional definitions while ignoring the
// optional ownership marker. Claude Code may strip that unknown field when it
// rewrites settings.json; the hook remains current when every supported field
// still exactly matches.
func sameHookDefinition(entry, desired any) bool {
	entryMap, entryOK := entry.(map[string]any)
	desiredMap, desiredOK := desired.(map[string]any)
	if !entryOK || !desiredOK {
		return false
	}
	entryCopy := maps.Clone(entryMap)
	desiredCopy := maps.Clone(desiredMap)
	delete(entryCopy, managedMarker)
	delete(desiredCopy, managedMarker)
	return canonicalEqual(entryCopy, desiredCopy)
}

// isLegacySeamlessDefinition recognizes only shapes that Seamless previously
// emitted or explicitly supported for hand-written adoption. A definition must
// contain exactly one handler, and command handlers must run an executable
// named seam or seam.exe with one of the known historical argument layouts.
func isLegacySeamlessDefinition(client Client, entry any, hs hookSpec, desiredURL string) bool {
	entryMap, ok := entry.(map[string]any)
	if !ok {
		return false
	}
	handlers, ok := entryMap["hooks"].([]any)
	if !ok || len(handlers) != 1 {
		return false
	}
	handler, ok := handlers[0].(map[string]any)
	if !ok {
		return false
	}

	// An http handler at the desired URL is the shape Claude Code's
	// UserPromptSubmit took on an http base URL before every hook became a
	// command hook: adopted, so the next install replaces it and its key.
	if handler["type"] == "http" {
		url, ok := handler["url"].(string)
		return ok && sameURL(url, desiredURL)
	}
	if handler["type"] != "command" {
		return false
	}
	return isLegacyHookCommand(client, handler, hs.CLIArg)
}

func isLegacyHookCommand(client Client, handler map[string]any, cliArg string) bool {
	command, ok := handler["command"].(string)
	if !ok {
		return false
	}
	if rawArgs, hasArgs := handler["args"]; hasArgs {
		args, ok := hookStringArgs(rawArgs)
		return ok && isSeamExecutable(command) && knownHookArgs(client, args, cliArg)
	}

	words, ok := splitHookCommand(command)
	if !ok || len(words) == 0 {
		return false
	}
	if strings.HasPrefix(words[0], "SEAMLESS_CONFIG=") {
		configPath := strings.TrimPrefix(words[0], "SEAMLESS_CONFIG=")
		if !isAnchoredHookPath(configPath) {
			return false
		}
		words = words[1:]
	}
	if len(words) == 0 || !isSeamExecutable(words[0]) {
		return false
	}
	return knownHookArgs(client, words[1:], cliArg)
}

func hookStringArgs(value any) ([]string, bool) {
	switch args := value.(type) {
	case []any:
		out := make([]string, len(args))
		for i, arg := range args {
			var ok bool
			out[i], ok = arg.(string)
			if !ok {
				return nil, false
			}
		}
		return out, true
	case []string:
		return args, true
	default:
		return nil, false
	}
}

// knownHookArgs accepts the current argv and the two historical variants:
// command hooks without --config, and Codex hooks missing --client codex. Any
// extra flag, reordered token, or non-anchored config path is not adopted.
func knownHookArgs(client Client, args []string, cliArg string) bool {
	if len(args) < 2 || args[0] != "hook" || args[1] != cliArg {
		return false
	}
	args = args[2:]
	if len(args) >= 2 && args[0] == "--config" {
		if !isAnchoredHookPath(args[1]) {
			return false
		}
		args = args[2:]
	}
	if len(args) == 0 {
		return true
	}
	return client == ClientCodex && len(args) == 2 && args[0] == "--client" && args[1] == string(ClientCodex)
}

func isSeamExecutable(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" || strings.ContainsAny(command, "\x00\r\n") {
		return false
	}
	base := pathpkg.Base(strings.ReplaceAll(command, `\`, "/"))
	return strings.EqualFold(base, "seam") || strings.EqualFold(base, "seam.exe")
}

func isAbsoluteHookPath(path string) bool {
	if filepath.IsAbs(path) {
		return true
	}
	if len(path) >= 3 && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) && path[1] == ':' {
		return path[2] == '\\' || path[2] == '/'
	}
	return strings.HasPrefix(path, `\\`)
}

// isAnchoredHookPath accepts the paths a hand-written hook command may carry:
// absolute, or home-anchored with a leading tilde (which the shell expands to
// an absolute path before seam runs). Adoption-only; the installer's own
// desired paths stay strictly absolute (validateDefinitionPaths).
func isAnchoredHookPath(path string) bool {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		return true
	}
	return isAbsoluteHookPath(path)
}

// splitHookCommand tokenizes the deliberately small shell subset used by old
// Seamless hooks and current Codex command strings. Unquoted shell operators
// are rejected rather than interpreted, keeping legacy adoption conservative.
func splitHookCommand(command string) ([]string, bool) {
	var words []string
	var word strings.Builder
	var quote rune
	haveWord := false
	runes := []rune(strings.TrimSpace(command))
	flush := func() {
		if haveWord {
			words = append(words, word.String())
			word.Reset()
			haveWord = false
		}
	}

	for i := 0; i < len(runes); i++ {
		ch := runes[i]
		switch quote {
		case '\'':
			if ch == '\'' {
				quote = 0
			} else {
				word.WriteRune(ch)
			}
			haveWord = true
			continue
		case '"':
			if ch == '"' {
				quote = 0
				haveWord = true
				continue
			}
			if ch == '\\' && i+1 < len(runes) && runes[i+1] == '"' {
				i++
				ch = runes[i]
			}
			word.WriteRune(ch)
			haveWord = true
			continue
		}

		switch {
		case unicode.IsSpace(ch):
			flush()
		case ch == '\'' || ch == '"':
			quote = ch
			haveWord = true
		case ch == '\\':
			if i+1 >= len(runes) {
				return nil, false
			}
			next := runes[i+1]
			if unicode.IsSpace(next) || next == '\'' || next == '"' {
				i++
				word.WriteRune(next)
			} else {
				word.WriteRune(ch)
			}
			haveWord = true
		// Tilde is deliberately absent: it expands to a path, never to a
		// command, and hand-written hooks routinely say ~/.local/bin/seam.
		case strings.ContainsRune(";&|<>$`(){}*?!#", ch):
			return nil, false
		default:
			word.WriteRune(ch)
			haveWord = true
		}
	}
	if quote != 0 {
		return nil, false
	}
	flush()
	return words, true
}

func classifiedHookIndices(client Client, arr []any, desired any, hs hookSpec, desiredURL string) ([]int, []hookDefinitionClass) {
	var indices []int
	var classes []hookDefinitionClass
	for i, entry := range arr {
		class := classifyHookDefinition(client, entry, desired, hs, desiredURL)
		if class == hookDefinitionForeign {
			continue
		}
		indices = append(indices, i)
		classes = append(classes, class)
	}
	return indices, classes
}

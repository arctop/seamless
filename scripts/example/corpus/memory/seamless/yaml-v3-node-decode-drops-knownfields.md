---
id: 01M7EV6RJZHZBPRBJQ1VQB8T1Y
kind: gotcha
name: yaml-v3-node-decode-drops-knownfields
description: "A custom UnmarshalYAML that calls node.Decode into a struct loses the outer decoder's KnownFields: a mistyped key loads silently. Check keys yourself"
project: seamless
created: 2026-11-15T05:34:43Z
invalid_at: null
superseded_by: null
source_session: 01M7EPQG5EJN3RXB7HFMZKDGBF
model: claude-opus-5-5
tags: [config, yaml, peer-auth]
---
config.Load decodes with `KnownFields(true)`, so an unknown key anywhere in seamless.yaml is refused. That stops inside a custom `UnmarshalYAML(*yaml.Node)`: `node.Decode(&someStruct)` there runs a fresh decoder WITHOUT KnownFields, so a mistyped key decodes with no error and the field stays zero.

This was found 2026-11-13 in the AAR B design (note 01M7C9TQH76XF1D59P3N18EHP9) and handled in cc638e0e (config.RestrictedAccount). For a restricted entry the silent zero fails open: `{account: X, exlude: [...]}` would load as a plain entry, which is the account's full reach.

**How to apply:**
- In a config type with its own UnmarshalYAML, walk the MappingNode's keys against an allow-list and refuse an unknown one with its name and `key.Line`.
- Decode each value separately (scalar into string, sequence into []string), never the whole mapping into a struct.
- Resolve alias nodes first (`resolveAlias` in internal/config).
- Pin it with a test that loads a typo and must fail. TestPeerAuth_RestrictedShapes "a mistyped exclude" does this, and it was shown to fail with the check removed.

Related: [[yaml-v3-refuses-bare-int-into-duration]].

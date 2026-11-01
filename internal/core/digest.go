package core

import (
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"
)

// MemoryDigest fingerprints what a memory tells an agent: its kind, scope,
// name, description and body. An owner confirmation is stamped with it, and
// the confirmation holds only while the memory still digests the same -- so
// any later change to what it says (an agent's edit, an append, a rekind, a
// move to another scope, or a hand edit of the file) lapses the confirmation,
// while a change that tells an agent nothing new (a star, a tag) does not.
//
// Every field is length-prefixed, so no choice of field contents can make two
// different memories digest alike by shifting text across a boundary.
func MemoryDigest(m Memory) string {
	var b strings.Builder
	for _, field := range []string{string(m.Kind), m.Project, m.Name, m.Description, m.Body} {
		b.WriteString(strconv.Itoa(len(field)))
		b.WriteByte(':')
		b.WriteString(field)
		b.WriteByte('|')
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(b.String())))
}

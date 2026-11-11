package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrIncomplete reports an archive whose entries disagree with its manifest: a
// file cut short, or one edited after it was written. Its gzip stream may be
// intact; what it holds is not what it says it holds.
var ErrIncomplete = errors.New("archive does not hold what its manifest describes")

// Verified is what Verify found: the archive's manifest, checked against every
// entry after it, and the digest of the bytes it was handed.
type Verified struct {
	Manifest Manifest
	// SHA256 is the lowercase hex SHA-256 of the whole stream: the archive file
	// as it is on disk.
	SHA256 string
	// Bytes is the archive's size, compressed.
	Bytes int64
	// Unpacked is the size of its entries after the manifest, decompressed:
	// what a restore writes to disk.
	Unpacked    int64
	MemoryFiles int
	NoteFiles   int
	HasDB       bool
}

// Verify reads a whole archive through the guards an import applies, writing
// nothing, and reports what it holds. copyTo, when not nil, receives every byte
// read, so a caller can keep a copy it knows is the verified one -- the copy is
// complete only when Verify returns nil.
//
// It refuses what Import refuses (not an archive, an unsupported format, a
// newer schema, an unsafe entry), an archive whose gzip stream does not check
// out to its end (the CRC is read only there), and one whose entries disagree
// with its manifest (ErrIncomplete).
func Verify(ctx context.Context, r io.Reader, copyTo io.Writer) (Verified, error) {
	h := sha256.New()
	var sink io.Writer = h
	if copyTo != nil {
		sink = io.MultiWriter(h, copyTo)
	}
	counted := &countingReader{r: r}
	tee := io.TeeReader(counted, sink)

	a, err := openArchive(tee)
	if err != nil {
		return Verified{}, err
	}
	defer func() { _ = a.Close() }()

	v := Verified{Manifest: a.manifest}
	for {
		if err := ctx.Err(); err != nil {
			return Verified{}, err
		}
		hdr, err := a.nextEntry()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Verified{}, err
		}
		if hdr.Name != DBName && hdr.Size > maxEntryBytes {
			return Verified{}, fmt.Errorf("archive.Verify: %w: %s exceeds %d bytes", ErrUnsafeEntry, hdr.Name, maxEntryBytes)
		}
		n, err := io.Copy(io.Discard, a.tr)
		if err != nil {
			return Verified{}, fmt.Errorf("archive.Verify: read %s: %w", hdr.Name, err)
		}
		v.Unpacked += n
		switch {
		case hdr.Name == DBName:
			v.HasDB = true
		case strings.HasPrefix(hdr.Name, MemoryTree+"/"):
			v.MemoryFiles++
		case strings.HasPrefix(hdr.Name, NotesTree+"/"):
			v.NoteFiles++
		}
	}
	// The tar ends before the gzip stream does: read the stream to its end, which
	// is where gzip checks the CRC and the length, then whatever the underlying
	// reader still holds, so the digest covers the whole file.
	if _, err := io.Copy(io.Discard, a.gz); err != nil {
		return Verified{}, fmt.Errorf("archive.Verify: %w: the compressed stream is damaged: %w", ErrNotArchive, err)
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return Verified{}, fmt.Errorf("archive.Verify: read to the end: %w", err)
	}

	m := v.Manifest
	switch {
	case m.IncludesDB != v.HasDB:
		return Verified{}, fmt.Errorf("archive.Verify: %w: the manifest says includes_db=%t, the archive %s a database",
			ErrIncomplete, m.IncludesDB, holdsWord(v.HasDB))
	case m.Counts.MemoryFiles != v.MemoryFiles:
		return Verified{}, fmt.Errorf("archive.Verify: %w: %d memory files, the manifest lists %d",
			ErrIncomplete, v.MemoryFiles, m.Counts.MemoryFiles)
	case m.Counts.NoteFiles != v.NoteFiles:
		return Verified{}, fmt.Errorf("archive.Verify: %w: %d note files, the manifest lists %d",
			ErrIncomplete, v.NoteFiles, m.Counts.NoteFiles)
	}
	v.SHA256 = hex.EncodeToString(h.Sum(nil))
	v.Bytes = counted.n
	return v, nil
}

// holdsWord says whether an archive holds a database, for an error.
func holdsWord(has bool) string {
	if has {
		return "holds"
	}
	return "holds no"
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

package mailbox

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// eocd returns the offset of data's end-of-central-directory record.
func eocd(data []byte) int { return bytes.LastIndex(data, []byte("PK\x05\x06")) }

// asZip64 rewrites a plain zip's end record into the zip64 form: a zip64 end
// record and locator before an end record whose fields say "see zip64".
func asZip64(data []byte, records, dirSize, dirOffset uint64) []byte {
	end := eocd(data)
	le := binary.LittleEndian
	rec := make([]byte, 56)
	le.PutUint32(rec, 0x06064b50)
	le.PutUint64(rec[4:], 44)
	le.PutUint64(rec[24:], records)
	le.PutUint64(rec[32:], records)
	le.PutUint64(rec[40:], dirSize)
	le.PutUint64(rec[48:], dirOffset)
	loc := make([]byte, 20)
	le.PutUint32(loc, 0x07064b50)
	le.PutUint64(loc[8:], uint64(end))
	le.PutUint32(loc[16:], 1)
	tail := bytes.Clone(data[end:])
	le.PutUint16(tail[8:], 0xffff)
	le.PutUint16(tail[10:], 0xffff)
	le.PutUint32(tail[12:], 0xffffffff)
	le.PutUint32(tail[16:], 0xffffffff)
	return append(append(append(bytes.Clone(data[:end]), rec...), loc...), tail...)
}

// A central directory over the caps, or one archive/zip would read past, is
// refused before archive/zip parses (and allocates for) any of it.
func TestReadImportZipDirectoryBounds(t *testing.T) {
	msg := []byte("Subject: one\r\n\r\nbody\r\n")
	good := zipOf(t, map[string][]byte{"a.eml": msg, "b.eml": msg, "c.eml": msg}, "a.eml", "b.eml", "c.eml")
	end := eocd(good)
	le := binary.LittleEndian
	dirSize, dirOffset := le.Uint32(good[end+12:]), le.Uint32(good[end+16:])
	patch := func(at int, v uint32, wide bool) []byte {
		out := bytes.Clone(good)
		if wide {
			le.PutUint32(out[end+at:], v)
		} else {
			le.PutUint16(out[end+at:], uint16(v))
		}
		return out
	}
	// A real archive whose directory is over 8 MiB with few entries.
	var big bytes.Buffer
	z := zip.NewWriter(&big)
	for i := range 4500 {
		if _, err := z.Create(fmt.Sprintf("%04d-%s.eml", i, strings.Repeat("n", 2000))); err != nil {
			t.Fatal(err)
		}
	}
	must(t, z.Close())

	reached := false
	defer func(orig func(io.ReaderAt, int64) (*zip.Reader, error)) { newZipReader = orig }(newZipReader)
	newZipReader = func(r io.ReaderAt, size int64) (*zip.Reader, error) { reached = true; return zip.NewReader(r, size) }
	for name, data := range map[string][]byte{
		"huge claimed directory":       patch(12, 0xfffffff0, true),
		"short claimed directory":      patch(12, dirSize-1, true),
		"offset before the directory":  patch(16, dirOffset-1, true),
		"prepended data":               append([]byte("PK\x03\x04 prepended stub"), good...),
		"over 10,000 entries":          patch(10, 10001, false),
		"directory over 8 MiB":         big.Bytes(),
		"zip64 huge directory":         asZip64(good, 3, 1<<40, uint64(dirOffset)),
		"zip64 over 10,000 entries":    asZip64(good, 10001, uint64(dirSize), uint64(dirOffset)),
		"zip64 directory short of end": asZip64(good, 3, uint64(dirSize)-1, uint64(dirOffset)),
		"no end record":                good[:end],
		"truncated end record comment": patch(20, 9, false),
	} {
		reached = false
		if _, _, err := readAll(t, data, 1<<20, 1<<30); !errors.Is(err, ErrImportArchive) || reached {
			t.Fatalf("%s: err=%v, archive/zip reached=%v", name, err, reached)
		}
	}
	for name, data := range map[string][]byte{"plain": good, "zip64": asZip64(good, 3, uint64(dirSize), uint64(dirOffset))} {
		if msgs, _, err := readAll(t, data, 1<<20, 1<<30); err != nil || len(msgs) != 3 || !reached {
			t.Fatalf("%s: %d messages, %v", name, len(msgs), err)
		}
	}
}

// zip64Stretch is the review's construction: the zip64 end record sits at
// the claimed end of a one-header directory, but that header's comment length
// runs over the record into a gap of more headers before the locator, which
// archive/zip then parses. copies chooses how many; adjacent drops the
// stretch and the gap's headers, keeping only a gap after the record.
func zip64Stretch(t *testing.T, copies int, stretch bool) []byte {
	t.Helper()
	msg := []byte("Subject: one\r\n\r\nbody\r\n")
	good := zipOf(t, map[string][]byte{"a.eml": msg}, "a.eml")
	le := binary.LittleEndian
	end := eocd(good)
	dirOffset := int(le.Uint32(good[end+16:]))
	h := bytes.Clone(good[dirOffset:end]) // one header, no comment
	out := bytes.Clone(good[:dirOffset])
	first := bytes.Clone(h)
	if stretch {
		le.PutUint16(first[32:], 56) // the comment covers the zip64 record
	}
	out = append(out, first...)
	p := len(out)
	rec := make([]byte, 56)
	le.PutUint32(rec, 0x06064b50)
	le.PutUint64(rec[4:], 44)
	le.PutUint64(rec[24:], 1)
	le.PutUint64(rec[32:], 1)
	le.PutUint64(rec[40:], uint64(len(h)))
	le.PutUint64(rec[48:], uint64(dirOffset))
	out = append(out, rec...)
	for range copies {
		out = append(out, h...)
	}
	loc := make([]byte, 20)
	le.PutUint32(loc, 0x07064b50)
	le.PutUint64(loc[8:], uint64(p))
	le.PutUint32(loc[16:], 1)
	tail := bytes.Clone(good[end:])
	le.PutUint16(tail[8:], 0xffff)
	le.PutUint16(tail[10:], 0xffff)
	le.PutUint32(tail[12:], 0xffffffff)
	le.PutUint32(tail[16:], 0xffffffff)
	return append(append(out, loc...), tail...)
}

func TestReadImportZipDirectoryWalk(t *testing.T) {
	stretched := zip64Stretch(t, 1<<16, true)
	// Unguarded, archive/zip parses every header in the gap without error.
	if zr, err := zip.NewReader(bytes.NewReader(stretched), int64(len(stretched))); err != nil || len(zr.File) != 1<<16+1 {
		t.Fatalf("construction does not reproduce: %v", err)
	}
	msg := []byte("Subject: one\r\n\r\nbody\r\n")
	good := zipOf(t, map[string][]byte{"a.eml": msg, "b.eml": msg, "c.eml": msg}, "a.eml", "b.eml", "c.eml")
	end := eocd(good)
	le := binary.LittleEndian
	dirSize, dirOffset := le.Uint32(good[end+12:]), le.Uint32(good[end+16:])
	miscount := bytes.Clone(good)
	le.PutUint16(miscount[end+8:], 2)
	le.PutUint16(miscount[end+10:], 2)
	overrun := bytes.Clone(good)
	last := bytes.LastIndex(overrun[:end], []byte("PK\x01\x02"))
	le.PutUint16(overrun[last+32:], 30) // the last header's comment runs past the directory
	badSignature := bytes.Clone(good)
	badSignature[dirOffset+3] = 0x03 // PK\x01\x03: not a central header
	lastShort := bytes.Clone(good)
	le.PutUint32(lastShort[end+12:], dirSize-1) // and the directory claimed one byte short
	le.PutUint32(lastShort[end+16:], dirOffset+1)

	reached := false
	defer func(orig func(io.ReaderAt, int64) (*zip.Reader, error)) { newZipReader = orig }(newZipReader)
	newZipReader = func(r io.ReaderAt, size int64) (*zip.Reader, error) { reached = true; return zip.NewReader(r, size) }
	for name, data := range map[string][]byte{
		"header stretching over the zip64 record": stretched,
		"zip64 record not before its locator":     zip64Stretch(t, 3, false),
		"entry count differs from the headers":    miscount,
		"header overrunning the directory":        overrun,
		"directory shifted by a byte":             lastShort,
		"header signature corrupt":                badSignature,
	} {
		reached = false
		if _, _, err := readAll(t, data, 1<<20, 1<<30); !errors.Is(err, ErrImportArchive) || reached {
			t.Fatalf("%s: err=%v, archive/zip reached=%v", name, err, reached)
		}
	}
	for name, data := range map[string][]byte{"plain": good, "zip64": asZip64(good, 3, uint64(dirSize), uint64(dirOffset)), "zip64 single": zip64Stretch(t, 0, false)} {
		reached = false
		if msgs, _, err := readAll(t, data, 1<<20, 1<<30); err != nil || len(msgs) == 0 || !reached {
			t.Fatalf("%s: %d messages, %v", name, len(msgs), err)
		}
	}
}

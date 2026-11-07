// Package storage implements the crash-safe, single-file storage engine.
//
// On-disk record layout (little-endian):
//
//	[4B MAGIC][4B CRC32][1B TYPE][8B BATCH_ID][8B SEQ][4B KEY_LEN][4B VAL_LEN][KEY][VALUE][4B TRAILER]
//
// The CRC32 covers everything from TYPE through the end of VALUE.
package storage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
)

// Record types.
const (
	OpPut         uint8 = 1
	OpDel         uint8 = 2
	OpBatchCommit uint8 = 3
)

// Size limits and fixed sizes.
const (
	HeaderSize     = 33 // magic(4) + crc(4) + type(1) + batch(8) + seq(8) + klen(4) + vlen(4)
	TrailerSize    = 4
	RecordOverhead = HeaderSize + TrailerSize

	MaxKeySize   = 1024        // 1 KB
	MaxValueSize = 1024 * 1024 // 1 MB
	MaxBatchOps  = 10000
)

var (
	magicHeader  = []byte{0xDE, 0xAD, 0xBE, 0xEF}
	magicTrailer = []byte{0xBE, 0xEF, 0xDE, 0xAD}
)

var (
	// ErrIncomplete means the buffer ends before the record does
	// (a torn write if this is the end of the file).
	ErrIncomplete = errors.New("incomplete record")

	// ErrCorrupt means bytes are present but fail validation
	// (bad magic, bad trailer, bad CRC, or impossible lengths).
	ErrCorrupt = errors.New("corrupt record")
)

// Record is one entry in the write-ahead log.
type Record struct {
	Type    uint8
	BatchID uint64 // 0 for a standalone operation
	Seq     uint64
	Key     []byte
	Value   []byte
}

// Size returns the number of bytes this record occupies on disk.
func (r Record) Size() int {
	return RecordOverhead + len(r.Key) + len(r.Value)
}

// Serialize encodes the record with framing and a CRC32 checksum.
func (r Record) Serialize() []byte {
	klen := len(r.Key)
	vlen := len(r.Value)
	total := HeaderSize + klen + vlen + TrailerSize
	buf := make([]byte, total)

	copy(buf[0:4], magicHeader)
	// buf[4:8] is the CRC, filled in last.
	buf[8] = r.Type
	binary.LittleEndian.PutUint64(buf[9:17], r.BatchID)
	binary.LittleEndian.PutUint64(buf[17:25], r.Seq)
	binary.LittleEndian.PutUint32(buf[25:29], uint32(klen))
	binary.LittleEndian.PutUint32(buf[29:33], uint32(vlen))
	copy(buf[HeaderSize:], r.Key)
	copy(buf[HeaderSize+klen:], r.Value)
	copy(buf[total-TrailerSize:], magicTrailer)

	crc := crc32.ChecksumIEEE(buf[8 : total-TrailerSize])
	binary.LittleEndian.PutUint32(buf[4:8], crc)
	return buf
}

// ParseRecord decodes one record from the start of buf.
// It returns the record and the number of bytes consumed.
//
// Errors:
//   - ErrIncomplete: not enough bytes yet (read more, or it is a torn tail)
//   - ErrCorrupt:    bytes are present but invalid
func ParseRecord(buf []byte) (Record, int, error) {
	if len(buf) < HeaderSize {
		return Record{}, 0, ErrIncomplete
	}
	if !bytes.Equal(buf[0:4], magicHeader) {
		return Record{}, 0, ErrCorrupt
	}

	recType := buf[8]
	if recType < OpPut || recType > OpBatchCommit {
		return Record{}, 0, ErrCorrupt
	}

	klen := binary.LittleEndian.Uint32(buf[25:29])
	vlen := binary.LittleEndian.Uint32(buf[29:33])
	if klen > MaxKeySize || vlen > MaxValueSize {
		// Impossible lengths: do not wait for bytes that will never be valid.
		return Record{}, 0, ErrCorrupt
	}

	total := HeaderSize + int(klen) + int(vlen) + TrailerSize
	if len(buf) < total {
		return Record{}, 0, ErrIncomplete
	}

	if !bytes.Equal(buf[total-TrailerSize:total], magicTrailer) {
		return Record{}, 0, ErrCorrupt
	}

	wantCRC := binary.LittleEndian.Uint32(buf[4:8])
	gotCRC := crc32.ChecksumIEEE(buf[8 : total-TrailerSize])
	if wantCRC != gotCRC {
		return Record{}, 0, ErrCorrupt
	}

	keyStart := HeaderSize
	valStart := keyStart + int(klen)
	rec := Record{
		Type:    recType,
		BatchID: binary.LittleEndian.Uint64(buf[9:17]),
		Seq:     binary.LittleEndian.Uint64(buf[17:25]),
		// Copy so the record stays valid after the caller reuses buf.
		Key:   append([]byte(nil), buf[keyStart:valStart]...),
		Value: append([]byte(nil), buf[valStart:valStart+int(vlen)]...),
	}
	return rec, total, nil
}

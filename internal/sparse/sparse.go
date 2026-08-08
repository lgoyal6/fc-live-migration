// Package sparse streams the data extents of sparse files.
//
// Firecracker writes diff-snapshot memory files sparsely: dirty pages at
// their guest-physical offsets, holes everywhere else. Transferring one is
// therefore a matter of walking its extents with SEEK_DATA/SEEK_HOLE and
// sending only (offset, length, bytes) records — the wire cost of a pre-copy
// round is proportional to the dirty set, not to guest memory size.
//
// Wire format, all integers big-endian:
//
//	header:  magic "FCMG" | u64 file size
//	extent:  u64 offset | u32 length | length bytes   (repeated)
//	trailer: u64 0xFFFFFFFFFFFFFFFF | u32 crc32(IEEE) of all extent payloads
//
// The receiver ftruncates the destination to the advertised size and pwrites
// extents in place, so applying rounds in order reconstructs the memory
// image incrementally.
package sparse

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

var magic = [4]byte{'F', 'C', 'M', 'G'}

const trailerOffset = ^uint64(0)

// maxExtentChunk bounds a single wire record so the receiver can size copy
// buffers; larger extents are split.
const maxExtentChunk = 4 << 20

// Extent is a contiguous data region of a sparse file.
type Extent struct {
	Offset int64
	Length int64
}

// WalkExtents returns the data extents of f in ascending offset order.
func WalkExtents(f *os.File) ([]Extent, error) {
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	var extents []Extent
	var off int64
	for off < size {
		dataStart, err := unix.Seek(int(f.Fd()), off, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			break // only holes remain
		}
		if err != nil {
			return nil, fmt.Errorf("SEEK_DATA at %d: %w", off, err)
		}
		holeStart, err := unix.Seek(int(f.Fd()), dataStart, unix.SEEK_HOLE)
		if err != nil {
			return nil, fmt.Errorf("SEEK_HOLE at %d: %w", dataStart, err)
		}
		extents = append(extents, Extent{Offset: dataStart, Length: holeStart - dataStart})
		off = holeStart
	}
	return extents, nil
}

// TotalBytes sums extent lengths.
func TotalBytes(extents []Extent) int64 {
	var n int64
	for _, e := range extents {
		n += e.Length
	}
	return n
}

// Send streams f's extents to w and returns the number of payload bytes sent.
func Send(w io.Writer, f *os.File) (int64, error) {
	extents, err := WalkExtents(f)
	if err != nil {
		return 0, err
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}

	var hdr [12]byte
	copy(hdr[:4], magic[:])
	binary.BigEndian.PutUint64(hdr[4:], uint64(size))
	if _, err := w.Write(hdr[:]); err != nil {
		return 0, err
	}

	crc := crc32.NewIEEE()
	buf := make([]byte, maxExtentChunk)
	var sent int64
	for _, e := range extents {
		for chunkOff := e.Offset; chunkOff < e.Offset+e.Length; chunkOff += maxExtentChunk {
			n := min(int64(maxExtentChunk), e.Offset+e.Length-chunkOff)
			if _, err := f.ReadAt(buf[:n], chunkOff); err != nil {
				return sent, fmt.Errorf("read extent at %d: %w", chunkOff, err)
			}
			var rec [12]byte
			binary.BigEndian.PutUint64(rec[:8], uint64(chunkOff))
			binary.BigEndian.PutUint32(rec[8:], uint32(n))
			if _, err := w.Write(rec[:]); err != nil {
				return sent, err
			}
			if _, err := w.Write(buf[:n]); err != nil {
				return sent, err
			}
			crc.Write(buf[:n])
			sent += n
		}
	}

	var trailer [12]byte
	binary.BigEndian.PutUint64(trailer[:8], trailerOffset)
	binary.BigEndian.PutUint32(trailer[8:], crc.Sum32())
	if _, err := w.Write(trailer[:]); err != nil {
		return sent, err
	}
	return sent, nil
}

// Receive applies a stream produced by Send onto f, which is truncated to the
// advertised file size. Returns the number of payload bytes written.
func Receive(r io.Reader, f *os.File) (int64, error) {
	var hdr [12]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, fmt.Errorf("read header: %w", err)
	}
	if [4]byte(hdr[:4]) != magic {
		return 0, fmt.Errorf("bad magic %q", hdr[:4])
	}
	size := binary.BigEndian.Uint64(hdr[4:])
	if err := f.Truncate(int64(size)); err != nil {
		return 0, err
	}

	crc := crc32.NewIEEE()
	buf := make([]byte, maxExtentChunk)
	var written int64
	for {
		var rec [12]byte
		if _, err := io.ReadFull(r, rec[:]); err != nil {
			return written, fmt.Errorf("read record header: %w", err)
		}
		off := binary.BigEndian.Uint64(rec[:8])
		length := binary.BigEndian.Uint32(rec[8:])
		if off == trailerOffset {
			if crc.Sum32() != length {
				return written, fmt.Errorf("payload CRC mismatch: got %08x want %08x", crc.Sum32(), length)
			}
			return written, nil
		}
		if length > maxExtentChunk {
			return written, fmt.Errorf("oversized extent record: %d bytes", length)
		}
		if off+uint64(length) > size {
			return written, fmt.Errorf("extent [%d,+%d) beyond file size %d", off, length, size)
		}
		if _, err := io.ReadFull(r, buf[:length]); err != nil {
			return written, fmt.Errorf("read extent payload: %w", err)
		}
		if _, err := f.WriteAt(buf[:length], int64(off)); err != nil {
			return written, fmt.Errorf("write extent at %d: %w", off, err)
		}
		crc.Write(buf[:length])
		written += int64(length)
	}
}

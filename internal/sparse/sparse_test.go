package sparse

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
)

// writeSparse creates a sparse file of totalSize with data at the given
// offsets (each chunk 4 KiB of random bytes).
func writeSparse(t *testing.T, path string, totalSize int64, offsets []int64) []byte {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(totalSize); err != nil {
		t.Fatal(err)
	}
	for _, off := range offsets {
		chunk := make([]byte, 4096)
		if _, err := rand.Read(chunk); err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt(chunk, off); err != nil {
			t.Fatal(err)
		}
	}
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return full
}

func TestSendReceiveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	const size = 1 << 20

	cases := map[string][]int64{
		"empty":          {},
		"single_page":    {8192},
		"adjacent":       {0, 4096, 8192},
		"scattered":      {0, 65536, 524288, size - 4096},
		"ends_at_eof":    {size - 4096},
		"starts_at_zero": {0},
	}
	for name, offsets := range cases {
		t.Run(name, func(t *testing.T) {
			src := filepath.Join(dir, name+".src")
			want := writeSparse(t, src, size, offsets)

			var wire bytes.Buffer
			sf, err := os.Open(src)
			if err != nil {
				t.Fatal(err)
			}
			defer sf.Close()
			sent, err := Send(&wire, sf)
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			if want := int64(len(offsets) * 4096); sent < want {
				// Filesystems may extend extents but never shrink data.
				t.Errorf("sent %d bytes, want at least %d", sent, want)
			}

			dst := filepath.Join(dir, name+".dst")
			df, err := os.Create(dst)
			if err != nil {
				t.Fatal(err)
			}
			defer df.Close()
			if _, err := Receive(&wire, df); err != nil {
				t.Fatalf("Receive: %v", err)
			}

			got, err := os.ReadFile(dst)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("round-trip mismatch (%d vs %d bytes)", len(got), len(want))
			}
		})
	}
}

// TestReceiveIncremental applies two streams to one destination — the way
// pre-copy rounds layer diffs onto the base image — and checks that exactly
// the diff's data extents overwrite the base. The expectation is built from
// the extents the filesystem actually reports (hole granularity differs
// between ext4/tmpfs and APFS), because "only transferred extents change"
// is the protocol contract under test.
func TestReceiveIncremental(t *testing.T) {
	dir := t.TempDir()
	const size = 256 << 10

	base := writeSparse(t, filepath.Join(dir, "base"), size, []int64{0, 4096, 8192})
	diff := writeSparse(t, filepath.Join(dir, "diff"), size, []int64{4096, 131072})

	dst, err := os.Create(filepath.Join(dir, "dst"))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	for _, name := range []string{"base", "diff"} {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		var wire bytes.Buffer
		if _, err := Send(&wire, f); err != nil {
			t.Fatal(err)
		}
		f.Close()
		if _, err := Receive(&wire, dst); err != nil {
			t.Fatal(err)
		}
	}

	want := append([]byte(nil), base...)
	df, err := os.Open(filepath.Join(dir, "diff"))
	if err != nil {
		t.Fatal(err)
	}
	defer df.Close()
	diffExtents, err := WalkExtents(df)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range diffExtents {
		copy(want[e.Offset:e.Offset+e.Length], diff[e.Offset:e.Offset+e.Length])
	}
	got, err := os.ReadFile(filepath.Join(dir, "dst"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("incremental apply mismatch")
	}
}

func TestReceiveRejectsCorruptPayload(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	writeSparse(t, src, 64<<10, []int64{4096})

	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var wire bytes.Buffer
	if _, err := Send(&wire, f); err != nil {
		t.Fatal(err)
	}
	raw := wire.Bytes()
	raw[20] ^= 0xff // flip a payload byte after the first record header

	dst, err := os.Create(filepath.Join(dir, "dst"))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if _, err := Receive(bytes.NewReader(raw), dst); err == nil {
		t.Fatal("corrupted stream accepted; CRC should have failed")
	}
}

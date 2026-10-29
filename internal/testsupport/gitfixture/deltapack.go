package gitfixture

import (
	"bytes"
	"compress/zlib"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"testing"
)

// DeltaPack returns a version-2 git packfile holding one small base blob and
// n OFS_DELTA objects on top of it, each of which copies the base and inserts
// insert bytes of random (incompressible) data. It is the shape that defeats
// a budget counted on inflated objects alone: while go-git streams a pack, a
// delta's raw bytes are buffered and none is inflated until the whole pack has
// been read. The raw pack is a little over n*insert bytes.
func DeltaPack(t testing.TB, n, insert int) []byte {
	t.Helper()
	base := []byte("base blob of the delta pack fixture\n")
	var pack bytes.Buffer
	pack.WriteString("PACK")
	_ = binary.Write(&pack, binary.BigEndian, uint32(2))
	_ = binary.Write(&pack, binary.BigEndian, uint32(n+1))

	baseOffset := int64(pack.Len())
	writePackObject(t, &pack, 3, base) // blob

	for i := 0; i < n; i++ {
		var delta bytes.Buffer
		writeDeltaSize(&delta, len(base))
		writeDeltaSize(&delta, len(base)+insert)
		// Copy the whole base: offset 0 (no offset bytes), one size byte.
		delta.Write([]byte{0x80 | 0x10, byte(len(base))})
		data := make([]byte, insert)
		if _, err := rand.Read(data); err != nil {
			t.Fatalf("gitfixture: random insert: %v", err)
		}
		for len(data) > 0 {
			c := min(len(data), 127)
			delta.WriteByte(byte(c))
			delta.Write(data[:c])
			data = data[c:]
		}
		off := int64(pack.Len())
		writeObjectHeader(&pack, 6, delta.Len()) // OFS_DELTA
		pack.Write(ofsDeltaOffset(off - baseOffset))
		writeZlib(t, &pack, delta.Bytes())
	}
	sum := sha1.Sum(pack.Bytes())
	pack.Write(sum[:])
	return pack.Bytes()
}

// BlobPack returns a version-2 git packfile holding blobs as plain (non-delta)
// blob objects. A large blob of zeros packs to a few KiB raw: the shape whose
// raw size is far below its inflated size.
func BlobPack(t testing.TB, blobs ...[]byte) []byte {
	t.Helper()
	var pack bytes.Buffer
	pack.WriteString("PACK")
	_ = binary.Write(&pack, binary.BigEndian, uint32(2))
	_ = binary.Write(&pack, binary.BigEndian, uint32(len(blobs)))
	for _, b := range blobs {
		writePackObject(t, &pack, 3, b)
	}
	sum := sha1.Sum(pack.Bytes())
	pack.Write(sum[:])
	return pack.Bytes()
}

func writePackObject(t testing.TB, w *bytes.Buffer, typ byte, content []byte) {
	writeObjectHeader(w, typ, len(content))
	writeZlib(t, w, content)
}

// writeObjectHeader writes a pack object header: the type and the low four
// bits of the size in the first byte, then seven bits per byte.
func writeObjectHeader(w *bytes.Buffer, typ byte, size int) {
	b := typ<<4 | byte(size&0x0f)
	size >>= 4
	for size > 0 {
		w.WriteByte(b | 0x80)
		b = byte(size & 0x7f)
		size >>= 7
	}
	w.WriteByte(b)
}

// ofsDeltaOffset encodes an OFS_DELTA's distance back to its base, git's
// offset encoding (each continuation adds one before shifting).
func ofsDeltaOffset(ofs int64) []byte {
	out := []byte{byte(ofs & 0x7f)}
	for ofs >>= 7; ofs > 0; ofs >>= 7 {
		ofs--
		out = append([]byte{0x80 | byte(ofs&0x7f)}, out...)
	}
	return out
}

// writeDeltaSize writes a delta's source or target size, seven bits per byte,
// least significant first.
func writeDeltaSize(w *bytes.Buffer, n int) {
	for {
		b := byte(n & 0x7f)
		n >>= 7
		if n == 0 {
			w.WriteByte(b)
			return
		}
		w.WriteByte(b | 0x80)
	}
}

func writeZlib(t testing.TB, w *bytes.Buffer, data []byte) {
	t.Helper()
	zw := zlib.NewWriter(w)
	if _, err := zw.Write(data); err != nil {
		t.Fatalf("gitfixture: zlib: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gitfixture: zlib: %v", err)
	}
}

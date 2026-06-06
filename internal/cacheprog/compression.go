package cacheprog

import (
	"archive/zip"
	"fmt"
	"io"
	"strings"

	"github.com/klauspost/compress/zstd"
)

type Compression uint16

const (
	CompressionStore   = Compression(zip.Store)
	CompressionDeflate = Compression(zip.Deflate)
	CompressionZstd    = Compression(93)
)

func (c Compression) String() string {
	switch c {
	case CompressionStore:
		return "store"
	case CompressionDeflate:
		return "deflate"
	case CompressionZstd:
		return "zstd"
	default:
		return fmt.Sprintf("method=%d", uint16(c))
	}
}

func ParseCompression(s string) (Compression, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "deflate":
		return CompressionDeflate, nil
	case "store", "none", "uncompressed":
		return CompressionStore, nil
	case "zstd", "zstandard":
		return CompressionZstd, nil
	default:
		return 0, fmt.Errorf("unknown compression %q (want one of: deflate, zstd, store)", s)
	}
}

func init() {
	zip.RegisterCompressor(uint16(CompressionZstd), func(w io.Writer) (io.WriteCloser, error) {
		return zstd.NewWriter(w)
	})
	zip.RegisterDecompressor(uint16(CompressionZstd), func(r io.Reader) io.ReadCloser {
		dec, err := zstd.NewReader(r)
		if err != nil {
			return errReadCloser{err}
		}
		return dec.IOReadCloser()
	})
}

type errReadCloser struct{ err error }

func (e errReadCloser) Read([]byte) (int, error) { return 0, e.err }
func (e errReadCloser) Close() error             { return nil }

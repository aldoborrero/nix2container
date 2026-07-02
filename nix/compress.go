package nix

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/nlewo/nix2container/types"
	godigest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// compressedMediaType maps a compressor name to its OCI layer mediaType.
// The empty string is the "no compression" mode (the existing behavior:
// tar streamed at push time, digest == diff_id). Unknown names error.
func compressedMediaType(compressor string) (string, error) {
	switch compressor {
	case "":
		return v1.MediaTypeImageLayer, nil
	case "gzip":
		return v1.MediaTypeImageLayerGzip, nil
	case "zstd":
		return v1.MediaTypeImageLayerZstd, nil
	default:
		return "", fmt.Errorf("unknown compressor %q (want one of: gzip, zstd)", compressor)
	}
}

// newCompressWriter wraps w with the named compressor, configured for
// byte-deterministic output: fixed level, no embedded timestamp, OS=255
// (unknown). The returned writer must be Close()d to flush the trailer.
//
// Determinism is load-bearing: the compressed digest is what the registry
// HEAD-checks, and the layers.json derivation is input-addressed (not a
// fixed-output derivation), so two build machines that produce different
// bytes for the same layer break blob dedup. Go's compress/flate is
// deterministic for a given Go version (no randomization, no threading);
// the only nondeterminism sources are the gzip header fields zeroed below.
func newCompressWriter(w io.Writer, compressor string) (io.WriteCloser, error) {
	switch compressor {
	case "gzip":
		gz, err := gzip.NewWriterLevel(w, 6)
		if err != nil {
			return nil, err
		}
		// Header is written on the first Write, so set fields first.
		gz.Header.ModTime = time.Time{} // MTIME=0
		gz.Header.Name = ""             // no FNAME
		gz.Header.OS = 255              // "unknown" — matches Python gzip
		return gz, nil
	case "zstd":
		// klauspost/compress/zstd (pure Go). Determinism: the encoder
		// chunks the input across goroutines and concatenates frames,
		// so output bytes depend on the chunk boundaries —
		// WithEncoderConcurrency(1) pins that to one frame regardless
		// of GOMAXPROCS. Level 3 (SpeedDefault) is the
		// container-ecosystem norm (matches `zstd -3`). Parallelism
		// across layers comes from newLayersCompressed's goroutine
		// pool, same as gzip.
		return zstd.NewWriter(w,
			zstd.WithEncoderLevel(zstd.SpeedDefault),
			zstd.WithEncoderConcurrency(1),
		)
	default:
		return nil, fmt.Errorf("unknown compressor %q", compressor)
	}
}

// TarPathsCompress tars the given paths and writes the compressed blob
// to outDir/<compressed-digest>.tar.<ext>. Returns the compressed digest
// (what the registry content-addresses), the uncompressed diff_id (what
// OCI config.rootfs.diff_ids lists), the compressed size, and the blob
// path.
func TarPathsCompress(paths types.Paths, compressor, outDir string) (digest, diffID godigest.Digest, size int64, path string, err error) {
	f, err := os.CreateTemp(outDir, "layer-*")
	if err != nil {
		return "", "", 0, "", err
	}
	defer f.Close() //nolint:errcheck

	// tar → tee(diffHasher, compress → tee(digestHasher+sizeCounter, file))
	digestHasher := godigest.Canonical.Digester()
	counted := &countingWriter{w: io.MultiWriter(f, digestHasher.Hash())}
	cw, err := newCompressWriter(counted, compressor)
	if err != nil {
		return "", "", 0, "", err
	}
	diffHasher := godigest.Canonical.Digester()
	tee := io.MultiWriter(cw, diffHasher.Hash())

	reader := TarPaths(paths)
	if _, err = io.Copy(tee, reader); err != nil {
		_ = reader.Close()
		return "", "", 0, "", err
	}
	if err = reader.Close(); err != nil {
		return "", "", 0, "", err
	}
	if err = cw.Close(); err != nil { // flush gzip trailer BEFORE reading digest/size
		return "", "", 0, "", err
	}

	digest = digestHasher.Digest()
	diffID = diffHasher.Digest()
	size = counted.n
	ext := map[string]string{"gzip": "tar.gz", "zstd": "tar.zst"}[compressor]
	path = fmt.Sprintf("%s/%s.%s", outDir, digest.Encoded(), ext)
	if err = os.Rename(f.Name(), path); err != nil {
		return "", "", 0, "", err
	}
	return digest, diffID, size, path, nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// newLayersCompressed is newLayers with each layer tar+compressed to
// outDir, parallelized over layers (one goroutine per layer, bounded by
// GOMAXPROCS). Layer order in the result matches the input group order.
func newLayersCompressed(groups []types.Paths, compressor, outDir string, history v1.History) ([]types.Layer, error) {
	mediaType, err := compressedMediaType(compressor)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}

	layers := make([]types.Layer, len(groups))
	errs := make([]error, len(groups))
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	var wg sync.WaitGroup
	for i, g := range groups {
		if len(g) == 0 {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, g types.Paths) {
			defer wg.Done()
			defer func() { <-sem }()
			digest, diffID, size, path, err := TarPathsCompress(g, compressor, outDir)
			if err != nil {
				errs[i] = err
				return
			}
			layers[i] = types.Layer{
				Digest:    digest.String(),
				DiffIDs:   diffID.String(),
				Size:      size,
				Paths:     g,
				MediaType: mediaType,
				LayerPath: path,
				History:   history,
			}
		}(i, g)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return nil, e
		}
	}
	// Drop empty slots (groups that were empty after parent-dedup).
	out := layers[:0]
	for _, l := range layers {
		if l.Digest != "" {
			out = append(out, l)
		}
	}
	return out, nil
}

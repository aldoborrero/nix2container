package nix

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/nlewo/nix2container/types"
	godigest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

// compressedMediaType maps a compressor name ("gzip" or "zstd") to its
// OCI layer mediaType. The uncompressed mode never reaches this file:
// NewLayers routes compressor == "" through newLayers instead.
func compressedMediaType(compressor string) (string, error) {
	switch compressor {
	case "gzip":
		return v1.MediaTypeImageLayerGzip, nil
	case "zstd":
		return v1.MediaTypeImageLayerZstd, nil
	default:
		return "", fmt.Errorf("unknown compressor %q (want one of: gzip, zstd)", compressor)
	}
}

// Encoders are pooled and Reset per layer: a zstd encoder allocates
// multi-MB state plus worker goroutines and a gzip writer its window,
// so paying that once per worker instead of once per layer keeps
// memory flat on many-layer images. Reset fully reinitializes encoder
// state, so pooling does not affect output bytes.
var (
	gzipPool = sync.Pool{New: func() any {
		gz, err := gzip.NewWriterLevel(io.Discard, 6)
		if err != nil {
			panic(err) // level 6 is always valid
		}
		return gz
	}}
	zstdPool = sync.Pool{New: func() any {
		// klauspost/compress/zstd (pure Go). Determinism: the encoder
		// chunks the input across goroutines and concatenates frames,
		// so output bytes depend on the chunk boundaries —
		// WithEncoderConcurrency(1) pins that to one frame regardless
		// of GOMAXPROCS. Level 3 (SpeedDefault) is the
		// container-ecosystem norm (matches `zstd -3`). Parallelism
		// across layers comes from newLayersCompressed instead.
		zw, err := zstd.NewWriter(nil,
			zstd.WithEncoderLevel(zstd.SpeedDefault),
			zstd.WithEncoderConcurrency(1),
		)
		if err != nil {
			panic(err) // static options are always valid
		}
		return zw
	}}
)

// acquireCompressWriter returns a pooled writer reset onto w, configured
// for byte-deterministic output (fixed level, no embedded timestamp,
// OS=255), plus a release func that returns it to its pool. The caller
// must Close() the writer (flushing the trailer) before release.
//
// Determinism is load-bearing: the compressed digest is what the registry
// HEAD-checks, and the layers.json derivation is input-addressed (not an
// FOD), so two build machines that produce different bytes for the same
// layer break blob dedup. Go's compress/flate is deterministic for a
// given Go version (no randomization, no threading); the only
// nondeterminism sources are the gzip header fields zeroed below.
func acquireCompressWriter(w io.Writer, compressor string) (io.WriteCloser, func(), error) {
	switch compressor {
	case "gzip":
		gz := gzipPool.Get().(*gzip.Writer)
		gz.Reset(w)
		// Header is written on the first Write, so set fields first.
		// Reset restores NewWriter defaults (which already match), but
		// the digest depends on these bytes — keep them explicit.
		gz.Header.ModTime = time.Time{} // MTIME=0
		gz.Header.Name = ""             // no FNAME
		gz.Header.OS = 255              // "unknown" — matches Python gzip
		return gz, func() { gzipPool.Put(gz) }, nil
	case "zstd":
		zw := zstdPool.Get().(*zstd.Encoder)
		zw.Reset(w)
		return zw, func() { zstdPool.Put(zw) }, nil
	default:
		return nil, nil, fmt.Errorf("unknown compressor %q", compressor)
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
	defer func() {
		f.Close() //nolint:errcheck
		if err != nil {
			os.Remove(f.Name()) //nolint:errcheck
		}
	}()

	// tar → tee(diffHasher, compress → tee(digestHasher+sizeCounter, file))
	digestHasher := godigest.Canonical.Digester()
	counted := &countingWriter{w: io.MultiWriter(f, digestHasher.Hash())}
	cw, release, err := acquireCompressWriter(counted, compressor)
	if err != nil {
		return "", "", 0, "", err
	}
	defer release()
	diffHasher := godigest.Canonical.Digester()
	tee := io.MultiWriter(cw, diffHasher.Hash())

	reader := TarPaths(paths)
	if _, err = io.Copy(tee, reader); err != nil {
		reader.Close() //nolint:errcheck
		cw.Close()     //nolint:errcheck
		return "", "", 0, "", err
	}
	if err = reader.Close(); err != nil {
		cw.Close() //nolint:errcheck
		return "", "", 0, "", err
	}
	if err = cw.Close(); err != nil { // flush the compressor trailer BEFORE reading digest/size
		return "", "", 0, "", err
	}
	// os.CreateTemp uses 0600; the blob is a content-addressed artifact
	// that other users/services may read directly.
	if err = f.Chmod(0o644); err != nil {
		return "", "", 0, "", err
	}

	digest = digestHasher.Digest()
	diffID = diffHasher.Digest()
	size = counted.n
	ext := map[string]string{"gzip": "tar.gz", "zstd": "tar.zst"}[compressor]
	path = filepath.Join(outDir, digest.Encoded()+"."+ext)
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
// outDir, parallelized over layers (bounded by GOMAXPROCS; the first
// error cancels the layers not yet started). Layer order in the result
// matches the input group order. groups must not contain empty groups;
// groupPaths guarantees this.
func newLayersCompressed(groups []types.Paths, compressor, outDir string, history v1.History) ([]types.Layer, error) {
	mediaType, err := compressedMediaType(compressor)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}

	layers := make([]types.Layer, len(groups))
	eg, ctx := errgroup.WithContext(context.Background())
	eg.SetLimit(runtime.GOMAXPROCS(0))
	for i, g := range groups {
		eg.Go(func() error {
			// A sibling already failed (ENOSPC, unreadable path):
			// don't burn CPU compressing layers of a doomed build.
			if err := ctx.Err(); err != nil {
				return err
			}
			digest, diffID, size, path, err := TarPathsCompress(g, compressor, outDir)
			if err != nil {
				return err
			}
			logrus.Infof("Adding %d paths to layer (size:%d digest:%s)", len(g), size, digest.String())
			layers[i] = types.Layer{
				Digest:    digest.String(),
				DiffIDs:   diffID.String(),
				Size:      size,
				Paths:     g,
				MediaType: mediaType,
				LayerPath: path,
				History:   history,
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	return layers, nil
}

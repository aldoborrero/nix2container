package nix

import (
	"io"
	"os"
	"testing"

	"github.com/nlewo/nix2container/types"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// Byte-determinism: same input → same compressed digest, twice. This is
// the property `nix-build --check` verifies for the layers.json
// derivation; testing it here catches a non-deterministic gzip config
// before it ships.
func TestTarPathsCompressDeterministic(t *testing.T) {
	paths := types.Paths{{Path: "../data/layer1"}}
	d1 := t.TempDir()
	d2 := t.TempDir()
	dg1, diff1, sz1, _, err := TarPathsCompress(paths, "gzip", d1)
	if err != nil {
		t.Fatal(err)
	}
	dg2, diff2, sz2, _, err := TarPathsCompress(paths, "gzip", d2)
	if err != nil {
		t.Fatal(err)
	}
	if dg1 != dg2 || sz1 != sz2 {
		t.Fatalf("non-deterministic gzip: %s(%d) vs %s(%d)", dg1, sz1, dg2, sz2)
	}
	if diff1 != diff2 {
		t.Fatalf("non-deterministic tar (diff_id): %s vs %s", diff1, diff2)
	}
	if dg1 == diff1 {
		t.Fatalf("digest should differ from diff_id when compressed (got both %s)", dg1)
	}
	// Gzip header sanity: MTIME=0, OS=255, no FNAME (FLG bit 3 = 0).
	f, err := os.Open(d1 + "/" + dg1.Encoded() + ".tar.gz")
	if err != nil {
		t.Fatalf("open blob: %v", err)
	}
	defer f.Close()
	hdr := make([]byte, 10)
	if _, err := io.ReadFull(f, hdr); err != nil {
		t.Fatalf("read gzip header: %v", err)
	}
	if hdr[0] != 0x1f || hdr[1] != 0x8b {
		t.Fatalf("not gzip: %x", hdr[:2])
	}
	if hdr[3]&0x08 != 0 {
		t.Fatalf("gzip FLG has FNAME set: %d", hdr[3])
	}
	if hdr[4] != 0 || hdr[5] != 0 || hdr[6] != 0 || hdr[7] != 0 {
		t.Fatalf("gzip MTIME=%x (expected zero)", hdr[4:8])
	}
	if hdr[9] != 255 {
		t.Fatalf("gzip OS=%d (expected 255)", hdr[9])
	}
}

func TestNewLayersCompressedParallel(t *testing.T) {
	groups := []types.Paths{
		{{Path: "../data/layer1/file1"}},
		{{Path: "../data/tar-directory"}},
		{{Path: "../data/layer1"}},
	}
	out := t.TempDir()
	layers, err := newLayersCompressed(groups, "gzip", out, v1.History{})
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 3 {
		t.Fatalf("want 3 layers, got %d", len(layers))
	}
	for i, l := range layers {
		if l.MediaType != v1.MediaTypeImageLayerGzip {
			t.Fatalf("layer %d mediaType=%s", i, l.MediaType)
		}
		if l.LayerPath == "" || l.Digest == "" || l.DiffIDs == "" || l.Digest == l.DiffIDs {
			t.Fatalf("layer %d incomplete or uncompressed: %+v", i, l)
		}
		if _, err := os.Stat(l.LayerPath); err != nil {
			t.Fatalf("layer %d blob missing: %v", i, err)
		}
	}
}

// zstd: same determinism properties as gzip, plus verify the
// concurrency pin actually produces stable output (the failure mode
// WithEncoderConcurrency(1) prevents).
func TestTarPathsCompressZstdDeterministic(t *testing.T) {
	paths := types.Paths{{Path: "../data/layer1"}}
	d1 := t.TempDir()
	d2 := t.TempDir()
	dg1, diff1, _, _, err := TarPathsCompress(paths, "zstd", d1)
	if err != nil {
		t.Fatal(err)
	}
	dg2, diff2, _, _, err := TarPathsCompress(paths, "zstd", d2)
	if err != nil {
		t.Fatal(err)
	}
	if dg1 != dg2 {
		t.Fatalf("non-deterministic zstd: %s vs %s", dg1, dg2)
	}
	if diff1 != diff2 || dg1 == diff1 {
		t.Fatalf("diff_id: %s vs %s (digest %s)", diff1, diff2, dg1)
	}
	// zstd magic: 28 b5 2f fd
	f, err := os.Open(d1 + "/" + dg1.Encoded() + ".tar.zst")
	if err != nil {
		t.Fatalf("open blob: %v", err)
	}
	defer f.Close()
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(f, hdr); err != nil {
		t.Fatalf("read zstd magic: %v", err)
	}
	if hdr[0] != 0x28 || hdr[1] != 0xb5 || hdr[2] != 0x2f || hdr[3] != 0xfd {
		t.Fatalf("not zstd: %x", hdr)
	}
}

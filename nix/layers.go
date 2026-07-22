package nix

import (
	_ "crypto/sha256"
	_ "crypto/sha512"
	"reflect"

	"github.com/nlewo/nix2container/types"
	godigest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
)

func getPaths(storePaths []string, parents []types.Layer, rewrites []types.RewritePath, exclude string, permPaths []types.PermPath) types.Paths {
	var paths types.Paths
	for _, p := range storePaths {
		path := types.Path{
			Path: p,
		}
		var pathOptions types.PathOptions
		hasPathOptions := false
		var perms []types.Perm
		for _, perm := range permPaths {
			if p == perm.Path {
				hasPathOptions = true
				perms = append(perms, types.Perm{
					Regex: perm.Regex,
					Mode:  perm.Mode,
					Uid:   perm.Uid,
					Gid:   perm.Gid,
					Uname: perm.Uname,
					Gname: perm.Gname,
				})
			}
		}
		if perms != nil {
			pathOptions.Perms = perms
		}
		for _, rewrite := range rewrites {
			if p == rewrite.Path {
				hasPathOptions = true
				pathOptions.Rewrite = types.Rewrite{
					Regex: rewrite.Regex,
					Repl:  rewrite.Repl,
				}
			}
		}
		if hasPathOptions {
			path.Options = &pathOptions
		}
		if p == exclude {
			logrus.Infof("Excluding path %s from layer", p)
			continue
		}
		if isPathInLayers(parents, path) {
			logrus.Infof("Excluding path %s because already present in a parent layer", p)
			continue
		}
		paths = append(paths, path)
	}
	return paths
}

// If tarDirectory is not an empty string, the tar layer is written to
// the disk. This is useful for layer containing non reproducible
// store paths.
//
// groups is the layer assignment: each inner slice becomes one layer.
// The caller (cmd/layers.go) computes it via closure.LayeredPaths.
func newLayers(groups []types.Paths, tarDirectory string, history v1.History) (layers []types.Layer, err error) {
	for _, layerPaths := range groups {
		if len(layerPaths) == 0 {
			continue
		}
		layerPath := ""
		var digest godigest.Digest
		var size int64
		if tarDirectory == "" {
			digest, size, err = TarPathsSum(layerPaths)
		} else {
			layerPath, digest, size, err = TarPathsWrite(layerPaths, tarDirectory)
		}
		if err != nil {
			return layers, err
		}
		logrus.Infof("Adding %d paths to layer (size:%d digest:%s)", len(layerPaths), size, digest.String())
		layer := types.Layer{
			Digest:    digest.String(),
			DiffIDs:   digest.String(),
			Size:      size,
			Paths:     layerPaths,
			MediaType: v1.MediaTypeImageLayer,
			History:   history,
		}
		if tarDirectory != "" {
			// TODO: we should use v1.MediaTypeImageLayerGzip instead
			layer.MediaType = v1.MediaTypeImageLayer
			layer.LayerPath = layerPath
		}

		layers = append(layers, layer)
	}
	return layers, nil
}

// groupPaths applies the parent/rewrite/exclude/perm filtering to each
// group and drops groups that end up empty (every path already present in
// a parent layer, or all excluded).
func groupPaths(groups [][]string, parents []types.Layer, rewrites []types.RewritePath, exclude string, perms []types.PermPath) []types.Paths {
	out := make([]types.Paths, 0, len(groups))
	for _, g := range groups {
		ps := getPaths(g, parents, rewrites, exclude, perms)
		if len(ps) > 0 {
			out = append(out, ps)
		}
	}
	return out
}

// NewLayers turns layer groups into layers: each inner slice of groups
// becomes one layer (after parent/exclude filtering).
//
// When compressor is non-empty ("gzip" | "zstd"), each layer is
// tar+compressed to blobDir at build time and the returned Layer carries
// {Digest: compressed, DiffIDs: uncompressed, LayerPath: blob file,
// MediaType: tar+<compressor>}. When compressor is "", the existing
// streaming behavior (tar at push time, Digest==DiffIDs, no blob file)
// applies and blobDir is unused.
func NewLayers(groups [][]string, compressor, blobDir string, parents []types.Layer, rewrites []types.RewritePath, exclude string, perms []types.PermPath, history v1.History) ([]types.Layer, error) {
	gp := groupPaths(groups, parents, rewrites, exclude, perms)
	if compressor != "" {
		return newLayersCompressed(gp, compressor, blobDir, history)
	}
	return newLayers(gp, "", history)
}

func NewLayersNonReproducible(groups [][]string, tarDirectory string, parents []types.Layer, rewrites []types.RewritePath, exclude string, perms []types.PermPath, history v1.History) (layers []types.Layer, err error) {
	return newLayers(groupPaths(groups, parents, rewrites, exclude, perms), tarDirectory, history)
}

func isPathInLayers(layers []types.Layer, path types.Path) bool {
	for _, layer := range layers {
		for _, p := range layer.Paths {
			if reflect.DeepEqual(p, path) {
				return true
			}
		}
	}
	return false
}

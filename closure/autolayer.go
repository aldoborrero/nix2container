// Package closure: autolayer.go is a Go port of the size-cohort layering
// algorithm from nixpkgs' pkgs/build-support/docker/auto-layer.py (MIT
// license; original by Lars Jellema, nixpkgs commit ff214e9d0e5e,
// nixpkgs PR #355189). This
// port is offered under nix2container's Apache-2.0 license; the algorithm
// description in the comments below is adapted from the original under
// MIT. See the nixpkgs file for the full rationale.
//
// The existing SortedPathsByPopularity + newLayers pairing puts the
// (maxLayers-1) most-referenced store paths in singleton layers and dumps
// everything else into one catch-all layer. For typical closures the
// catch-all ends up holding the application-specific leaves (the roots of
// the closure graph — the things that change most often) alongside most of
// the bytes, so a one-path change re-uploads the bulk of the image.
//
// This algorithm instead picks the largest paths by narSize as "primaries",
// one per iteration, and assigns every other path to the layer identified
// by the set of primaries that (transitively) depend on it. Paths used by
// exactly the same set of primaries land in the same layer; paths used by
// nothing (closure roots) share one small layer. Iteration stops when
// adding the next primary would exceed maxLayers.
//
// Determinism: the primary order is narSize descending with path as the
// tiebreak; the layer-ID sets are content-addressed; the output layer
// order is closureSize ascending with the sorted layer-ID as tiebreak. The
// same closure graph always produces the same grouping, and adding one
// small leaf path to the closure leaves every primary's layer assignment
// unchanged (the new leaf lands in the roots layer).
package closure

import (
	"sort"
	"strings"
)

// LayeredPaths groups the closure into at most maxLayers layers. Each
// returned slice is one layer's store paths, ordered so that a layer comes
// after every layer it depends on. Paths are deterministically sorted
// within each layer.
func LayeredPaths(storepaths []Storepath, maxLayers int) [][]string {
	if maxLayers < 1 {
		maxLayers = 1
	}

	// Index by path; build forward (references) and reverse (users) adjacency.
	type node struct {
		size  int64
		refs  []string // direct references (deduped, self-loop dropped)
		users []string // direct referrers
	}
	nodes := make(map[string]*node, len(storepaths))
	for _, sp := range storepaths {
		n := &node{size: sp.NarSize}
		seen := map[string]bool{sp.Path: true}
		for _, r := range sp.References {
			if !seen[r] {
				seen[r] = true
				n.refs = append(n.refs, r)
			}
		}
		nodes[sp.Path] = n
	}
	for p, n := range nodes {
		for _, r := range n.refs {
			if rn, ok := nodes[r]; ok {
				rn.users = append(rn.users, p)
			}
		}
	}

	// Transitive closure over an adjacency function.
	walk := func(start string, adj func(string) []string) map[string]bool {
		seen := map[string]bool{start: true}
		stack := []string{start}
		for len(stack) > 0 {
			p := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, q := range adj(p) {
				if !seen[q] {
					seen[q] = true
					stack = append(stack, q)
				}
			}
		}
		return seen
	}

	// Primary candidates, largest first; path as tiebreak for determinism.
	bySize := make([]string, 0, len(nodes))
	for p := range nodes {
		bySize = append(bySize, p)
	}
	sort.Slice(bySize, func(i, j int) bool {
		if nodes[bySize[i]].size != nodes[bySize[j]].size {
			return nodes[bySize[i]].size > nodes[bySize[j]].size
		}
		return bySize[i] < bySize[j]
	})

	// layerOf[path] = set of primaries whose closure contains path (and which
	// are not themselves in another chosen primary's closure).
	layerOf := make(map[string]map[string]bool, len(nodes))
	for p := range nodes {
		layerOf[p] = map[string]bool{}
	}

	layerCount := func(split map[string]map[string]bool) int {
		seen := map[string]bool{}
		for _, s := range split {
			seen[keyOf(s)] = true
		}
		return len(seen)
	}

	for _, primary := range bySize {
		next := make(map[string]map[string]bool, len(layerOf))
		for p, s := range layerOf {
			ns := make(map[string]bool, len(s))
			for k := range s {
				ns[k] = true
			}
			next[p] = ns
		}
		next[primary] = map[string]bool{primary: true}
		deps := walk(primary, func(p string) []string { return nodes[p].refs })
		delete(deps, primary)
		users := walk(primary, func(p string) []string { return nodes[p].users })
		delete(users, primary)
		for d := range deps {
			// Drop any existing primary-user of d that is itself a user of
			// the new primary (the new primary subsumes it for d's cohort).
			for u := range users {
				delete(next[d], u)
			}
			// If none of d's remaining primary-users is itself a dependency
			// of the new primary, the new primary is a direct cohort member.
			covered := false
			for u := range next[d] {
				if deps[u] {
					covered = true
					break
				}
			}
			if !covered {
				next[d][primary] = true
			}
		}
		if layerCount(next) > maxLayers {
			break
		}
		layerOf = next
	}

	// Materialize layers keyed by their primary-set.
	type layer struct {
		id          string
		paths       []string
		closureSize int64
	}
	byID := map[string]*layer{}
	for p, s := range layerOf {
		id := keyOf(s)
		l, ok := byID[id]
		if !ok {
			l = &layer{id: id}
			byID[id] = l
		}
		l.paths = append(l.paths, p)
	}
	for _, l := range byID {
		// closureSize = sum of narSize over the layer's full dependency
		// closure. A layer's closure strictly contains every layer it
		// depends on, so ordering by closureSize is a valid topological
		// order (matching auto-layer.py's "neat" ordering).
		seen := map[string]bool{}
		for _, p := range l.paths {
			for d := range walk(p, func(q string) []string { return nodes[q].refs }) {
				seen[d] = true
			}
		}
		for d := range seen {
			l.closureSize += nodes[d].size
		}
		sort.Strings(l.paths)
	}

	ordered := make([]*layer, 0, len(byID))
	for _, l := range byID {
		ordered = append(ordered, l)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].closureSize != ordered[j].closureSize {
			return ordered[i].closureSize < ordered[j].closureSize
		}
		return ordered[i].id < ordered[j].id
	})

	out := make([][]string, len(ordered))
	for i, l := range ordered {
		out[i] = l.paths
	}
	return out
}

// keyOf canonicalizes a primary-set as a sorted, NUL-joined string so it
// can be used as a map key. Store paths never contain NUL.
func keyOf(s map[string]bool) string {
	ks := make([]string, 0, len(s))
	for k := range s {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, "\x00")
}

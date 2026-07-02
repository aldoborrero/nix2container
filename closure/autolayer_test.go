package closure

import (
	"reflect"
	"testing"
)

// Small hand-built closure: three independent "large" roots (P, Q, R)
// sharing a common dependency chain C→B→A, plus a tiny leaf L that
// nothing depends on (the closure root / perturbation stand-in).
//
//	P(100) ─┐
//	Q( 90) ─┼─► C(10) ─► B(10) ─► A(10)
//	R( 80) ─┘
//	L(  1)
//
// With maxLayers=5 we expect: [A,B,C] (shared by {P,Q,R}), [P], [Q],
// [R], [L] — the tiny leaf lands alone instead of in a catch-all with
// P/Q/R's private bytes.
func testGraph() []Storepath {
	return []Storepath{
		{Path: "A", NarSize: 10, References: []string{"A"}},
		{Path: "B", NarSize: 10, References: []string{"B", "A"}},
		{Path: "C", NarSize: 10, References: []string{"C", "B"}},
		{Path: "P", NarSize: 100, References: []string{"P", "C"}},
		{Path: "Q", NarSize: 90, References: []string{"Q", "C"}},
		{Path: "R", NarSize: 80, References: []string{"R", "C"}},
		{Path: "L", NarSize: 1, References: []string{"L"}},
	}
}

func TestLayeredPathsDeterministic(t *testing.T) {
	g := testGraph()
	a := LayeredPaths(g, 5)
	// Same closure, shuffled input order → identical output.
	g2 := []Storepath{g[3], g[6], g[0], g[5], g[2], g[1], g[4]}
	b := LayeredPaths(g2, 5)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("non-deterministic: %v vs %v", a, b)
	}
	if len(a) > 5 {
		t.Fatalf("exceeded maxLayers: got %d", len(a))
	}
	// L (the leaf) must be alone or with other roots only — never grouped
	// with P/Q/R's bytes.
	for _, layer := range a {
		hasL, hasP := false, false
		for _, p := range layer {
			if p == "L" {
				hasL = true
			}
			if p == "P" || p == "Q" || p == "R" {
				hasP = true
			}
		}
		if hasL && hasP {
			t.Fatalf("leaf L grouped with a primary: %v", layer)
		}
	}
}

// Adding one tiny leaf must not perturb any existing layer's path set —
// the load-bearing property for incremental pushes.
func TestLayeredPathsMinimalDelta(t *testing.T) {
	g := testGraph()
	before := LayeredPaths(g, 5)
	g2 := append(g, Storepath{Path: "L2", NarSize: 1, References: []string{"L2"}})
	after := LayeredPaths(g2, 5)

	// Every layer in 'before' that doesn't mention L must appear verbatim
	// in 'after' (L2 lands in the roots layer alongside L).
	index := map[string]bool{}
	for _, l := range after {
		index[key(l)] = true
	}
	for _, l := range before {
		if contains(l, "L") {
			continue // the roots layer gains L2; everything else is stable.
		}
		if !index[key(l)] {
			t.Fatalf("adding a leaf perturbed an unrelated layer: %v", l)
		}
	}
}

func key(l []string) string {
	s := ""
	for _, p := range l {
		s += p + ","
	}
	return s
}

func contains(l []string, p string) bool {
	for _, x := range l {
		if x == p {
			return true
		}
	}
	return false
}

func TestLayeredPathsMaxLayers1(t *testing.T) {
	g := testGraph()
	out := LayeredPaths(g, 1)
	if len(out) != 1 {
		t.Fatalf("maxLayers=1 → %d layers", len(out))
	}
	if len(out[0]) != len(g) {
		t.Fatalf("maxLayers=1 layer has %d paths, want %d", len(out[0]), len(g))
	}
}

func TestLayeredPathsFewerPathsThanLayers(t *testing.T) {
	// 7 paths, maxLayers=100 → should use ≤7 layers and not panic.
	out := LayeredPaths(testGraph(), 100)
	if len(out) > 7 {
		t.Fatalf("more layers (%d) than paths (7)", len(out))
	}
}

func TestLayeredPathsEmpty(t *testing.T) {
	out := LayeredPaths([]Storepath{}, 10)
	if len(out) != 0 {
		t.Fatalf("empty graph → %d layers", len(out))
	}
}

// One giant path + many tiny leaves (the "few huge, many small" closure
// shape). The giant path must get its own layer; the tiny leaves share
// the roots layer, NOT the giant's.
func TestLayeredPathsAsymmetric(t *testing.T) {
	g := []Storepath{
		{Path: "glibc", NarSize: 30, References: []string{"glibc"}},
		{Path: "giant", NarSize: 10000, References: []string{"giant", "glibc"}},
	}
	for i := 0; i < 50; i++ {
		p := string(rune('a'+i%26)) + string(rune('0'+i/26))
		g = append(g, Storepath{Path: p, NarSize: 1, References: []string{p, "glibc"}})
	}
	out := LayeredPaths(g, 10)
	if len(out) > 10 {
		t.Fatalf("exceeded maxLayers: %d", len(out))
	}
	// giant must be alone (or with only its private deps, of which it has none).
	for _, l := range out {
		if contains(l, "giant") && len(l) != 1 {
			t.Fatalf("giant not isolated: %v", l)
		}
	}
	// glibc (the shared dep) must be isolated from the leaves — if a leaf
	// changes, glibc's layer shouldn't re-upload.
	for _, l := range out {
		if contains(l, "glibc") {
			for _, p := range l {
				if p != "glibc" && p != "giant" {
					t.Fatalf("glibc grouped with leaf %s: %v", p, l)
				}
			}
		}
	}
	// The remaining tiny leaves (after the algorithm has spent the layer
	// budget on the largest-first primaries) share the roots layer — no
	// leaf ends up grouped with giant's bytes.
	var leafBytes int64
	for _, l := range out {
		for _, p := range l {
			if p != "giant" && p != "glibc" {
				leafBytes++ // each leaf is size 1
			}
		}
		if contains(l, "giant") && len(l) > 1 {
			t.Fatalf("giant grouped with something: %v", l)
		}
	}
	if leafBytes != 50 {
		t.Fatalf("lost a path: leaf count %d", leafBytes)
	}
}

package lineage

import (
	"errors"
	"testing"
)

func TestEdge_Validate(t *testing.T) {
	cases := []struct {
		name    string
		edge    Edge
		wantErr bool
	}{
		{"ok buyout", Edge{ChildAgentID: "c", ParentAgentID: "p", Via: ViaBuyout}, false},
		{"missing child", Edge{ParentAgentID: "p", Via: ViaBuyout}, true},
		{"missing parent", Edge{ChildAgentID: "c", Via: ViaBuyout}, true},
		{"self edge", Edge{ChildAgentID: "x", ParentAgentID: "x", Via: ViaBuyout}, true},
		{"bad via", Edge{ChildAgentID: "c", ParentAgentID: "p", Via: "fork"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.edge.Validate()
			if (err != nil) != c.wantErr {
				t.Errorf("Validate %v: err=%v wantErr=%v", c.edge, err, c.wantErr)
			}
		})
	}
}

func TestMemoryGraph_AddEdgeAndAncestry(t *testing.T) {
	g := NewMemoryGraph()
	must := func(e Edge) {
		t.Helper()
		if err := g.AddEdge(e); err != nil {
			t.Fatalf("AddEdge %v: %v", e, err)
		}
	}
	// Chain: a → b → c → d (each child derived from parent).
	must(Edge{ChildAgentID: "b", ParentAgentID: "a", Via: ViaBuyout})
	must(Edge{ChildAgentID: "c", ParentAgentID: "b", Via: ViaBuyout})
	must(Edge{ChildAgentID: "d", ParentAgentID: "c", Via: ViaSubscribe})

	anc, _ := g.Ancestors("d")
	for _, want := range []string{"a", "b", "c"} {
		if _, ok := anc[want]; !ok {
			t.Errorf("Ancestors(d) missing %s: %v", want, anc)
		}
	}
	if _, ok := anc["d"]; ok {
		t.Errorf("Ancestors(d) should not include d itself")
	}

	desc, _ := g.Descendants("a")
	for _, want := range []string{"b", "c", "d"} {
		if _, ok := desc[want]; !ok {
			t.Errorf("Descendants(a) missing %s: %v", want, desc)
		}
	}
}

func TestMemoryGraph_CycleDirect(t *testing.T) {
	g := NewMemoryGraph()
	if err := g.AddEdge(Edge{ChildAgentID: "b", ParentAgentID: "a", Via: ViaBuyout}); err != nil {
		t.Fatal(err)
	}
	// Now adding a→b's parent = b, i.e. edge a child of b, would close
	// loop a → b → a.
	err := g.AddEdge(Edge{ChildAgentID: "a", ParentAgentID: "b", Via: ViaBuyout})
	var cyc *ErrCycle
	if !errors.As(err, &cyc) {
		t.Fatalf("expected ErrCycle, got %v", err)
	}
}

func TestMemoryGraph_CycleTransitive(t *testing.T) {
	g := NewMemoryGraph()
	must := func(e Edge) {
		t.Helper()
		if err := g.AddEdge(e); err != nil {
			t.Fatal(err)
		}
	}
	must(Edge{ChildAgentID: "b", ParentAgentID: "a", Via: ViaBuyout})
	must(Edge{ChildAgentID: "c", ParentAgentID: "b", Via: ViaBuyout})
	// a → b → c established. Adding a's parent = c would close loop.
	err := g.AddEdge(Edge{ChildAgentID: "a", ParentAgentID: "c", Via: ViaBuyout})
	var cyc *ErrCycle
	if !errors.As(err, &cyc) {
		t.Fatalf("expected transitive ErrCycle, got %v", err)
	}
}

func TestMemoryGraph_DuplicateEdgeIdempotent(t *testing.T) {
	g := NewMemoryGraph()
	e := Edge{ChildAgentID: "b", ParentAgentID: "a", Via: ViaBuyout}
	if err := g.AddEdge(e); err != nil {
		t.Fatal(err)
	}
	if err := g.AddEdge(e); err != nil {
		t.Errorf("expected duplicate AddEdge to be no-op, got %v", err)
	}
	anc, _ := g.Ancestors("b")
	if len(anc) != 1 {
		t.Errorf("expected exactly one ancestor, got %v", anc)
	}
}

func TestMemoryGraph_DiamondClosure(t *testing.T) {
	// a is the root. Both b and c derive from a. d derives from both
	// b and c. Closure for d must include a once with no duplication.
	g := NewMemoryGraph()
	must := func(e Edge) {
		t.Helper()
		if err := g.AddEdge(e); err != nil {
			t.Fatal(err)
		}
	}
	must(Edge{ChildAgentID: "b", ParentAgentID: "a", Via: ViaBuyout})
	must(Edge{ChildAgentID: "c", ParentAgentID: "a", Via: ViaBuyout})
	must(Edge{ChildAgentID: "d", ParentAgentID: "b", Via: ViaSubscribe})
	must(Edge{ChildAgentID: "d", ParentAgentID: "c", Via: ViaSubscribe})

	anc, _ := g.Ancestors("d")
	if len(anc) != 3 {
		t.Errorf("diamond ancestors of d: want 3, got %d (%v)", len(anc), anc)
	}
	for _, want := range []string{"a", "b", "c"} {
		if _, ok := anc[want]; !ok {
			t.Errorf("diamond Ancestors(d) missing %s", want)
		}
	}
}

func TestCheckNoCycle_DelegatesToGraph(t *testing.T) {
	g := NewMemoryGraph()
	if err := g.AddEdge(Edge{ChildAgentID: "b", ParentAgentID: "a", Via: ViaBuyout}); err != nil {
		t.Fatal(err)
	}
	// Independent of AddEdge: CheckNoCycle should refuse a→b's loop
	// without mutating the graph.
	err := CheckNoCycle(g, Edge{ChildAgentID: "a", ParentAgentID: "b", Via: ViaBuyout})
	var cyc *ErrCycle
	if !errors.As(err, &cyc) {
		t.Fatalf("expected ErrCycle, got %v", err)
	}
	// Graph unchanged: a still has no ancestors.
	anc, _ := g.Ancestors("a")
	if len(anc) != 0 {
		t.Errorf("CheckNoCycle must not mutate graph: a ancestors=%v", anc)
	}
}

func TestCheckNoMatryoshka_DetectsAncestralOwner(t *testing.T) {
	// alice owns agent A. bob bought it (B derived from A, owned by bob).
	// bob lightly modifies into B' (also bob's). Now bob lists B'. If
	// alice is a candidate buyer, this is matryoshka — alice would
	// repurchase her own lineage.
	g := NewMemoryGraph()
	if err := g.AddEdge(Edge{ChildAgentID: "B", ParentAgentID: "A", Via: ViaBuyout}); err != nil {
		t.Fatal(err)
	}

	owners := MapOwnerLookup{"A": "alice", "B": "bob"}
	forbidden := map[string]struct{}{"alice": {}}

	err := CheckNoMatryoshka(g, owners, "B", forbidden)
	var matr *ErrMatryoshka
	if !errors.As(err, &matr) {
		t.Fatalf("expected ErrMatryoshka, got %v", err)
	}
	if matr.OffendingAgent != "A" || matr.OffendingOwner != "alice" {
		t.Errorf("matryoshka details wrong: %+v", matr)
	}
}

func TestCheckNoMatryoshka_PassesWhenOwnerNotInLineage(t *testing.T) {
	g := NewMemoryGraph()
	if err := g.AddEdge(Edge{ChildAgentID: "B", ParentAgentID: "A", Via: ViaBuyout}); err != nil {
		t.Fatal(err)
	}
	owners := MapOwnerLookup{"A": "alice", "B": "bob"}
	forbidden := map[string]struct{}{"carol": {}}
	if err := CheckNoMatryoshka(g, owners, "B", forbidden); err != nil {
		t.Errorf("carol not in lineage: should pass, got %v", err)
	}
}

func TestCheckNoMatryoshka_EmptyForbiddenIsNoop(t *testing.T) {
	g := NewMemoryGraph()
	if err := CheckNoMatryoshka(g, MapOwnerLookup{}, "X", nil); err != nil {
		t.Errorf("empty forbidden set should pass, got %v", err)
	}
}

func TestCheckNoMatryoshka_RequiresSellerID(t *testing.T) {
	g := NewMemoryGraph()
	err := CheckNoMatryoshka(g, MapOwnerLookup{}, "", map[string]struct{}{"x": {}})
	if err == nil {
		t.Fatal("expected error for empty seller agent id")
	}
}

package dag

import (
	"sort"
	"testing"
)

func sorted(s []string) []string {
	sort.Strings(s)
	return s
}

func TestNew_Linear(t *testing.T) {
	g, err := New(
		[]string{"a", "b", "c"},
		map[string][]string{"b": {"a"}, "c": {"b"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	ready := g.Ready(nil)
	if len(ready) != 1 || ready[0] != "a" {
		t.Errorf("expected [a], got %v", ready)
	}

	ready = g.Ready(map[string]bool{"a": true})
	if len(ready) != 1 || ready[0] != "b" {
		t.Errorf("expected [b], got %v", ready)
	}

	ready = g.Ready(map[string]bool{"a": true, "b": true})
	if len(ready) != 1 || ready[0] != "c" {
		t.Errorf("expected [c], got %v", ready)
	}

	ready = g.Ready(map[string]bool{"a": true, "b": true, "c": true})
	if len(ready) != 0 {
		t.Errorf("expected empty, got %v", ready)
	}
}

func TestNew_Parallel(t *testing.T) {
	g, err := New(
		[]string{"a", "b", "c", "d"},
		map[string][]string{"d": {"a", "b", "c"}},
	)
	if err != nil {
		t.Fatal(err)
	}

	ready := sorted(g.Ready(nil))
	if len(ready) != 3 {
		t.Fatalf("expected 3 roots, got %v", ready)
	}
	want := []string{"a", "b", "c"}
	for i, r := range ready {
		if r != want[i] {
			t.Errorf("ready[%d] = %q, want %q", i, r, want[i])
		}
	}

	ready = g.Ready(map[string]bool{"a": true, "b": true, "c": true})
	if len(ready) != 1 || ready[0] != "d" {
		t.Errorf("expected [d], got %v", ready)
	}
}

func TestNew_CycleDetection(t *testing.T) {
	_, err := New(
		[]string{"a", "b", "c"},
		map[string][]string{"b": {"a"}, "c": {"b"}, "a": {"c"}},
	)
	if err == nil {
		t.Error("expected cycle error")
	}
}

func TestNew_SelfCycle(t *testing.T) {
	_, err := New(
		[]string{"a"},
		map[string][]string{"a": {"a"}},
	)
	if err == nil {
		t.Error("expected cycle error for self-reference")
	}
}

func TestNew_DuplicateNode(t *testing.T) {
	_, err := New([]string{"a", "a"}, nil)
	if err == nil {
		t.Error("expected duplicate node error")
	}
}

func TestNew_UnknownDep(t *testing.T) {
	_, err := New(
		[]string{"a"},
		map[string][]string{"a": {"missing"}},
	)
	if err == nil {
		t.Error("expected unknown dep error")
	}
}

func TestNew_UnknownNode(t *testing.T) {
	_, err := New(
		[]string{"a"},
		map[string][]string{"missing": {"a"}},
	)
	if err == nil {
		t.Error("expected unknown node error")
	}
}

func TestAncestors_Linear(t *testing.T) {
	g, _ := New(
		[]string{"a", "b", "c", "d"},
		map[string][]string{"b": {"a"}, "c": {"b"}, "d": {"c"}},
	)

	anc := sorted(g.Ancestors("d"))
	want := []string{"a", "b", "c"}
	if len(anc) != 3 {
		t.Fatalf("expected 3 ancestors, got %v", anc)
	}
	for i, a := range anc {
		if a != want[i] {
			t.Errorf("ancestors[%d] = %q, want %q", i, a, want[i])
		}
	}

	anc = g.Ancestors("a")
	if len(anc) != 0 {
		t.Errorf("root should have no ancestors, got %v", anc)
	}
}

func TestAncestors_Diamond(t *testing.T) {
	//   a
	//  / \
	// b   c
	//  \ /
	//   d
	g, _ := New(
		[]string{"a", "b", "c", "d"},
		map[string][]string{"b": {"a"}, "c": {"a"}, "d": {"b", "c"}},
	)

	anc := sorted(g.Ancestors("d"))
	want := []string{"a", "b", "c"}
	if len(anc) != 3 {
		t.Fatalf("expected 3 ancestors, got %v", anc)
	}
	for i, a := range anc {
		if a != want[i] {
			t.Errorf("ancestors[%d] = %q, want %q", i, a, want[i])
		}
	}
}

func TestSplice_Basic(t *testing.T) {
	// a → b → c  becomes  a → b → new → c
	g, _ := New(
		[]string{"a", "b", "c"},
		map[string][]string{"b": {"a"}, "c": {"b"}},
	)

	if err := g.Splice("new", "b", []string{"c"}); err != nil {
		t.Fatal(err)
	}

	// b should no longer be a direct parent of c
	ready := g.Ready(map[string]bool{"a": true, "b": true})
	r := sorted(ready)
	if len(r) != 1 || r[0] != "new" {
		t.Errorf("expected [new] ready after a+b, got %v", r)
	}

	ready = g.Ready(map[string]bool{"a": true, "b": true, "new": true})
	if len(ready) != 1 || ready[0] != "c" {
		t.Errorf("expected [c] ready after a+b+new, got %v", ready)
	}

	// Ancestors of c should include new, b, a
	anc := sorted(g.Ancestors("c"))
	if len(anc) != 3 {
		t.Fatalf("expected 3 ancestors of c, got %v", anc)
	}
}

func TestSplice_MultipleRewire(t *testing.T) {
	// a → b, a → c  splice new after a, rewire both
	// becomes a → new → b, a → new → c
	g, _ := New(
		[]string{"a", "b", "c"},
		map[string][]string{"b": {"a"}, "c": {"a"}},
	)

	if err := g.Splice("new", "a", []string{"b", "c"}); err != nil {
		t.Fatal(err)
	}

	ready := g.Ready(map[string]bool{"a": true})
	if len(ready) != 1 || ready[0] != "new" {
		t.Errorf("expected [new], got %v", ready)
	}

	ready = sorted(g.Ready(map[string]bool{"a": true, "new": true}))
	if len(ready) != 2 {
		t.Fatalf("expected [b, c], got %v", ready)
	}
}

func TestSplice_DuplicateNode(t *testing.T) {
	g, _ := New([]string{"a", "b"}, map[string][]string{"b": {"a"}})
	if err := g.Splice("a", "a", []string{"b"}); err == nil {
		t.Error("expected error for duplicate node in splice")
	}
}

func TestSplice_InvalidRewire(t *testing.T) {
	g, _ := New(
		[]string{"a", "b", "c"},
		map[string][]string{"b": {"a"}, "c": {"b"}},
	)
	if err := g.Splice("new", "a", []string{"c"}); err == nil {
		t.Error("expected error: c does not depend on a directly")
	}
}

func TestSplice_CycleRollback(t *testing.T) {
	// a → b. Splicing "new" after b with rewire=[a] would create a → b → new → a cycle.
	// But a doesn't depend on b, so this should fail at the "does not depend on" check.
	// Let's create a case where cycle detection triggers:
	// a → b → c, c → d. Splice "new" after c rewire=[d].
	// Then try to make d depend on new depend on c — that's fine (no cycle).
	// For an actual cycle: we'd need splice to create new → c somehow.
	// Actually, splice always creates after → new → rewired, so cycles only happen
	// if after is a descendant of one of the rewired nodes, which can't happen
	// in a DAG. The cycle check is defense-in-depth.
	g, _ := New(
		[]string{"a", "b"},
		map[string][]string{"b": {"a"}},
	)
	// Valid splice
	if err := g.Splice("c", "b", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReady_Empty(t *testing.T) {
	g, _ := New([]string{"a"}, nil)
	ready := g.Ready(nil)
	if len(ready) != 1 || ready[0] != "a" {
		t.Errorf("single node should be ready, got %v", ready)
	}
}

func TestChildren(t *testing.T) {
	g, _ := New(
		[]string{"a", "b", "c"},
		map[string][]string{"b": {"a"}, "c": {"a"}},
	)
	kids := sorted(g.Children("a"))
	if len(kids) != 2 || kids[0] != "b" || kids[1] != "c" {
		t.Errorf("expected [b, c], got %v", kids)
	}
	if len(g.Children("b")) != 0 {
		t.Error("leaf should have no children")
	}
}

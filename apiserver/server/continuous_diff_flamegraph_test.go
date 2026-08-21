// ============================================================
// server/continuous_diff_flamegraph_test.go — 差分火焰图树对齐算法单测
// ============================================================
// 只测 diffContinuousTreeNode/truncateDiffTree 这两个纯函数，不依赖数据库/
// 对象存储——continuousTreeNode 树完全在内存里手工搭建。
// ============================================================

package server

import "testing"

func buildTreeNode(name string, value, self float64, children ...*continuousTreeNode) *continuousTreeNode {
	node := &continuousTreeNode{Name: name, Value: value, Self: self, Children: map[string]*continuousTreeNode{}}
	for _, c := range children {
		node.Children[c.Name] = c
		node.Order = append(node.Order, c)
	}
	return node
}

func TestDiffContinuousTreeNodeAlignsMatchingFrames(t *testing.T) {
	base := buildTreeNode("root", 100, 0,
		buildTreeNode("main", 100, 0,
			buildTreeNode("foo", 60, 60),
			buildTreeNode("bar", 40, 40),
		),
	)
	compare := buildTreeNode("root", 120, 0,
		buildTreeNode("main", 120, 0,
			buildTreeNode("foo", 90, 90),
			buildTreeNode("bar", 30, 30),
		),
	)

	diff := diffContinuousTreeNode("root", base, compare)
	if diff.BaseValue != 100 || diff.CompareValue != 120 || diff.Delta != 20 {
		t.Fatalf("root diff mismatch: %+v", diff)
	}
	if len(diff.Children) != 1 || diff.Children[0].Name != "main" {
		t.Fatalf("expected single main child, got %+v", diff.Children)
	}
	mainChildren := diff.Children[0].Children
	if len(mainChildren) != 2 {
		t.Fatalf("expected 2 children under main, got %d", len(mainChildren))
	}
	// foo 权重(max(60,90)=90)比 bar(max(40,30)=40)大，应该排在前面
	if mainChildren[0].Name != "foo" {
		t.Fatalf("expected foo sorted first by weight, got %s", mainChildren[0].Name)
	}
	foo := mainChildren[0]
	if foo.BaseValue != 60 || foo.CompareValue != 90 || foo.Delta != 30 {
		t.Fatalf("foo diff mismatch: %+v", foo)
	}
	bar := mainChildren[1]
	if bar.BaseValue != 40 || bar.CompareValue != 30 || bar.Delta != -10 {
		t.Fatalf("bar diff mismatch: %+v", bar)
	}
}

func TestDiffContinuousTreeNodeHandlesOneSidedFrames(t *testing.T) {
	base := buildTreeNode("root", 50, 0,
		buildTreeNode("onlyInBase", 50, 50),
	)
	compare := buildTreeNode("root", 30, 0,
		buildTreeNode("onlyInCompare", 30, 30),
	)

	diff := diffContinuousTreeNode("root", base, compare)
	if len(diff.Children) != 2 {
		t.Fatalf("expected 2 children (union of both sides), got %d", len(diff.Children))
	}
	byName := map[string]ProfileDiffNode{}
	for _, c := range diff.Children {
		byName[c.Name] = c
	}
	onlyBase, ok := byName["onlyInBase"]
	if !ok {
		t.Fatalf("onlyInBase missing from diff children")
	}
	if onlyBase.BaseValue != 50 || onlyBase.CompareValue != 0 || onlyBase.Delta != -50 {
		t.Fatalf("onlyInBase diff mismatch (function disappeared): %+v", onlyBase)
	}
	onlyCompare, ok := byName["onlyInCompare"]
	if !ok {
		t.Fatalf("onlyInCompare missing from diff children")
	}
	if onlyCompare.BaseValue != 0 || onlyCompare.CompareValue != 30 || onlyCompare.Delta != 30 {
		t.Fatalf("onlyInCompare diff mismatch (function is new): %+v", onlyCompare)
	}
	if onlyCompare.DeltaPercent != 100 {
		t.Fatalf("brand-new function should be flagged via DeltaPercent=100, got %v", onlyCompare.DeltaPercent)
	}
}

func TestDiffContinuousTreeNodeHandlesNilRoots(t *testing.T) {
	compare := buildTreeNode("root", 10, 0, buildTreeNode("leaf", 10, 10))

	diff := diffContinuousTreeNode("root", nil, compare)
	if diff.BaseValue != 0 || diff.CompareValue != 10 {
		t.Fatalf("nil base root should diff as all-zero base: %+v", diff)
	}
	if len(diff.Children) != 1 || diff.Children[0].BaseValue != 0 {
		t.Fatalf("children under nil base should also have BaseValue=0: %+v", diff.Children)
	}
}

func TestTruncateDiffTreeKeepsHeaviestBranchesWithinBudget(t *testing.T) {
	root := ProfileDiffNode{
		Name: "root",
		Children: []ProfileDiffNode{
			{Name: "heavy", BaseValue: 100, CompareValue: 100, Children: []ProfileDiffNode{
				{Name: "heavy.child", BaseValue: 100, CompareValue: 100},
			}},
			{Name: "light", BaseValue: 1, CompareValue: 1},
		},
	}
	// countDiffNodes(root) = heavy + heavy.child + light = 3
	truncated, wasTruncated := truncateDiffTree(root, 1)
	if !wasTruncated {
		t.Fatalf("expected truncation to be reported")
	}
	if len(truncated.Children) != 1 {
		t.Fatalf("expected exactly 1 top-level child kept within budget, got %d", len(truncated.Children))
	}
	if truncated.Children[0].Name != "heavy" {
		t.Fatalf("expected the heaviest branch to be kept, got %s", truncated.Children[0].Name)
	}
	if len(truncated.Children[0].Children) != 0 {
		t.Fatalf("budget of 1 should not leave room for heavy's own child, got %+v", truncated.Children[0].Children)
	}
}

func TestTruncateDiffTreeNoOpWhenUnderBudget(t *testing.T) {
	root := ProfileDiffNode{
		Name: "root",
		Children: []ProfileDiffNode{
			{Name: "a", BaseValue: 1, CompareValue: 1},
			{Name: "b", BaseValue: 1, CompareValue: 1},
		},
	}
	truncated, wasTruncated := truncateDiffTree(root, continuousDefaultMaxNodes)
	if wasTruncated {
		t.Fatalf("should not report truncation when node count is under budget")
	}
	if len(truncated.Children) != 2 {
		t.Fatalf("expected both children preserved, got %d", len(truncated.Children))
	}
}

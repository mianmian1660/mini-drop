// ============================================================
// server/task_diff_flamegraph.go — 周期任务/按需任务的差分火焰图
// ============================================================
// docs/periodic-diff-flamegraph-design.md 的实现：GetTaskDiff 的
// format=flamegraph 分支。数据源是两个任务各自的 {tid}/folded.txt
// （折叠栈文本），和持续采集从数据库聚合建树不同，但建好树之后的
// 差分算法（diffContinuousTreeNode/truncateDiffTree，continuous.go）
// 原样复用，不重新实现。
// ============================================================

package server

import (
	"bufio"
	"context"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/mini-drop/apiserver/model"
)

// fetchLegacyFoldedStacks 从对象存储读取旧任务的 {tid}/folded.txt。
// 新分代任务由 fetchFoldedStacksForTask 按 active analysis job 读取。
func (s *APIServer) fetchLegacyFoldedStacks(tid string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	bucket := s.Config.Storage.Bucket
	key := tid + "/folded.txt"

	reader, err := s.Storage.GetObject(ctx, bucket, key)
	if err != nil {
		return "", false
	}
	defer reader.Close()

	var sb strings.Builder
	if _, err := io.Copy(&sb, reader); err != nil {
		return "", false
	}
	return sb.String(), true
}

// fetchFoldedStacksForTask 优先读取任务当前 active analysis generation 的
// folded.txt。阶段 4 起分析产物使用 tasks/{tid}/analysis/.../gN/ 路径，不能
// 再把 {tid}/folded.txt 当作唯一来源。只有没有分代产物的历史任务才回退旧 key。
func (s *APIServer) fetchFoldedStacksForTask(task *model.HotmethodTask) (string, bool) {
	if task == nil {
		return "", false
	}
	job, _ := s.resolveSelectedAnalysisJob(task, "")
	if job != nil {
		artifacts := s.jobArtifactsByLogicalName(task.TID, job.ID, "folded.txt")
		if len(artifacts) > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for i := range artifacts {
				body, err := s.readArtifactLogicalContent(ctx, artifacts[i], 64<<20)
				if err == nil && len(body) > 0 {
					return string(body), true
				}
			}
			return "", false
		}
		// active generation 已有其它分代产物时，folded 的缺失是真缺失，不能
		// 偷读旧代同名 key，否则两侧会悄悄比较错误的 generation。
		if s.jobHasAnyArtifacts(task.TID, job.ID) {
			return "", false
		}
	}
	return s.fetchLegacyFoldedStacks(task.TID)
}

// foldedTextToTreeNode 把折叠栈文本（"funcA;funcB;funcC 123" 一行一条）解析成
// continuousTreeNode 调用树。插入逻辑照抄 continuousAddSample
// （continuous.go:3771-3792）：Value 沿路径累加（inclusive），叶子帧额外
// 累加 Self，保证和持续采集那棵树的字段语义完全一致，diff 算法才能直接复用。
func foldedTextToTreeNode(text string) *continuousTreeNode {
	root := &continuousTreeNode{Name: "root", Children: map[string]*continuousTreeNode{}}
	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		idx := strings.LastIndex(line, " ")
		if idx < 0 {
			continue
		}
		count, err := strconv.ParseFloat(line[idx+1:], 64)
		if err != nil || count <= 0 {
			continue
		}
		stack := strings.Split(line[:idx], ";")

		node := root
		node.Value += count
		for i, frame := range stack {
			frame = strings.TrimSpace(frame)
			if frame == "" {
				frame = "unknown"
			}
			if node.Children == nil {
				node.Children = map[string]*continuousTreeNode{}
			}
			child := node.Children[frame]
			if child == nil {
				child = &continuousTreeNode{Name: frame, Children: map[string]*continuousTreeNode{}}
				node.Children[frame] = child
				node.Order = append(node.Order, child)
			}
			child.Value += count
			if i == len(stack)-1 {
				child.Self += count
			}
			node = child
		}
	}
	return root
}

// normalizeTreeToPercent 把树上每个节点的 Value/Self 从原始样本数换算成该树
// 自身总样本数的百分比，就地修改。两个对比任务的采样时长/频率经常不一致
// （比如一个 10s 一个 30s 窗口），原始样本数直接比较会被窗口本身的时长
// 差异污染——同样的 CPU 行为，30s 窗口的样本数天然是 10s 窗口的 3 倍。
// 归一化后 diffContinuousTreeNode 比的才是"占比有没有变"，不是
// "样本数量有没有变"（见 docs/periodic-diff-flamegraph-design.md §5）。
func normalizeTreeToPercent(node *continuousTreeNode, total float64) {
	if node == nil || total <= 0 {
		return
	}
	node.Value = node.Value / total * 100
	node.Self = node.Self / total * 100
	for _, child := range node.Order {
		normalizeTreeToPercent(child, total)
	}
}

// buildTaskDiffFlamegraph 是 GetTaskDiff 的 format=flamegraph 分支实现。
// reason 非空时表示不能生成（缺产物/任务类型不支持），调用方应该把它当作
// 409 返回给前端，引导退回表格 diff 视图。
func (s *APIServer) buildTaskDiffFlamegraph(baselineTask, compareTask *model.HotmethodTask, maxNodes int) (ProfileDiffFlamegraph, string) {
	reasonFor := func(t *model.HotmethodTask) string {
		if t.AnalysisStatus == 3 {
			return "分析失败，无法生成差分火焰图"
		}
		if t.AnalysisStatus < 2 {
			return "分析尚未完成，无法生成差分火焰图"
		}
		if t.ProfilerType == ProfilerBPF {
			return "eBPF 直方图任务产出的是延迟分布而非调用栈，无法做火焰图对比"
		}
		return "没有可对比的调用栈产物（folded.txt）"
	}

	baseText, baseOK := s.fetchFoldedStacksForTask(baselineTask)
	if !baseOK {
		return ProfileDiffFlamegraph{}, "基线任务（" + baselineTask.TID + "）" + reasonFor(baselineTask)
	}
	compareText, compareOK := s.fetchFoldedStacksForTask(compareTask)
	if !compareOK {
		return ProfileDiffFlamegraph{}, "对比任务（" + compareTask.TID + "）" + reasonFor(compareTask)
	}

	baseRoot := foldedTextToTreeNode(baseText)
	compareRoot := foldedTextToTreeNode(compareText)
	baseTotal, compareTotal := baseRoot.Value, compareRoot.Value
	if baseTotal <= 0 || compareTotal <= 0 {
		return ProfileDiffFlamegraph{}, "两侧调用栈样本数为空，无法生成差分火焰图"
	}
	normalizeTreeToPercent(baseRoot, baseTotal)
	normalizeTreeToPercent(compareRoot, compareTotal)

	diffRoot := diffContinuousTreeNode("root", baseRoot, compareRoot)
	if maxNodes <= 0 {
		maxNodes = continuousDefaultMaxNodes
	}
	truncatedRoot, truncated := truncateDiffTree(diffRoot, maxNodes)

	return ProfileDiffFlamegraph{
		Root:         truncatedRoot,
		BaseTotal:    baseTotal,
		CompareTotal: compareTotal,
		// 归一化后单位是"占该次采集总样本数的百分比"，不是持续采集那边的
		// 原始 samples/us 计数，用独立单位名避免前端把两者当成同一口径。
		Unit:        "percent",
		Empty:       len(truncatedRoot.Children) == 0,
		Source:      "mini-drop-task-diff",
		Truncated:   truncated,
		GeneratedAt: time.Now(),
	}, ""
}

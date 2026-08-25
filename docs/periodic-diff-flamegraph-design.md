# 周期性采样时间基线对比 → 差分火焰图 设计

## 1. 现状

`ScheduleTimeline.js` 的"设为基线/与基线对比"走 `tasks.diff` → 后端 [GetTaskDiff](../apiserver/server/task.go:2458)，
只对比两个任务 `top.json` 里的扁平 TopN 函数列表（[fetchTopFunctions](../apiserver/server/task.go:1303)），
产出的是红绿表格（[InlineDiffPanel.js](../web_frontend/src/components/InlineDiffPanel.js)），不是树，看不出调用关系变化。

持续采集那边已经有真正的差分火焰图：[queryNativeContinuousDiffFlamegraph](../apiserver/server/continuous.go:1852)
两次拉时间窗口聚合成 `continuousTreeNode` 调用树，喂给 [diffContinuousTreeNode](../apiserver/server/continuous.go:3961)
（递归对齐两棵树，孩子按 `max(base,compare)` 排序）+ [truncateDiffTree](../apiserver/server/continuous.go:4044)
（DFS 预算截断），前端 `InteractiveFlamegraph.js` 的 `diffMode` 渲染。

## 2. 能不能复用——数据源是关键差异

持续采集的树是从数据库里按时间窗口聚合样本现建的；周期任务的每个窗口是一次独立的 `HotmethodTask`，
产物是 COS/MinIO 上的 `<tid>_folded.txt`（折叠栈文本，`funcA;funcB;funcC count` 一行一条），
前端 `InteractiveFlamegraph.js` 的 `foldedTextToFlamegraph` 已经在客户端把它解析成树用于单任务火焰图展示
（[TaskResultPage.js:409](../web_frontend/src/pages/TaskResultPage.js:409)）。

**结论：差分算法（`diffContinuousTreeNode`/`truncateDiffTree`）可以原样复用，不用改一行；要新写的只是"建树"这一步**——
从两份 `folded.txt` 各自建出一棵 `continuousTreeNode`，而不是从数据库聚合建树。建树逻辑本身也不用发明，
照抄 `continuousAddSample` 插入一条 `stack` 的写法（[continuous.go:3771-3792](../apiserver/server/continuous.go:3771)）：
沿 `Value`（inclusive，含子树）累加、叶子累加 `Self`，逐帧建 `Children` map。

## 3. 接口设计

对齐持续采集的做法——`GetProfileDiff` 用 `?format=flamegraph` 参数分流（不是独立端点），
`GetTaskDiff` 照做同样的扩展：

```
GET /api/v1/tasks/diff?baseline_tid=&compare_tid=&threshold=&format=flamegraph
```

- 不传 `format` 或 `format=table`：现有行为不变，走 `fetchTopFunctions` 表格对比
- `format=flamegraph`：
  1. 分别下载两个 tid 的 `<tid>_folded.txt`（复用现有的 COS 读取工具函数，`fetchTopFunctions` 旁边应该有同款按 key 读对象的封装可以抄）
  2. 解析成两棵 `continuousTreeNode`（新函数 `foldedTextToTreeNode`，逻辑抄 `continuousAddSample`）
  3. `diffContinuousTreeNode("root", baseRoot, compareRoot)` → `truncateDiffTree(diffRoot, maxNodes)`
  4. 复用已有的 `ProfileDiffNode`/`ProfileDiffFlamegraph` 响应结构（[profile.go](../apiserver/server/profile.go) 里已经为持续采集定义过，直接用）

**不是所有 task_kind 都能做**：eBPF 直方图类任务（调度延迟/IO延迟）产出的是延迟分布不是调用栈，没有 `folded.txt`——
`GetTaskDiff` 现有代码已经在处理这个情况（[task.go:2509-2511](../apiserver/server/task.go:2509)"eBPF 直方图任务产出的是延迟分布而非函数列表，无法做热点对比"），
`format=flamegraph` 分支要复用同一条判断逻辑，不能让用户对着两个直方图任务点"火焰图对比"却拿到空树。

## 4. 前端改动

`InlineDiffPanel.js` 加"表格 / 火焰图"两个 tab（和持续采集 `ContinuousProfilingPanel.js` diff 区块的 tab 做法一致），
火焰图 tab 直接复用已经写好的 `InteractiveFlamegraph.js` 的 `diffMode` prop + `diffFlamegraphColor`（红热蓝冷）+
`formatDiffFlamegraphLabel`——这几个都是持续采集那次已经做好、跑过单测和生产构建的，不用重写。

## 5. 风险与代价

- **性能**：持续采集的建树是数据库聚合查询；周期任务这边要从对象存储下载+解析文本文件，两个任务各一次，
  比数据库查询慢，且 `folded.txt` 文件大小取决于采样时长/频率，没做过大小上限评估，需要补一个类似
  `truncateDiffTree` maxNodes 之外的"文件本身太大直接拒绝/降级"的兜底。
- **归一化**：两个任务的采样时长/频率如果不同（比如一个 10s 一个 30s），`Value`/`Self` 的绝对值没有可比性，
  差分只能看相对占比不能看绝对数——这一点持续采集那边因为窗口时长固定所以没暴露这个问题，周期任务这边
  必须在 UI 上显式提示，或者在后端按总样本数归一化后再比较，需要你确认要不要做归一化。
- **改动量**：比"给表格加个开关"大，但比"从零做差分火焰图"小很多——核心算法零改动，只是换了个建树的输入源。

## 6. 建议的实现顺序

1. 后端：`foldedTextToTreeNode` 建树函数 + `GetTaskDiff` 的 `format=flamegraph` 分支，直接接 `diffContinuousTreeNode`/`truncateDiffTree`
2. 前端：`InlineDiffPanel.js` 加 tab + 接入 `InteractiveFlamegraph.js` 的 `diffMode`
3. 验证：至少两个真实周期任务窗口（同一目标、不同时间）跑一次端到端对比，人工确认树对齐结果合理

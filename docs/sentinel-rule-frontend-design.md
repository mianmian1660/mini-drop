# 哨兵规则前端交互设计：创建入口 · 详情卡片 · 时间轴高光

## 0. 背景

后端已落地检测→触发深度诊断的 MVP 判异循环（[apiserver/server/detection.go](../apiserver/server/detection.go)），
但目前只能直接写库创建规则、没有任何前端入口。上一版 [docs/detection-trigger-pipeline-design.md](./detection-trigger-pipeline-design.md)
给的是"新建一个独立的哨兵规则管理页面"（列表 + 判异事件审计）的方案。

这次是你提出的另一条交互路线：**不新起一个独立管理页面，而是把哨兵规则挂进持续采集自己的创建流程和详情页里**，
并且用时间轴高光的方式呈现"什么时候异常过、点开看诊断结果"。这条路线的核心判断是对的：
一条哨兵规则的意义完全依附于它监控的那个持续采集会话——`sentinel_rules.target_ip + signal` 本质上就是在描述
"这个持续采集会话该在什么时候报警"，脱离会话单独管理规则反而增加了认知负担。本文档把你的五点交互想法逐条落成
可执行设计，并指出各自要改的现有文件。

## 1. 交互总览

```
新建持续采集弹窗                持续采集详情页                    深度诊断任务详情页
CreateContinuousSessionModal ──▶ ContinuousSessionDetailPage ──▶ /task/result?tid=xxx
      │                                │  │
      │ 勾选"创建后台哨兵"               │  └─ 时间轴高光 → hover 显示标签 → 点击跳转
      ▼                                ▼
  session + 可选 sentinel_rule     "当前哨兵"卡片（可删除）
```

## 2. 创建持续采集时新增"哨兵规则"栏

**位置**：[CreateContinuousSessionModal.js](../web_frontend/src/components/CreateContinuousSessionModal.js)，
挂在"采集信号"分区（第 162-177 行）之后、"高级设置" `<details>`（第 179 行）之前，作为新的一个 `<div style={S.section}>`。

**为什么放这里而不是独立表单**：这一步用户已经选好了 `scope`/`selectedExe`/`selectedSignals`，哨兵规则的
`target_ip` 和"能监控哪些信号"直接从这几个已选字段派生，不需要用户重新选一遍目标和信号，只需要追加"选哪个信号、
阈值多少"。

**字段设计**（复用 `S.label`/`S.input`/`S.subtle` 现有样式 token，不新造视觉语言）：

```
□ 创建后台哨兵，超过阈值自动触发一次深度诊断          ← 复选框，默认不勾选
  （勾选后展开）
  监控信号：      [下拉，仅列出已勾选的 selectedSignals 中支持判异的信号]
  告警阈值（p99）： [数字输入 + 单位 ms]
  冷却期：        [数字输入，默认 15] 分钟
  提示：告警触发的是一次 60 秒的短时深度诊断，不会常驻运行；同一规则冷却期内不会重复触发。
```

- 监控信号下拉只展示 `detectionSignalTaskKind`（[detection.go:33](../apiserver/server/detection.go)）支持的三个信号交集
  `selectedSignals`——如果用户只勾了 `db_snapshot`，这一栏应该整体隐藏并提示"当前信号暂不支持自动告警"，
  而不是展示一个选了也无效的选项。
- 复选框不勾选时，`submit()`（第 103-126 行）走原逻辑，不受影响；勾选时，`continuous.createSession` 成功拿到
  `session.sid` 后，紧接着追加一次 `POST /sentinel-rules`（新增 API，见 §5），把 `target_ip` 直接取
  `session.target_ip`、`signal`/`metric`/`floor_value`/`cooldown_seconds` 取表单值。
- 这一步失败（比如规则创建失败但持续采集已经建成）不应该回滚已创建的 session，只在 `onSuccess` 之后追加一条
  警告 toast："持续采集已创建，但哨兵规则创建失败：{msg}，可在详情页重试"——两个资源没有事务性绑定，容忍
  部分失败比强行做分布式事务划算。

## 3. 详情页新增"当前哨兵"卡片

**位置**：[ContinuousSessionDetailPage.js](../web_frontend/src/pages/ContinuousSessionDetailPage.js)，
新增一个 `<section style={S.card}>`，插在现有的会话元信息卡片（第 108-125 行）和 `<ContinuousProfilingPanel>`
（第 126 行）之间。

**数据来源**：新增 `GET /sentinel-rules?target_ip=xxx&signal=xxx`（见 §5），按当前会话的 `target_ip` +
已选信号过滤，通常 0~2 条。

**卡片内容**（对齐 `ContinuousSessionDetailPage.js` 已有的 `Metric` 组件风格，不是表格）：

```
┌ 当前哨兵 ──────────────────────────────────────┐
│  调度延迟 · p99 > 5 ms                          │
│  冷却期 15 分钟 · 已触发 4 次 · 最近一次 3 分钟前   [删除]│
│  ● 已启用                                        │
└─────────────────────────────────────────────┘
```

- 没有规则时不展示空卡片，改成一行提示 + 一个"+ 添加哨兵"按钮，点击打开一个小弹窗（复用
  CreateContinuousSessionModal 里同款字段的极简版），而不是让用户回到创建流程重建会话。
- **删除键**：点击后 `window.confirm('停止「调度延迟 · p99 > 5 ms」的哨兵监控？停止后不再自动触发深度诊断。')`，
  确认后 `DELETE /sentinel-rules/{sid}`，成功后从卡片列表移除。删除规则不影响已经触发过的历史诊断任务和
  `DetectionEvent` 审计记录——删除只是停止未来判异，不做级联删除，理由和任务/日志系统的通常做法一致：
  审计记录要留痕。
- 规则处于降级状态（比如后端因为数据覆盖率不足连续跳过判异）目前后端没有暴露这个信号；MVP 阶段这张卡片先只显示
  启用状态和触发统计，不做"健康度"展示，避免承诺一个后端还没有的能力。

## 4. 时间轴高光异常时间段/点

**这是本次设计里改动面最大的一块**，落在 [ContinuousProfilingPanel.js](../web_frontend/src/components/ContinuousProfilingPanel.js)
的 histogram 视图上（第 863 行 `<HistogramPanel>`，内部 `trend` 数组见第 1661 行）。目前这个视图只有一张
趋势数据表（第 1730 行 `trend.slice(-20).map(...)`），**没有横向时间轴图**——你描述的"高光时间段/时间点、
hover 显示标签、点击跳转"这套交互，仓库里已经有一个几乎原样的实现可以照抄：
[TimelineChart.js](../web_frontend/src/components/TimelineChart.js)（周期任务时间轴用的 d3 色块图，
支持缩放、拖动、hover tooltip、点击跳转）。

**设计方案（已按你的反馈修正）**：不新起一个独立组件叠在上面、自己管一份 x 比例尺去跟原图对齐——那样两份
比例尺永远要手动同步，缩放/窗口切换时很容易错位。正确做法是**直接在渲染 histogram 趋势的同一段代码里加数据、
加图层**，标记和原有趋势曲线共用同一个 `x`/`draw()`：

- 如果 histogram 趋势这次顺带从纯表格升级成 SVG 时间轴（沿用 `TimelineChart.js` 的 d3 骨架：`d3.scaleTime`
  比例尺、`mousemove`/`mouseleave` tooltip、`onClick` 跳转），触发点标记就是这份 `draw()` 函数里多 append 的
  一组 `<circle>`/`<line>`，和趋势曲线共享同一个 `x`；缩放平移时标记跟着一起变换，不需要额外同步代码。
- 这样"复用 `TimelineChart.js`"就不是抄一份骨架另起一个组件，而是把 histogram 趋势视图**改造成**
  `TimelineChart.js` 同款结构、在同一个 `draw()` 里追加异常标记——一张图，一份比例尺，一套 hover/点击逻辑。

```
持续采集 histogram 趋势（同一张时间轴图，不是两层叠加）
──────────────────────────────────────────────
   曲线：p99 走势  ▲                ▲
                   │ 触发点标记，和曲线共用同一份 x 比例尺
──────────────────────────────────────────────
 12:00        12:30        13:00        13:30
```

- **数据来源**：`GET /sentinel-rules/events?target_ip=xxx&signal=xxx&from=xxx&to=xxx`（§5 新增只读接口，
  直接查 `detection_events` 表），只需要 `evaluated_at`/`status`/`child_tid`/`observed_value`/`floor_value`
  四五个字段，返回时间范围对齐 histogram 查询用的同一个 `[from, to]`，两个请求可以并发发。
- **高光的对象是"点"不是"段"**：`DetectionEvent` 本身是判异循环的一次快照（每 30 秒一次），没有天然的
  起止时间，所以视觉上用一个窄色块／三角标记（宽度固定几像素，类似 `TimelineChart.js` 里 `MIN_BAR_W` 的处理）
  标在 `evaluated_at` 对应的 x 位置，而不是画一个"异常区间"——避免编造一个后端数据里不存在的"异常持续了多久"。
  只标 `status === 'fired'` 的事件（红色），`skipped_*` 的不上时间轴，避免把判异闸门的内部细节暴露给用户造成噪音。
- **hover 显示的小标签**（对齐 `TimelineChart.js` 第 189-201 行 tooltip 样式，`rgba(40,40,40,.94)` 深底白字）：
  ```
  调度延迟 p99 41.0 ms（阈值 5.0 ms）
  08-24 14:32:05 · 已触发深度诊断
  ```
- **鼠标移开隐藏**：直接复用 `onMouseleave={() => setTip(null)}` 同款状态管理，不需要额外做法。
- **点击跳转**：`DetectionEvent.child_tid` 就是触发出的 `HotmethodTask.tid`，点击直接
  `navigate('/task/result?tid=' + event.child_tid)`——这个路由和组件已经存在（`TimelineChart.js` 第 129 行
  就是同款跳法），不需要新页面。
- 缩放/平移：既然标记和曲线现在是同一个 `draw()` 里的同一份数据绑定，`TimelineChart.js` 那套挂在整个 svg 上
  的 `d3.zoom()` 直接照搬即可——`zoom` 回调里重新调用一次 `draw(rescaledX)`，标记和曲线会一起跟着缩放走，
  不存在"两个图各自比例尺要不要同步"的问题。如果这次 histogram 视图暂不做缩放，就先按静态比例尺画一次，
  等以后要加缩放时也只需要照抄 `TimelineChart.js` 第 147-153 行的 zoom 挂载方式，不用额外为标记层单独处理。

## 5. 深度诊断详情页的返回键

`/task/result?tid=` 页面（`TaskResultPage.js`，未在本次改动范围内列出但路由已存在）目前的返回行为需要确认：
如果现状是硬编码跳一个固定路由，就无法满足你说的"点击返回能回到*前一个*页面"，因为访问来源不止一种
（从持续采集时间轴点进来 / 从周期任务时间轴点进来 / 直接打开链接）。

**设计**：用 `useNavigate()` 的 `navigate(-1)`（浏览器历史回退）作为默认返回行为，而不是拼一个固定的
"返回上级"链接——这样无论用户是从哨兵时间轴、周期任务时间轴、还是任务列表点进来的，返回都精确回到那个具体位置，
不需要每个来源页面都手动传 `from` 参数。唯一的例外：如果 `history.length` 很短（比如用户是通过直接粘贴链接
打开的，没有上一页可退），`navigate(-1)` 会跳出应用或停在原地——这种情况下需要一个兜底：用
`window.history.state?.idx > 0` 判断有没有真实历史记录，没有就退回到当前已有的固定路由兜底（比如任务列表页），
和现有其它详情页返回键的兜底逻辑保持一致（需要先确认 `TaskResultPage.js` 现状如何处理，这部分不在本次
探索范围内，实现时需要单独确认该文件当前的返回键实现）。

## 6. 新增后端接口（本文档范围内需要，但按你之前的分工选择，本轮不实现，只在此列出接口形状供前端对接参考）

现有后端（[detection.go](../apiserver/server/detection.go)）只有判异循环，没有任何面向前端的 CRUD/查询接口。
本设计要求补齐：

| 方法 | 路径 | 用途 |
|---|---|---|
| `POST` | `/sentinel-rules` | 创建规则（§2 创建流程使用） |
| `GET` | `/sentinel-rules?target_ip=&signal=` | 按目标+信号查规则（§3 详情卡片使用） |
| `DELETE` | `/sentinel-rules/{sid}` | 删除/停用规则（§3 删除键使用） |
| `GET` | `/sentinel-rules/events?target_ip=&signal=&from=&to=` | 查判异事件（§4 时间轴高光使用） |

这四个接口都是直接读写 `model.SentinelRule`/`model.DetectionEvent` 表的薄封装，不涉及判异逻辑本身，
可以独立于判异循环开发，风险很低。

## 7. 与上一版设计文档的关系

[docs/detection-trigger-pipeline-design.md](./detection-trigger-pipeline-design.md) §9 提出的是"新建独立
哨兵规则管理页"的方案，本文档是同一批后端能力（`sentinel_rules`/`detection_events`）的另一种前端呈现方式。
两者不冲突、也不需要二选一实现：管理页面适合"批量看所有规则、做运维台账"，本文档这套嵌入式设计适合"日常在某个
持续采集详情页里顺手看一眼有没有报警"。如果后续两种入口都要，§6 的四个接口是两版设计共用的，不需要重复设计
后端。

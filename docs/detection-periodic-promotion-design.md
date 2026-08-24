# 哨兵触发→周期性采集升级：设计方案

> **状态：已评估，暂不实施（2026-08-24）。**
> 本文档描述的方案曾按 §3 落地过一版代码（`SentinelRule.PromoteAfterConsecutive` 等），
> 但评估后判断不值得自动化，已回退。原因：①周期采样的时长/结束时机无法预设，只能靠
> "连续N次恢复正常"这类反应式计数滞后判断，不能真正解决"猜多久"的问题；②生产环境里
> 异常大多是瞬时的，"反复出现值得升级为周期采集"这个场景本身发生概率低；③没有到期机制
> 的周期采集会持续增加存储成本，而到期机制本身又受①所限，做不到干净。结论：单次触发
> 诊断本身已经把证据链（`DetectionEvent`/一次性诊断任务时间轴）留好，"要不要升级为周期
> 采集"交给人看着这条时间轴自己判断、手动走现有的 `CreateSchedule` 路径即可，不需要哨兵
> 自动决策。如果之后要重新捡起这个方向，先解决"到期机制"这个前置问题，不要照抄下面的
> §3 直接实现。
>
> 承接 `docs/detection-trigger-pipeline-design.md`(MVP,已落地)与
> `docs/detection-trigger-design-positioning.md`(§9.1 提出但未实现的"反复出现→周期计划"分支)。
> 本文档只解决一个问题:**同一目标同一信号反复触发时,如何从"一堆孤立的60秒诊断"升级为
> "一条持续追踪的周期性采样计划",以及升级之后如何验收。**
>
> 前提说明:代码现状核对以 2026-08-24 为准,`apiserver/server/detection.go` +
> `apiserver/model/detection.go`。MVP 判异是固定阈值(`FloorValue`),不是三层模型;
> `detection-trigger-design-positioning.md` 里的三层阈值模型是该文档的独立设计提案,
> **尚未落地**,与本文档要做的"周期化升级"是两件独立的事——本文档的方案不依赖三层模型
> 是否实现,固定阈值也能跑通。

## 1. 问题:为什么现在的哨兵触发"看不出模式"

`evaluateSentinelRule`(`apiserver/server/detection.go:78-139`)每次命中阈值,做的事情固定是:
建一条 `Duration=60s, Frequency=1` 的一次性诊断任务(`triggerDetectionDiagnosis`,detection.go:188-237),
然后进入 900 秒冷却期。如果同一目标同一信号每隔十几分钟就超一次阈值,运维在时间轴上看到的是
一串互不relate的60秒任务,必须自己在脑子里把时间戳拼起来才能判断"这是持续性劣化"还是
"纯粹偶发,互不相关"。这件事本该由系统做。

## 2. 参考的设计与理由

**Prometheus Alertmanager 的 `for` 语义**——告警规则里,`for: 10m` 表示条件持续满足10分钟才真正
触发(而不是抖一下就报),避免瞬时噪声升级为长期观测对象。本方案的"连续命中计数"是这个思路的
简化版:不引入时间窗口概念,直接用"连续几次评估周期都 fired"作为升级条件,原因是哨兵的评估间隔
(`detectionEvalInterval=30s`)本身很短,累计计数比维护一个时间窗口更简单,且和现有
`DetectionState` 单行缓存的结构直接兼容。

**SysOM 的"检测与深度诊断解耦"设计**(已在 `detection-trigger-design-positioning.md` §8 调研过)——
触发动作只负责"决定要不要升级",不负责"升级后怎么采集"。落到本方案:哨兵只负责把
`ScheduleTask` 创建出来并启用,采集本身完全交给现有 cron 调度器(`schedule.go:48-94`)执行,
两条链路除了"创建"这个动作外不再耦合。

**不参考的方案**:没有采用 Parca-agent 式的"持续动态调整采集频率"(运行时热调参),因为
mini-drop 现有 backend 不支持热调速(`detection-trigger-design-positioning.md` §7 已确认),
强行做等于新开一条采集通道,超出本文档范围,且与"稀疏触发"的约束冲突。

## 3. 方案

### 3.1 数据模型改动

`model.SentinelRule`(`apiserver/model/detection.go:21-45`)新增一个字段:

```go
// PromoteAfterConsecutive 连续命中达到这个次数时,自动创建/复用一条周期性采样计划。
// 0 或未设置 = 不做周期化升级,维持 MVP 的"每次触发都是一次性诊断"行为(向后兼容)。
PromoteAfterConsecutive int `gorm:"column:promote_after_consecutive;default:0" json:"promote_after_consecutive"`
// PromotedScheduleSID 记录已升级出的 ScheduleTask.SID,避免重复创建(幂等)。
PromotedScheduleSID string `gorm:"column:promoted_schedule_sid;size:64" json:"promoted_schedule_sid,omitempty"`
```

`model.DetectionState`(`apiserver/model/detection.go:49-55`)新增:

```go
// ConsecutiveFiredCount 连续 fired 的次数;只要中间出现一次非 fired(正常/冷却/跳过)就清零。
ConsecutiveFiredCount int `gorm:"column:consecutive_fired_count;default:0" json:"consecutive_fired_count"`
```

不新建表,复用现有两张表加字段,迁移成本最低。

### 3.2 计数逻辑

`markDetectionFired`(detection.go:240-253)是唯一一处触发成功后更新 `DetectionState` 的地方,
在这里顺带维护计数:

```go
func (s *APIServer) markDetectionFired(rule model.SentinelRule) {
    // ...现有 LastFiredAt 更新逻辑不变...
    // 新增:连续计数 +1
    newCount := state.ConsecutiveFiredCount + 1
    s.DB.Model(&state).Updates(map[string]interface{}{
        "last_fired_at": &now, "updated_at": now,
        "consecutive_fired_count": newCount,
    })
    if rule.PromoteAfterConsecutive > 0 && newCount >= rule.PromoteAfterConsecutive && rule.PromotedScheduleSID == "" {
        s.promoteToScheduleTask(rule)
    }
}
```

计数清零发生在 `evaluateSentinelRule` 里未命中阈值的分支(detection.go:113,`observed <= rule.FloorValue`
处):未超阈值说明这次是"恢复正常",不管之前连续了几次都应该清零,否则"曾经连续过"会一直残留导致
误升级。这里需要新增一次 `DetectionState` 归零写入(目前该分支直接 `return`,不碰
`DetectionState`,需要补上)。

### 3.3 升级动作:创建 ScheduleTask

新增 `promoteToScheduleTask`,直接复用 `CreateScheduleReq` 走的同一套创建逻辑(不是另起一套):

```go
func (s *APIServer) promoteToScheduleTask(rule model.SentinelRule) {
    cronExpr := "*/10 * * * *" // 默认10分钟一次,量级对齐 detectionLookback(5分钟窗口)
    req := CreateScheduleReq{
        Name:      rule.Name + "（哨兵升级为周期采样）",
        CronExpr:  cronExpr,
        TaskKind:  detectionSignalTaskKind[rule.Signal],
        TaskType:  TaskTypeBPF,
        ProfilerType: ProfilerBPF,
        TargetIP:  rule.TargetIP,
        Duration:  60,
        Frequency: 1,
        Event:     detectionSignalEvent[rule.Signal],
    }
    sch := &model.ScheduleTask{ /* 按 req 填充,SID/UID/UserName 沿用 rule */ }
    if err := s.DB.Create(sch).Error; err != nil {
        s.Logger.Error("哨兵升级周期计划失败", zap.String("rule_sid", rule.SID), zap.Error(err))
        return
    }
    s.addCronJob(*sch) // 立即挂进运行中的 cron 调度器,不需要重启进程
    s.DB.Model(&rule).Update("promoted_schedule_sid", sch.SID)
}
```

`PromotedScheduleSID` 写回后,`markDetectionFired` 里 `rule.PromotedScheduleSID == ""` 这个判断
天然保证同一条规则只升级一次——即便之后又连续命中好几轮,也不会重复建 `ScheduleTask`。

### 3.4 用户可控的降级路径

升级后的 `ScheduleTask` 和普通手动创建的定时任务完全一样,用户可以在现有定时任务管理页面直接
禁用/删除它——不需要为"哨兵创建的周期任务"单独做一套管理 UI,这是刻意选择,复用已有能力,
避免为一个边缘场景新建管理界面。哨兵规则本身的 `PromotedScheduleSID` 只用于防重复升级,
不反向控制那条 `ScheduleTask` 的生命周期,两者创建后即解耦(呼应 §2 的 SysOM 解耦设计)。

## 4. 改动范围(供后续实现时对照)

| 文件 | 改动 |
|---|---|
| `apiserver/model/detection.go` | `SentinelRule` +2字段,`DetectionState` +1字段 |
| `apiserver/server/detection.go` | `markDetectionFired` 补充计数与升级触发;未命中阈值分支补充计数归零;新增 `promoteToScheduleTask` |
| `apiserver/server/detection_test.go` | 新增：连续N次命中触发升级、升级后不重复创建、中途恢复正常后计数清零 三个用例 |

不改动 `schedule.go`——升级动作只是"调用已有创建路径",不需要修改 cron 调度器本身。

## 5. 验收标准

1. **单元测试**:在测试库里 seed 一条 `PromoteAfterConsecutive=3` 的规则,连续3次喂入超阈值观测值,
   断言第3次触发后 `sentinel_rules.promoted_schedule_sid` 非空,且 `schedule_tasks` 表出现对应记录、
   `enabled=true`。
2. **幂等验证**:同一条规则继续喂第4、5次超阈值观测值,断言 `schedule_tasks` 表该规则对应的行数
   仍为1(没有重复创建)。
3. **计数归零验证**:连续命中2次(未达到 `PromoteAfterConsecutive=3`)后喂一次正常值,再连续命中2次,
   断言仍未升级(计数在正常值那次被清零,不是"3次"简单累加到5次误触发)。
4. **端到端验证**(需要跑起来的环境):对一台被采集机器人为制造持续调度延迟(如用 `stress-ng`
   打满 CPU 制造调度争抢),配置一条 `sched_latency` 哨兵规则并设置
   `promote_after_consecutive=3`、评估间隔30秒、冷却期缩短到60秒方便测试;等待约
   3×(60秒冷却+触发间隔)后,在定时任务管理页面应该能看到一条新增的、名称带"哨兵升级为周期采样"
   后缀的 `ScheduleTask`,且后续按 cron 表达式持续产出诊断任务,不需要人再次手动创建。
5. **不影响 MVP 行为**:`promote_after_consecutive` 默认值0的既有规则(现有全部规则)行为不变——
   跑一遍现有 `detection_test.go` 全部用例应保持全绿,确认这是纯增量能力。

## 6. 本文档范围之外、仍待排期的缺口

按对下一步"导师复核"最有影响的顺序列出(不在本文档展开设计,避免文档内容超出标题范围):

- **`db_snapshot`(慢查询/锁等待)信号接入判异**——现在 `evaluateSentinelRule` 查不到映射直接跳过,
  是唯一未实现、但已被[DBA Gap Analysis](对慢请求根因定位最有价值)的信号,优先级应高于本文档。
- **规则管理前端/API**——现在只能直接写库,导师提的"前端需求"维度接不上。
- **触发通知/`DetectionEvent` 前端展示**——触发了但目前没有任何界面能看到,等于闭环没打通。
- **进程消失检测**(`detection-trigger-design-positioning.md` §5.6 已设计,未实现)。
- **滚动基线判异**(`RecentValues` 列已建未用)——固定阈值在负载天然变化后会失准,是否升级到
  `detection-trigger-design-positioning.md` 提出的三层模型,需要用户单独拍板,不在本文档决定范围。

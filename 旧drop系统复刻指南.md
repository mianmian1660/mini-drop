# Drop 性能诊断系统 · 完整逆向复刻指南

> **目标读者**：希望从零开始独立复刻一个"分布式性能采集与分析平台"的工程实习生 / 高年级学生 / 中级工程师。
> **教学方式**：本文档是基于现有 `drop / apiserver / analysis / web_frontend` 4 个仓库逆向得到的"产品规格 + 架构说明 + 复刻路线图"，**不直接给代码**，而是把每个模块的"输入 / 输出 / 职责 / 接口契约 / 数据流 / 易踩坑点"讲清楚，让你自己写代码完成。

---

## 0. 总览：Drop 系统是什么

Drop 是一个**面向 Linux 服务器与容器场景的"按需性能采集 + 可视化分析"平台**。一句话概括：

> 用户在 Web 上点一下"采集"，平台把 perf / async-profiler / eBPF / pprof / memray 等工具下发到目标机器执行，把原始数据收回中心，跑一系列分析（火焰图、热点、内存泄漏、Java 堆分析、时序异常检测等），最后在前端给出可视化报告与 AI 优化建议。

### 0.1 四个仓库的职责划分

```
┌──────────────────────────┐   HTTP/JSON   ┌────────────────────────────┐
│  web_frontend (React)    │ ───────────→  │  apiserver (Go + Gin)       │
│  ─ 任务列表/详情/火焰图   │ ←───────────  │  ─ 鉴权、任务编排、DB、对象存储 │
└──────────────────────────┘               └────────────┬───────────────┘
                                                        │ gRPC
                                                        ▼
                                           ┌────────────────────────────┐
                                           │  drop (C++)                 │
                                           │  ┌──────────┐  ┌─────────┐  │
                                           │  │  Server  │──│  Agent  │  │
                                           │  │ (调度)   │  │ (采集)  │  │
                                           │  └──────────┘  └─────────┘  │
                                           └────────────┬───────────────┘
                                                        │ 产出 perf.data / 折叠栈 / 堆 dump
                                                        ▼
                                           ┌────────────────────────────┐
                                           │  analysis (Python+Go)       │
                                           │  ─ 火焰图 / 热点 / 内存 / Java堆/ AI建议 │
                                           └────────────────────────────┘
```

| 仓库 | 主要语言 | 进程形态 | 关键目录 |
|---|---|---|---|
| `drop` | C++14 / CMake | 长驻守护进程 `drop_agent` + `drop_server` | `agent/ server/ common/ tools/` |
| `apiserver` | Go 1.18+ / Gin / GORM | HTTP 服务 + gRPC 客户端 | `server/ model/ middleware/ proto/ pkg/` |
| `analysis` | Python 3.8+ (主) + Go (Java 堆) | 命令行工具 / 一次性脚本 | `data_parser/ drop_analyzer/ java_heap_analyzer/` |
| `web_frontend` | React 16 + Redux-Saga + TDesign | 静态站点 | `src/pages/ src/components/ src/api/` |

### 0.2 一次完整任务的端到端流程

```
[1] 用户登录 web_frontend → 跳到 /index 看到自己的 Agent 列表
[2] 点 "新建采样" → 选目标 Agent + 采集类型(CPU/内存/IO/Java...) + 参数 (PID/时长/频率)
[3] 前端 POST /api/v1/tasks → apiserver
[4] apiserver:
      a. 写 hotmethod_task 表 (status=0 新建), 生成 tid
      b. gRPC ControlService.CreateTask → drop_server
[5] drop_server: 把 TaskDesc 塞进 tasks_[targetIP] 队列
[6] drop_agent 每秒一次 HealthCheck.Do() 心跳 → 拿到 TaskDesc
[7] drop_agent: fork 一个 perf/async-profiler 子进程采集 N 秒
[8] 采集完, drop_agent 把产出文件 (perf.data / 折叠栈) 上传到 COS
[9] drop_agent: gRPC NotifyResult → drop_server, 带上 cos 路径
[10] drop_server 写库, 更新任务状态
[11] apiserver 定时器/前端轮询: 看到 status=2(完成), 触发 analysis
[12] analysis: 从 COS 拉数据 → 生成火焰图 SVG / 热点 JSON / AI建议 → 写回 COS
[13] 前端轮询拿到 analysis_status=2 → 跳到 /task/result 渲染火焰图
```

记住这个流程，后面所有模块都是为这一条链路服务的。

---

## 1. 仓库 A：drop（C++ 采集核心）

### 1.1 这块要让学生做出什么

一个由两个二进制组成的小型分布式系统：
- `drop_server`：**集中调度 + 转发结果**。监听一个 gRPC 端口，接收 apiserver 的下发请求，按 `target_ip` 维护任务队列。
- `drop_agent`：**远程采集探针**。每秒向 server 心跳，拉到任务后 fork 子进程跑 perf / async-profiler 等工具，结果上传 COS 后通过 gRPC 回报。

### 1.2 模块拆解

```
drop/
├── common/
│   ├── proto/                # 全部 gRPC 协议定义
│   │   ├── healthcheck.proto # 心跳服务（Agent → Server）
│   │   ├── hotmethod.proto   # 任务下发服务（Server → Agent）
│   │   ├── control.proto     # 控制平面（apiserver → Server）
│   │   ├── init.proto        # Agent 注册与配置拉取
│   │   └── common.proto      # 通用结构 (PidStats / File / CosConfig)
│   ├── Perf.cpp              # 封装 Linux perf 工具（fork+exec）
│   ├── Process.cpp           # 读 /proc/[pid]/stat,io 计算 CPU/内存/IO 使用率
│   ├── COSClient.cpp         # 腾讯云 COS SDK 封装（5 种连接模式）
│   ├── Daemon.cpp            # 守护进程化（fork + setsid + 关 stdio）
│   ├── ScriptRunner.cpp      # 通用脚本执行器（处理 ScriptExec 任务）
│   └── Log.cpp               # 基于 glog 的日志
│
├── agent/                    # 二进制 drop_agent
│   ├── main.cpp              # 入口：解析 flag → 加载 config → 守护化 → 启动通道
│   ├── Config.h/cpp          # JSON 配置读取 + 多 Server 故障转移
│   ├── HealthCheckChannel.*  # 1Hz 心跳线程，把心跳结果中的 TaskDesc 投递到队列
│   ├── HotmethodChannel.*    # 工作线程池：从队列取任务 → 选采集器 → 执行 → 回报
│   └── ContainerInfo.*       # 容器/cgroup 信息识别
│
├── server/                   # 二进制 drop_server
│   ├── main.cpp              # 启动 4 个 gRPC service
│   ├── HealthCheckService.*  # 处理 Agent 心跳，返回待派任务
│   ├── HotmethodService.*    # 维护 tasks_[ip] 队列；接收 NotifyResult
│   ├── ControlService.*      # apiserver 的入口：CreateTask/FetchData/StatAgent
│   └── InitAgentInfoService.*# Agent 启动时拉 COS 配置
│
├── client/                   # Python CLI（开发期调试用，不重要）
├── etc/                      # config.json.example、自签名证书
├── tools/                    # 第三方 perf / async-profiler / eBPF 工具二进制
└── CMakeLists.txt
```

### 1.3 必须实现的 4 个 gRPC 服务（接口契约）

学生第一步就把下面 4 个 `.proto` 文件写出来，后续完全围绕它们展开。

#### 1.3.1 `healthcheck.proto`

```proto
service HealthCheck {
  rpc Do(HealthCheckRequest) returns (HealthCheckResponse);
}
message HealthCheckRequest {
  string hostName       = 1;
  string ipAddr         = 2;
  string uid            = 3;   // Agent 自己的唯一 id
  string agentVersion   = 4;
  PidStats selfPstats     = 5; // Agent 自身资源
  PidStats childrenPstats = 6; // Agent 启动的子进程聚合资源
}
message HealthCheckResponse {
  enum ServingStatus { UNKNOWN=0; SERVING=1; NOT_SERVING=2; }
  ServingStatus status = 1;
  bool          pending = 2;       // 是否带任务
  hotmethod.TaskDesc taskDesc = 3; // 待执行任务
}
```

#### 1.3.2 `hotmethod.proto`（最核心）

```proto
service Hotmethod {
  rpc Collect(Target)            returns (google.protobuf.Empty); // server -> agent: 同步下发
  rpc NotifyResult(TaskResult)   returns (google.protobuf.Empty); // agent -> server: 上报结果
}
message TaskDesc {
  string  taskID        = 1;
  uint32  taskType      = 2;     // 0 通用 / 1 Java / 2 Tracing / 4 MemCheck / 6 JavaHeap ...
  uint32  profilerType  = 3;     // 0 perf / 1 async-profiler / 2 pprof
  RecordArgv sampleArgv = 4;
  string  containerName = 5;     // 容器 / Pod 名（可空）
  uint32  containerType = 6;
  uint32  timeoutSec    = 7;
  CosConfig cosConfig   = 8;     // 临时 COS 凭证
  string  scriptContent = 9;     // 仅 ScriptExec 任务使用
}
message RecordArgv {
  uint32 hz        = 1;          // 采样频率 Hz
  uint64 duration  = 2;          // 采集秒数
  int32  pid       = 3;          // 目标进程
  string callgraph = 4;          // fp / dwarf / lbr
  bool   subprocess= 5;
  string event     = 6;          // cpu-cycles / cache-misses ...
}
message TaskResult {
  string taskID       = 1;
  string errorMessage = 2;       // 空字符串=成功
  File   file         = 3;       // 内联小文件，大文件走 COS
  string cosKey       = 4;       // 大文件的 COS key
  repeated PidStats selfPstats     = 5;
  repeated PidStats childrenPstats = 6;
}
```

#### 1.3.3 `control.proto`（apiserver 调用）

```proto
service Control {
  rpc CreateTask(CreateTaskRequest) returns (CreateTaskResponse);
  rpc FetchData(FetchDataRequest)   returns (FetchDataResponse);
  rpc StatAgent(StatAgentRequest)   returns (StatAgentResponse);
}
```

#### 1.3.4 `init.proto`

Agent 启动时调一次 `RegisterAgent` + `FetchConfig`，拿到一份 `CosConfig`（一次性临时密钥），用于上传结果。

### 1.4 Agent 端的关键技术点（学生最容易卡住的点）

1. **守护化**：`fork() → 父退出 → setsid() → 再 fork() → 关 0/1/2 文件描述符 → 重定向到 /dev/null`。
2. **心跳线程 + 工作线程**：心跳 1 Hz 不能被采集任务阻塞，**任务必须丢进自己的队列**，由独立线程消费。
3. **fork/exec 子进程**：用 `posix_spawn` 或 `fork+execvp` 启动 `perf record -F 99 -g -p <pid> -- sleep <dur>`；**一定要 `waitpid` 防僵尸**。
4. **超时杀进程**：用 `setpgid` 把 perf 放进独立进程组，超时用 `killpg(pgid, SIGTERM)`，5 秒后再 `SIGKILL`。
5. **采集器抽象**：定义 `class IProfiler { virtual int record(...)=0; virtual int collect_result(...)=0; };`，每个工具一个子类（`PerfProfiler`、`AsyncProfilerProfiler`、`PprofProfiler`、`MemrayProfiler`、`JavaHeapDumper`、`ScriptRunner`）。
6. **CPU/内存/IO 自监控**：周期采集自身和所有子进程的 `/proc/<pid>/stat` 与 `/proc/<pid>/io`，计算 1 秒区间内的 CPU% / RSS / 读写 KB/s，写进 `PidStats` 上报。这是避免"采集探针自身打挂业务进程"的关键。
7. **COS 上传 5 种模式**：内网+FLAG / 公网 / 仅 FLAG 内网 / 仅配置内网 / HTTP 代理。设计为**链式回退**：上传失败就尝试下一种模式。
8. **多 Server 故障转移**：配置里允许写多个 `server_ips`，启动时挨个 `RegisterAgent`，拿到第一个成功的就用。

### 1.5 Server 端的关键设计

1. **任务队列**：`std::map<string /*targetIP*/, std::deque<TaskDesc>> tasks_;` + `std::mutex`。
2. **心跳消费**：`HealthCheckService::Do()` 收到心跳 → `tasks_[req.ipAddr]` 弹一个出来塞进 response.taskDesc。
3. **任务状态机**：`PENDING → DISPATCHED(已派给 Agent) → RUNNING(Agent 报告开始) → FINISHED/FAILED/TIMEOUT`。
4. **超时清理**：单独线程扫描，DISPATCHED 超过 30 秒未报开始视为掉线。
5. **存储抽象**：把"结果落地"封装成 interface（本地文件 / PostgreSQL / 仅转发），**不要把 SQL 直接写在 RPC 处理函数里**。

### 1.6 复刻顺序（drop 仓库）

| 周 | 目标 | 验收 |
|---|---|---|
| W1 | 4 个 .proto 写完，CMake + grpc 跑通；写一个 hello-world 的 server/agent，心跳互通 | `./drop_agent → ./drop_server`，server 日志能看到 "host=xxx ip=yyy" 心跳 |
| W2 | Agent 集成 perf：固定参数 `perf record -F 99 -g -- sleep 10`，结果落到 /tmp | 给 server 发一个假 TaskDesc，Agent 真的产出 perf.data |
| W3 | 任务全链路跑通：CreateTask(grpc cli) → server 队列 → agent 拉到 → perf → NotifyResult | 用 `grpcurl` 调 ControlService.CreateTask，10 秒后能在 server 日志看到 NotifyResult |
| W4 | 接 COS（或 MinIO 替代）；自监控 PidStats；超时杀进程 | 故意 PID 不存在，看到 errorMessage 正确返回 |
| W5 | 加第 2/3 个采集器：async-profiler（Java）、pprof（Go）；配置文件、多 Server 故障转移 | 4 种采集器都能跑 |

### 1.7 易踩的坑（必读）

1. `perf_event_paranoid`：CentOS/Ubuntu 默认是 2 或 4，普通用户 perf 不到任何东西。**写入 `/proc/sys/kernel/perf_event_paranoid` 为 1 或更低**，或给二进制 `setcap cap_perfmon,cap_sys_resource=ep`。
2. **gRPC 与 protobuf 版本必须一致**，不一致会出现链接期符号找不到或运行期 segfault。锁定版本：grpc 1.48.x + protobuf 3.20.x。
3. **`perf record` 的 callgraph fp 模式要求被测程序 `-fno-omit-frame-pointer`**，否则栈是断的。dwarf 模式更通用但 perf.data 体积大 5～10 倍。
4. fork 子进程必须 close 父进程持有的 socket fd，否则 server 重启后 socket 不会真的释放。
5. 多线程访问 `tasks_` 必须加锁；用 `condition_variable` 唤醒心跳处理线程，而不是 sleep 轮询。
6. **不要用 `system()` 调外部命令**，会通过 shell 解释，注入风险且杀不掉。一律 `posix_spawn` 或 `fork+execvp`。

### 1.8 前置知识清单

- C++14 现代特性（智能指针、`std::thread/mutex/condition_variable`、lambda）
- CMake 与 `find_package` 写法
- Protobuf3 + gRPC C++（unary RPC 即可，不需要 streaming）
- Linux 系统编程：`fork/exec/wait`、信号、`/proc`、`pthread`、`setrlimit`、`setpgid`
- glog / gflags 用法
- perf / async-profiler 命令行用法
- 至少一种对象存储 SDK（COS 或 S3 或 MinIO）

---

## 2. 仓库 B：apiserver（Go 后端编排层）

### 2.1 这块要让学生做出什么

一个标准的 Go HTTP 后端：**对前端讲 REST/JSON，对 drop_server 讲 gRPC，把任务、用户、Agent、分组、定时任务、AI 建议这些业务实体落到 PostgreSQL，把任务产出文件存到 COS**。

### 2.2 技术栈选型（和原项目一致）

```
Web 框架：    Gin v1.10
ORM：         GORM v1.25 (PostgreSQL 主，可选 MySQL)
配置：        Viper + YAML
日志：        Zap (访问日志 JSON, 错误日志 console)
gRPC client： grpc-go 1.48
定时任务：    robfig/cron v3
对象存储：    腾讯云 COS SDK 或 MinIO Go SDK
鉴权：        Cookie + 自有 TPS Token (中小项目可换 JWT)
```

### 2.3 数据模型（最少 7 张表）

复刻 MVP 只要这 7 张就够，剩下 15 张是企业内的扩展。

```
1. user_info(uid PK, name, groups jsonb, key)
2. agent_info(id PK, hostname, ip_addr, online bool, uid FK, gid, version, environment)
3. hotmethod_task(id PK, tid UNIQUE, name, type, profiler_type, target_ip,
                  request_params jsonb, status, status_info,
                  uid, user_name,
                  create_time, begin_time, end_time,
                  master_task_tid, deleted_at)   -- ⚠ 任务核心表
4. multi_tasks(tid PK, sub_tids jsonb, type, status, analysis_status, trigger_type)
5. groups(gid PK, name, owner_id)
6. group_members(gid, uid)        -- 复合主键
7. analysis_suggestion(id PK, tid FK, func, suggestion text, ai_suggestion text, status)
```

**任务状态机**：`0 新建 → 1 执行中 → 2 成功 / 3 失败`
**分析状态机**（独立）：`0 待分析 → 1 分析中 → 2 成功 / 3 失败`

### 2.4 必须实现的 12 个核心 API（MVP）

| # | Method | Path | 作用 | 备注 |
|---|---|---|---|---|
| 1 | GET | `/api/v1/auth/check` | 鉴权回调 / 检查 cookie | 失败 302 跳登录 |
| 2 | GET | `/api/v1/users` | 获取当前用户信息 | |
| 3 | GET | `/api/v1/agents` | Agent 列表（含组共享） | |
| 4 | GET | `/api/v1/agent/stat` | 查询某 Agent 当前资源占用 | gRPC StatAgent 透传 |
| 5 | POST | `/api/v1/tasks` | 创建任务 | 写库 + gRPC CreateTask |
| 6 | GET | `/api/v1/tasks` | 任务列表（自己 + 组内共享，分页） | |
| 7 | GET | `/api/v1/tasks/:tid` | 任务详情（含产出 URL） | 生成 COS 临时签名 |
| 8 | DELETE | `/api/v1/tasks/:tid` | 软删任务 | 事务级联删 suggestion / tag / cos |
| 9 | POST | `/api/v1/tasks/:tid/retry` | 用同参数重新建任务 | |
| 10 | GET | `/api/v1/cosfiles?tid=xxx` | 列任务产出文件并签名 | 15min 临时 URL |
| 11 | POST | `/api/v1/group` + 添加成员/Agent | 用户组管理 | |
| 12 | POST | `/api/v1/schedule/task` | 创建定时任务 | cron 表达式 |

### 2.5 关键代码结构（推荐目录）

```
apiserver/
├── main.go                 # 解析 -c → 加载 config → 初始化 DB/gRPC/Storage/Cron → router.Run
├── config/                 # Viper 加载，支持热加载日志级别
├── server/
│   ├── server.go           # 持有 Db / cc(gRPC) / cron / storage / auth 的 APIServer 结构体
│   ├── auth.go             # /auth/check + 中间件 CheckLogin
│   ├── task.go             # 任务相关 11 个 handler（CRUD/重试/列表/详情）
│   ├── agent.go            # 2 个 handler
│   ├── group.go            # 组管理 6 个 handler
│   ├── schedule.go         # 定时任务
│   ├── suggestion.go       # 分析建议读写 + AI 流式
│   ├── flame.go            # 火焰图 diff 计算
│   └── const_enum.go       # 全部状态码 / 错误码常量
├── model/                  # 7~22 张表的 GORM Model + AutoMigrate
├── middleware/log.go       # access log 中间件，记录 method/path/status/latency/body
├── proto/                  # 复制自 drop 仓库的 control.proto / hotmethod.proto，go generate
├── pkg/storage/            # interface Storage { Get/PutF/PreSign/Delete/IsExist }
│   ├── cos/                # 腾讯云 COS 实现
│   └── minio/              # MinIO 实现（学生用 MinIO 即可）
├── util/                   # db.go / storage.go / log.go / httpclient.go
└── apiserver.yaml.example
```

### 2.6 创建任务的核心代码骨架（伪代码）

```go
func (s *APIServer) CreateTask(c *gin.Context) {
    var req CreateTaskReq
    if err := c.ShouldBindJSON(&req); err != nil { ... }

    uid := c.GetString("uid")  // 来自鉴权中间件
    tid := genTID()            // 短 UUID

    // 1) 写库
    task := &model.Task{
        TID: tid, Name: req.Name, Type: req.Type, ProfilerType: req.ProfilerType,
        TargetIP: req.TargetIP, RequestParams: mustJSON(req),
        Status: 0, UID: uid, UserName: c.GetString("user_name"),
        CreateTime: model.Now(),
    }
    if err := s.Db.Create(task).Error; err != nil { ... }

    // 2) 调 drop_server
    pbReq := &pb.CreateTaskRequest{
        TargetIP: req.TargetIP,
        Service: "hotmethod",
        TaskDesc: buildTaskDesc(tid, req),  // 翻译成 protobuf
    }
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    if _, err := s.cc.CreateTask(ctx, pbReq); err != nil {
        // 任务在 drop 端没收到，回滚状态为失败
        s.Db.Model(task).Updates(map[string]interface{}{
            "status": 3, "status_info": "dispatch failed: " + err.Error(),
        })
        c.JSON(500, ...); return
    }

    c.JSON(200, gin.H{"code": 0, "data": gin.H{"tid": tid}})
}
```

### 2.7 易踩的坑

1. **GORM 默认连接池小**：高并发下立刻打爆。`SetMaxOpenConns(100); SetMaxIdleConns(10); SetConnMaxLifetime(time.Hour)`。
2. **gin 中间件记录 body** 容易内存泄漏 → 大文件超过 10MB 不要 `bytes.Buffer` 缓存。
3. **Cron 定时任务多实例重复执行**：用 Redis 分布式锁或单实例 `--enable-cron=true` 标志。
4. **PreSign 过期太短**：前端复制链接发同事打开就 403。MVP 设 1 小时以上。
5. **任务权限漏检**：所有 list / get / delete 必须带 `WHERE uid = ? OR gid IN(...)`。
6. **PostgreSQL 跨年使用 JSONB**：用 GORM 的 `datatypes.JSON / datatypes.JSONSlice[T]`，不要自己 string→json。
7. **gRPC TLS 证书路径**：相对路径在 systemd 下经常找不到，强制绝对路径。
8. **Cookie 跨域**：前端 `withCredentials=true`，后端必须 `Access-Control-Allow-Credentials: true` 且 Origin 不能用 `*`。

### 2.8 复刻顺序（apiserver 仓库）

| 周 | 目标 | 验收 |
|---|---|---|
| W1 | 项目骨架 + Viper + GORM + AutoMigrate(7 张表) + 一个 healthz | `curl /healthz` 返回 ok，PG 里能看到表 |
| W2 | 鉴权 + 用户/Agent/任务 列表与创建（先 mock drop） | postman 跑通 8 个 GET/POST |
| W3 | 接通 drop_server gRPC：CreateTask 真的下发 | 用 W3 完成的 drop 联调，10 秒后 status=2 |
| W4 | MinIO 存储 + 文件签名 + 任务详情 | 前端能下载到 perf.data |
| W5 | 用户组 + 定时任务 + 软删除 + access log | 6 个组 API 跑通 |

---

## 3. 仓库 C：analysis（Python 分析引擎）

### 3.1 这块要让学生做出什么

一组**离线脚本 / Job**：从 COS 拉到 Agent 上传的原始性能数据（`perf.data` / `pprof.heap` / `*.hprof` / 折叠栈 txt 等），跑一系列分析，把可视化结果（SVG 火焰图 / JSON 调用图 / Markdown 建议）写回 COS，并在 PostgreSQL 里写 `analysis_suggestion` 行。它**不是常驻 HTTP 服务**，而是被 apiserver 用 `subprocess` 或 K8s Job 调起来。

### 3.2 文件清单与职责

| 文件 | 职责 |
|---|---|
| `hotmethod_analyzer.py` | **总入口**。命令行收 task_id → 从 PG 拿任务参数 → 从 COS 下载原始数据 → 按 task_type 分发到具体 analyzer → 上传产物 → 写状态回 PG |
| `data_parser/collapsed_data_parser.py` | 把 perf script 输出的"原始栈"折叠成 `func1;func2;func3 1234` 一行一栈的格式（火焰图标准输入） |
| `data_parser/pprof_data_parser.py` | 解析 Go/C++ pprof profile（基于 `pprof` 命令行） |
| `data_parser/pprof_heap_parser.py` | 解析 pprof heap profile（内存采样） |
| `flamegraph.py` + `flamegraph.pl` + `stackcollapse-perf.pl` | 火焰图生成链：`perf script → stackcollapse-perf.pl → flamegraph.pl → SVG` |
| `hotmethod_common.py` | 解析折叠栈，计算每个函数 self+inclusive 时间，输出 TopN |
| `memleak_analyzer.py` | 内存泄漏识别：把 alloc/free 配对，没 free 的就是泄漏；倒序找"第一个用户函数"作为责任人 |
| `bw_sync_analyzer.py` + `detect_bw_sync.py` | 时序异常检测：相同负载下两个时刻的指标对比 |
| `resource_analyzer.py` | 解析 PidStats 历史，给出 CPU / 内存 / IO 占用曲线 |
| `biotrace.py` | 解析 eBPF biosnoop 输出，找慢 IO |
| `namespace_parse.py` | 容器 / cgroup 命名空间解析 |
| `java_heap_analyzer/` (**Go 子项目**) | Java HPROF 堆分析：解析 hprof 二进制 → 输出对象图 / 主导树 / 大对象列表 |
| `drop_analyzer/assembly_code_analyzer/assembly_code_analyzer.py` | 反汇编 + 注解，结合 perf 数据找热点指令 |
| `analysis_advisor.py` | 规则匹配引擎：根据热点函数名匹配预置规则，给出"建议改用 X 方案"的中文建议 |
| `rainbow.py` + `hunyuanApi.py` | 调腾讯彩虹石（配置中心）拉规则；调腾讯混元 LLM 生成 AI 建议 |
| `storage.py` | `class Storage / class COSStorage / class MinIOStorage` 接口 |
| `error.py` | 错误码 + ErrorInfo 结构体 |

### 3.3 数据流

```
COS: /<tid>/perf.data
       │
       ▼
hotmethod_analyzer.py:
  - subprocess: perf script -i perf.data > perf.script.txt
  - subprocess: stackcollapse-perf.pl perf.script.txt > collapsed.txt
  - subprocess: flamegraph.pl collapsed.txt > flamegraph.svg
  - hotmethod_common.parse(collapsed.txt) -> top.json
  - analysis_advisor.match(top.json) -> suggestions.md
  - hunyuanApi.ask(top.json + source) -> ai_suggestion.md
       │
       ▼
COS: /<tid>/flamegraph.svg, top.json, suggestions.md, ai_suggestion.md
PG : insert analysis_suggestion (tid, func, suggestion, ai_suggestion)
PG : update hotmethod_task set analysis_status=2
```

### 3.4 学生最少要做的 3 个 analyzer（MVP）

1. **CPU 火焰图** —— `perf script` → `stackcollapse-perf.pl` → `flamegraph.pl`，纯调外部脚本，10 行 Python。
2. **TopN 热点函数** —— 解析折叠栈，按分号分割，统计每个函数出现次数，排序输出 JSON。
3. **规则建议引擎** —— 一个 yaml 配置：`{ regex: ".*malloc.*", advice: "考虑用 jemalloc 或对象池" }`，匹配 TopN 函数名给建议。

> 这 3 个加起来约 300 行 Python，配 perl 脚本就能跑出像样的 demo。Java 堆 / eBPF / AI 建议是后续扩展。

### 3.5 入口约定（很重要）

`hotmethod_analyzer.py` 接受**这样**的命令行：

```
python3 hotmethod_analyzer.py \
    --task-id <tid> \
    --task-type <int> \
    --config /etc/analysis/config.ini
```

退出码：`0 成功 / 非 0 失败`，`stderr` 里写 ErrorInfo JSON。
**这是 apiserver 调度它的契约**，不要随便改命令行格式。

### 3.6 易踩的坑

1. `flamegraph.pl` 是 Perl 脚本，记得 `chmod +x` 并安装 `perl`。
2. `perf script` 在不同内核版本输出格式略有差异，stackcollapse 偶尔丢栈。给 perf 加 `--header --no-inline` 更稳。
3. Java async-profiler 的折叠栈格式和 perf 不一样，需要用 `flamegraph.pl --color=java`。
4. 解析 HPROF 二进制非常吃内存，**一定走 Go 子项目（`java_heap_analyzer/`）**，不要硬塞 Python。
5. COS 下载大文件用流式（`get_object` 返回 stream），别 `.read()` 全装内存。
6. AI 建议（混元）会限速，做指数退避。
7. 同一个 tid 可能被重复触发分析，**用文件锁或 PG 行锁** 保证幂等。

### 3.7 推荐技术栈

```
Python 3.8+    主语言
psycopg2       PG driver
qcloud-cos     COS SDK，可换 minio
pandas         折叠栈计算
matplotlib     时序图（resource_analyzer 用）
sseclient-py   流式接 LLM
Go 1.18+       Java HPROF 解析子项目
perf, perl     系统级依赖（Dockerfile 里 yum install）
```

### 3.8 复刻顺序（analysis 仓库）

| 周 | 目标 | 验收 |
|---|---|---|
| W1 | `storage.py` + 命令行入口 + 配置加载 | `python hotmethod_analyzer.py --task-id xxx` 不报错 |
| W2 | 实现 perf → 火焰图链路 | 给一个 perf.data，能产出 svg |
| W3 | TopN 热点 + 规则建议引擎 | 产出 top.json + suggestions.md |
| W4 | 接 PG 写 analysis_suggestion；接 COS 上传产物 | 端到端：apiserver 调 analyzer → 前端能看到结果 |
| W5 | 内存泄漏 / Java 堆（任选一个） | 完整 MVP |

---

## 4. 仓库 D：web_frontend（React 可视化）

### 4.1 这块要让学生做出什么

一个登录后能"建任务、看任务、看火焰图"的 SPA。核心难点不是 React，是**火焰图渲染**和**轮询任务状态**。

### 4.2 技术栈

```
React 16/18      函数组件 + Hooks
react-router 5/6 路由
Redux + redux-saga    或 Zustand（更简单，推荐学生用）
axios            HTTP 客户端 + 拦截器
TDesign / Ant Design    UI 组件
D3 v7            火焰图
ECharts          时序图
pako             解压 gz 数据
```

### 4.3 4 个必做页面（MVP）

| 路由 | 名字 | 功能 |
|---|---|---|
| `/login` 或 `/auth/check` | 登录回跳 | 拿 cookie 后回首页 |
| `/index` | 主页 | 我的 Agent 列表 + 我的任务列表 + "新建采样" 按钮（弹窗） |
| `/tasks` | 全部任务 | 表格分页 + 搜索 + 删除 |
| `/task/result?tid=xxx` | 任务详情 | 三个 Tab：基本信息 / 火焰图 / 热点 TopN / AI 建议 |

### 4.4 axios 拦截器约定

```js
axios.defaults.baseURL = window.config.HOST_URL;
axios.defaults.withCredentials = true;
axios.defaults.timeout = 30000;

axios.interceptors.request.use(cfg => {
  cfg.headers['Drop_user_uid']  = getCookie('drop_user_uid');
  cfg.headers['Drop_user_name'] = getCookie('drop_user_name');
  return cfg;
});

axios.interceptors.response.use(
  r => r.data,
  err => {
    if (err.response?.status === 401 && err.response.data.code === 4010001) {
      const loc = new URL(err.response.data.data.location);
      loc.searchParams.set('redirect_uri', window.location.href);
      window.location.href = loc.toString();
    }
    return Promise.reject(err);
  }
);
```

### 4.5 火焰图怎么渲染

最省事的方案：**直接用 d3-flame-graph npm 包**，给它喂 `{name, value, children}` 树。

数据来源 2 选 1：
- 让 analysis 直接产 SVG，前端 `<iframe>` 加载 → 5 行代码搞定。
- 让 analysis 产 JSON 树（pako gz），前端用 d3-flame-graph 渲染 → 可交互（点击放大、搜索高亮）。

原项目走第 2 条路，自研了 `src/components/flamegraph/d3-flamegraph/`。学生 MVP 走第 1 条路即可。

### 4.6 轮询任务状态

```js
useEffect(() => {
  if (task.status >= 2) return;       // 已完成不轮询
  const id = setInterval(() => {
    api.getTask(tid).then(t => setTask(t));
  }, 3000);
  return () => clearInterval(id);     // 卸载清理！
}, [tid, task.status]);
```

**易错**：忘 return clearInterval → 切页面后还在跑，越来越多。

### 4.7 推荐目录

```
src/
├── api/         axios 实例 + 每个后端接口一个函数
├── store/       zustand 或 redux
├── pages/
│   ├── login/
│   ├── home/        # /index
│   ├── taskList/    # /tasks
│   └── taskResult/  # /task/result
├── components/
│   ├── header/  顶部导航
│   ├── flamegraph/  封装 d3-flame-graph
│   └── createTaskModal/  新建任务弹窗
├── router.tsx
├── App.tsx
└── index.tsx
```

### 4.8 易踩的坑

1. **跨域 + Cookie**：`withCredentials=true` 必须配，后端 `Access-Control-Allow-Credentials: true` + 显式 Origin。
2. **CRA 里 TDesign 主题色**：要用 craco override 改 less-loader。
3. **火焰图渲染卡死**：超过 50 万个栈帧浏览器会卡死，先在 analysis 端做"自顶向下深度截断"。
4. **大文件下载**：直接用 COS 临时签名 URL，不要走后端中转。
5. **大列表卡顿**：超过 1000 行用 `react-window`。
6. **生产环境配置注入**：`public/config/config.js` 在 `index.html` 之前加载，部署时由 nginx 模板生成，不要打进 build。

### 4.9 复刻顺序（web_frontend 仓库）

| 周 | 目标 | 验收 |
|---|---|---|
| W1 | CRA 起一个 + 路由 + axios + 登录回跳 | 能拿到 cookie，看到自己用户名 |
| W2 | 主页 + 任务列表（mock 数据） | 表格能搜索、分页 |
| W3 | 新建任务弹窗 + 真接 apiserver | 能从前端真的发起一次任务 |
| W4 | 任务详情 + 火焰图 iframe | 能看到完整 svg 火焰图 |
| W5 | 轮询 + 删除 + 错误处理 + 部署 docker | nginx 容器化部署 |

---

## 5. 端到端联调与部署

### 5.1 docker-compose 推荐拓扑（学生本机就能跑）

```yaml
version: '3'
services:
  postgres:
    image: postgres:14
    environment: { POSTGRES_PASSWORD: dev, POSTGRES_DB: drop }
    ports: ["5432:5432"]

  minio:
    image: minio/minio
    command: server /data --console-address ":9001"
    environment: { MINIO_ROOT_USER: drop, MINIO_ROOT_PASSWORD: dropdrop }
    ports: ["9000:9000", "9001:9001"]

  drop_server:
    build: ./drop
    command: ./drop_server -port 50051
    ports: ["50051:50051"]

  drop_agent:
    build: ./drop
    command: ./drop_agent -config /etc/drop/config.json
    network_mode: host          # 需要 perf 权限
    privileged: true
    pid: host                   # 能采到主机进程
    cap_add: [SYS_ADMIN, PERFMON]

  apiserver:
    build: ./apiserver
    depends_on: [postgres, minio, drop_server]
    ports: ["8191:8191"]
    environment:
      DROP_GRPC: drop_server:50051
      PG_DSN: "host=postgres user=postgres password=dev dbname=drop sslmode=disable"
      S3_ENDPOINT: minio:9000

  analysis_runner:                # 一次性 Job 容器，apiserver 用 docker exec 触发
    build: ./analysis
    entrypoint: ["python3", "hotmethod_analyzer.py"]

  web_frontend:
    build: ./web_frontend
    ports: ["80:80"]
    environment:
      HOST_URL: "http://localhost:8191"
```

### 5.2 端到端验证清单

- [ ] `docker compose up`，5 个服务都 healthy
- [ ] 浏览器开 `http://localhost`，看到登录页
- [ ] 登录后看到 1 个 Agent（drop_agent 自己）
- [ ] 新建一个 CPU 采样任务，target=自己 IP，pid=`pgrep firefox`，时长 10 秒
- [ ] 30 秒后任务变绿，能下载 `perf.data`
- [ ] 60 秒内 analysis 跑完，火焰图渲染成功
- [ ] AI 建议或规则建议显示在右侧

只要这条链路通了，你就**实质上复刻完成了一个 drop**。

---

## 6. 总评分标准（建议给学生用）

| 维度 | 满分 | 评分点 |
|---|---|---|
| 协议设计 | 15 | 4 个 .proto 完整且向后兼容 |
| C++ Agent | 20 | 守护化、心跳、超时杀进程、自监控、≥2 种采集器 |
| C++ Server | 10 | 任务队列线程安全、状态机完整 |
| Go API | 20 | 12 个 API + 鉴权 + 软删 + 权限校验 |
| 数据库 | 5 | 表结构合理、有索引、AutoMigrate |
| 分析引擎 | 15 | 火焰图 + TopN + 至少 1 种建议 |
| 前端 | 10 | 4 个页面 + 火焰图渲染 + 轮询 |
| 部署 | 5 | docker-compose 一把跑通 |

**总分 70+ 视为合格 MVP；85+ 已超越很多企业内部工具的水准。**

```

### 第 1 周：各模块独立跑通（D1~D7）

| Day | T | A (drop C++) | B (apiserver Go) | C (analysis Py) | D (前端) |
|---|---|---|---|---|---|
| D1 | **全员协议会议**：1 天内把 4 个 .proto 定稿，所有人签字（之后不许改） | proto 编译跑通；CMake 起骨架 | go.mod 起骨架；连 PG；AutoMigrate 4 张表 (user/agent/task/multi) | requirement.txt + storage.py + 命令行入口 | CRA 起骨架 + 路由 + axios |
| D2 | 推 docker-compose 雏形（PG + MinIO） | 心跳 server+agent 互通（hello world） | `/users` `/agents` mock 返回 | 本地能用 perf script + flamegraph.pl 出 svg | 登录回跳 + 主页空壳 |
| D3 | 联调 D2 各人成果 | drop_server 维护 tasks_[ip] 队列；agent 拉到 TaskDesc 打印 | `POST /tasks` 写库 + 打印 TaskDesc（暂不调 grpc） | 命令行接 task_id：从 MinIO 拉 perf.data → 出 svg → 上传 MinIO | 任务列表表格（mock 数据） |
| D4 | 协助 A/B 联调 grpc | agent fork+exec `perf record`；超时杀；waitpid | `POST /tasks` 真调 drop_server.CreateTask | 接 PG：跑完写 analysis_status=2、insert suggestion(空) | 创建任务弹窗 |
| D5 | **第一次端到端 spike**：尝试一次完整链路 | agent 上传 MinIO + NotifyResult 回报 cosKey | task 详情接口返回 cos 签名 URL；新增 trigger analysis 的接口 | 被 apiserver subprocess 调起，跑 30 秒级真实 perf.data | 任务详情页 + iframe 加载 svg |
| D6 | bug 调度 | 自监控 PidStats（/proc 读取）；多线程加锁 | 鉴权 cookie 中间件；权限校验 (uid 过滤) | 顺手加 TopN 热点 JSON | 状态轮询 hook（3s 一次） |
| D7 | **W1 验收**：手动跑通 1 次端到端，火焰图能在浏览器看到 | 优化 perf 参数；写 README | `DELETE /tasks/:tid` + 错误处理 | 顺手加 yaml 规则建议 | 列表筛选 + 分页 |

### 第 2 周：联调、加固、演示（D8~D14）

| Day | T | A | B | C | D |
|---|---|---|---|---|---|
| D8 | 全员一起跑端到端，记 bug list | bug fix；docker 镜像 | bug fix；docker 镜像 | bug fix；docker 镜像 | bug fix；docker 镜像 |
| D9 | 写 docker-compose 完整版 | agent 容器化（privileged + pid:host） | apiserver 容器化 + 健康检查 | analysis 镜像（含 perl + perf） | nginx 容器化 + 配置注入 |
| D10 | 部署到测试机 | 测试 host 模式 perf 权限 | 测试连通 | 跑通容器内 analyzer | 测试 cookie/CORS |
| D11 | **第二次端到端验收**：从 0 起容器到出火焰图 ≤ 5 分钟 | 加 pprof 采集器（如果有余力） | 加任务重试 | 时序图 (resource_analyzer) | TopN 热点表格 |
| D12 | 准备演示脚本 + 数据 | 性能压测：连续 50 个任务 | 压测下 GORM 连接池调优 | 压测火焰图生成耗时 | UI 打磨、加 loading |
| D13 | 写最终文档 + 录视频 | 完善 README + 部署文档 | 完善 API 文档 | 完善 analyzer 文档 | 完善前端 README |
| D14 | **答辩 / 验收** | 现场演示 | 现场演示 | 现场演示 | 现场演示 |

---

## 附录 A：4 个仓库一句话总结（背下来）

- **drop**：C++ 写的 Agent + Server，靠 gRPC 把"perf 采集"远程化。
- **apiserver**：Go 写的 HTTP 编排层，对前端讲 REST，对 drop 讲 gRPC，业务数据进 PG。
- **analysis**：Python 一次性脚本，把 perf.data 变成 SVG 火焰图和优化建议。
- **web_frontend**：React SPA，登录 → 建任务 → 看火焰图。

## 附录 B：里程碑产物清单

完成本指南后，你将拥有：
- 4 个独立但可联调的 git 仓库
- 1 套完整的 protobuf 协议
- 1 份 docker-compose 一键部署脚本
- 1 份完整的端到端 demo 视频（建任务 → 看火焰图 → 看建议）

---

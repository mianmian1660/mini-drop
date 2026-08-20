#include "agent/WorkerThread.h"

#include "agent/Runner.h"
#include "agent/RunnerRegistry.h"
#include "agent/RunnerUtils.h"
#include "agent/AgentUtils.h"
#include "agent/RunnerOutcome.h"
#include "common/Log.h"     // drop::log_event
#include "common/Process.h" // drop::collect_self_pidstats, collect_children_pidstats

#include <iostream>
#include <string>
#include <chrono>
#include <vector>
#include <fstream>
#include <cstdlib>
#include <cstring>
#include <utility>

#include "common/proto/hotmethod.grpc.pb.h"

using namespace std;
using namespace std::chrono;

namespace drop_agent
{

    namespace
    {

        // ============================================================
        // WSL2 Mock 数据生成：当 perf 无法在当前内核运行时，生成模拟折叠栈
        // 使火焰图→分析→Web 展示全链路在 WSL2/容器 环境中可演示
        // ============================================================
        void GenerateMockCollapsedStacks(const string &outputPath)
        {
            const char *mockData =
                "main;run_event_loop;process_request;parse_json;json_parse_string 42\n"
                "main;run_event_loop;process_request;handle_query;sql_execute;btree_search 38\n"
                "main;run_event_loop;process_request;handle_query;sql_execute;index_scan 25\n"
                "main;run_event_loop;accept_connection;tcp_handshake 15\n"
                "main;run_event_loop;process_request;serialize_response;json_encode 12\n"
                "main;run_event_loop;process_request;auth_check;verify_token 10\n"
                "main;worker_thread;compress_log;gzip_compress;[kernel] 8\n"
                "main;worker_thread;flush_log;write;[kernel]sys_write 7\n"
                "main;gc_thread;mark_phase;scan_roots 6\n"
                "main;gc_thread;sweep_phase;free_pages 5\n"
                "main;run_event_loop;timer_callback;update_metrics 4\n"
                "main;run_event_loop;process_request;rate_limit;token_bucket 3\n"
                "main;worker_thread;mem_alloc;[kernel]page_fault 3\n"
                "idle;[kernel]cpu_idle 2\n";

            ofstream out(outputPath);
            if (out.is_open())
            {
                out << mockData;
                out.close();
                cout << "[agent] ⚠️ perf 不可用，已生成 WSL2 模拟数据: " << outputPath
                     << " (" << strlen(mockData) << " bytes)" << endl;
            }
            else
            {
                cerr << "[agent] 无法创建模拟数据文件: " << outputPath << endl;
            }
        }

        // ============================================================
        // run_task_lifecycle — 用 drop_agent::Runner 的
        // Validate->Prepare->Start->(Poll循环)->Collect 生命周期跑一次采集。
        //
        // runningFlag 是 Agent 的全局运行标志：Poll 循环里每一轮都会检查，
        // 一旦 Agent 收到关闭信号（SIGTERM/SIGINT），就调用 Runner::Stop
        // (StopReason::kAgentShutdown) 中断正在跑的采集进程，而不是傻等它
        // 自然结束或超时——否则 WorkerThread::Stop() 的 join() 会被一个
        // 几十秒的任务拖住，SIGTERM 就成了摆设。
        // ============================================================
        RunnerOutcome RunTaskLifecycle(uint32_t profilerType, const hotmethod::TaskDesc &task,
                                        const string &outputPath,
                                        const std::atomic<bool> &runningFlag,
                                        drop::PidRegistry &pidRegistry,
                                        AttemptTracker &attemptTracker)
        {
            RunnerOutcome out;
            out.outputPath = outputPath;

            cout << "[runner] stage=Validate taskID=" << task.taskid()
                 << " attemptID=" << task.attempt_id() << endl;
            if (task.taskid().empty())
            {
                out.resultCode = -1;
                out.profilerName = "unknown";
                return out;
            }
            if (task.deadline_unix_ms() > 0)
            {
                int64_t nowMs = duration_cast<milliseconds>(system_clock::now().time_since_epoch()).count();
                if (task.deadline_unix_ms() <= nowMs)
                {
                    out.resultCode = -3;
                    out.profilerName = "deadline";
                    return out;
                }
            }

            string profilerLabel;
            switch (profilerType)
            {
            case 0:
                profilerLabel = "perf";
                cout << "[agent] 选择采集器: perf (profilerType=0)" << endl;
                break;
            case 1:
                profilerLabel = "async-profiler";
                cout << "[agent] 选择采集器: async-profiler (profilerType=1)" << endl;
                break;
            case 2:
                profilerLabel = "pprof";
                cout << "[agent] 选择采集器: pprof (profilerType=2)" << endl;
                break;
            case 3:
                profilerLabel = "eBPF";
                cout << "[agent] 选择采集器: eBPF/bpftrace (profilerType=3)" << endl;
                break;
            default:
                profilerLabel = "perf(回退)";
                cerr << "[agent] 未知的 profilerType=" << profilerType << "，回退到 perf" << endl;
                break;
            }

            static drop::RealProcessExecutor s_executor;
            static drop::RealClock s_clock;
            static drop::MinioObjectStore s_objectStore;
            static drop::RealLogger s_logger;

            drop_agent::TaskContext ctx;
            ctx.task = task;
            ctx.taskDir = outputPath;
            ctx.executor = &s_executor;
            ctx.clock = &s_clock;
            ctx.objectStore = &s_objectStore;
            ctx.logger = &s_logger;
            ctx.pidRegistry = &pidRegistry;

            auto runner = drop_agent::CreateRunner(profilerType);

            cout << "[runner] stage=Prepare taskID=" << task.taskid() << endl;
            int resultCode = 0;
            auto validation = runner->Validate(ctx);
            if (!validation.ok)
            {
                resultCode = validation.resultCode;
            }
            else
            {
                auto prepare = runner->Prepare(ctx);
                if (!prepare.ok)
                {
                    resultCode = prepare.resultCode;
                }
                else
                {
                    cout << "[runner] stage=Start taskID=" << task.taskid() << endl;
                    auto start = runner->Start(ctx);
                    if (!start.ok)
                    {
                        resultCode = start.resultCode;
                    }
                    else
                    {
                        bool canceled = false;
                        while (true)
                        {
                            auto poll = runner->Poll(ctx);
                            if (poll.status != drop_agent::PollStatus::kRunning)
                            {
                                resultCode = poll.resultCode;
                                break;
                            }
                            if (!runningFlag)
                            {
                                cout << "[runner] stage=Stop taskID=" << task.taskid()
                                     << " reason=AgentShutdown，中断采集" << endl;
                                runner->Stop(ctx, drop_agent::StopReason::kAgentShutdown);
                                // 再 Poll 一次，让 Runner 观察到进程已被终止并
                                // 更新内部状态，这样下面的 Collect() 才能拿到
                                // 正确的 resultCode，而不是残留的"仍在运行"状态。
                                auto afterStop = runner->Poll(ctx);
                                resultCode = afterStop.resultCode;
                                break;
                            }
                            if (attemptTracker.IsCancelRequested(task.taskid(), task.attempt_id()))
                            {
                                cout << "[runner] stage=Stop taskID=" << task.taskid()
                                     << " reason=Cancel，中断采集" << endl;
                                runner->Stop(ctx, drop_agent::StopReason::kCancel);
                                auto afterStop = runner->Poll(ctx);
                                resultCode = afterStop.resultCode;
                                canceled = true;
                                break;
                            }
                            this_thread::sleep_for(milliseconds(200));
                        }
                        cout << "[runner] stage=Monitor taskID=" << task.taskid()
                             << " resultCode=" << resultCode << endl;
                        auto collect = runner->Collect(ctx);
                        resultCode = collect.resultCode; // Collect() 可能在文件校验/后处理阶段进一步改判定(如 -6/-2)
                        // 主动取消时统一报 -7(TASK_CANCELED)，不用 Collect() 给出的
                        // 信号终止码——那个码含糊(超时/关闭/取消都可能落到同一个
                        // -5)，调用方需要能明确区分"这是用户/Server 主动取消的"。
                        if (canceled)
                            resultCode = -7;
                    }
                }
            }

            string profilerName = profilerLabel;
            if (resultCode != 0)
            {
                if (!EnvEnabled("DROP_ALLOW_EBPF_MOCK"))
                {
                    cout << "[agent] " << profilerLabel << " 采集失败(result=" << resultCode
                         << ")，默认不生成 mock。如仅本地开发要看页面链路，"
                         << "可设置 DROP_ALLOW_EBPF_MOCK=1。" << endl;
                }
                else if (profilerType == 3)
                {
                    cout << "[agent] eBPF 不可用，DROP_ALLOW_EBPF_MOCK=1，启用 mock 模式" << endl;
                    string bpfPath = outputPath + ".bpf";
                    if (task.sampleargv().event() == "io" || task.sampleargv().event() == "blk")
                    {
                        ofstream mockIO(bpfPath);
                        mockIO << "# Mini-Drop eBPF IO Latency (MOCK)\n";
                        mockIO << "@io_lat_us:\n";
                        mockIO << "[1, 2)        42 |@@@@@@@@@\n";
                        mockIO << "[2, 4)        88 |@@@@@@@@@@@@@@@@@@@\n";
                        mockIO << "[4, 8)       156 |@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@\n";
                        mockIO << "[8, 16)      230 |@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@\n";
                        mockIO << "[16, 32)      89 |@@@@@@@@@@@@@@@@@@@@\n";
                        mockIO << "[32, 64)      45 |@@@@@@@@@@\n";
                        mockIO << "[64, 128)     12 |@@\n";
                        mockIO << "[128, 256)     3 |@\n";
                        mockIO << "# Total IO: 665\n";
                        mockIO.close();
                        resultCode = 0;
                        profilerName = "eBPF(mock-io)";
                    }
                    else if (task.sampleargv().event() == "sched" || task.sampleargv().event() == "schedule")
                    {
                        ofstream mockSched(bpfPath);
                        mockSched << "# Mini-Drop eBPF Scheduler Latency (MOCK)\n";
                        mockSched << "@sched_lat_us:\n";
                        mockSched << "[0, 10)      520 |@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@\n";
                        mockSched << "[10, 50)     210 |@@@@@@@@@@@@@@@@@@@@\n";
                        mockSched << "[50, 100)     68 |@@@@@@\n";
                        mockSched << "[100, 250)    24 |@@\n";
                        mockSched << "[250, 500)     7 |@\n";
                        mockSched << "[500, 1K)      2 |\n";
                        mockSched << "# Total Scheduler Wakeups: 831\n";
                        mockSched.close();
                        resultCode = 0;
                        profilerName = "eBPF(mock-sched)";
                    }
                    else
                    {
                        GenerateMockCollapsedStacks(outputPath);
                        resultCode = 0;
                        profilerName = "eBPF(mock)";
                    }
                }
                else
                {
                    cout << "[agent] " << profilerLabel << " 不可用，DROP_ALLOW_EBPF_MOCK=1，启用 mock 模式" << endl;
                    GenerateMockCollapsedStacks(outputPath);
                    resultCode = 0;
                    profilerName = profilerLabel + "(mock)";
                }
            }

            out.resultCode = resultCode;
            out.profilerName = profilerName;
            out.outputPath = drop_agent::ResolveOutputPath(outputPath);
            out.partial = out.resultCode != 0 && FileExists(out.outputPath);
            cout << "[runner] stage=Collect taskID=" << task.taskid()
                 << " outputPath=" << out.outputPath
                 << " partial=" << (out.partial ? "true" : "false") << endl;
            out.remoteKey = task.taskid() + "/" + drop_agent::RemoteKeyFor(profilerName);
            out.contentType = drop_agent::ContentTypeFor(profilerName);
            return out;
        }

    } // namespace

    WorkerThread::WorkerThread(TaskQueue &taskQueue,
                               UploadQueue &uploadQueue,
                               drop::PidRegistry &pidRegistry,
                               AttemptTracker &attemptTracker,
                               std::atomic<bool> &runningFlag)
        : taskQueue_(taskQueue),
          uploadQueue_(uploadQueue),
          pidRegistry_(pidRegistry),
          attemptTracker_(attemptTracker),
          running_(runningFlag)
    {
    }

    void WorkerThread::Start()
    {
        thread_ = std::thread(&WorkerThread::Loop, this);
    }

    void WorkerThread::Stop()
    {
        if (thread_.joinable())
            thread_.join();
    }

    void WorkerThread::Loop()
    {
        while (running_)
        {
            hotmethod::TaskDesc task;
            if (!taskQueue_.WaitPop(200, &task))
                continue;

            uint32_t ptype = task.profilertype();
            cout << "[worker] 开始执行任务! taskID=" << task.taskid()
                 << " taskKind=" << task.task_kind()
                 << " requestID=" << task.request_id()
                 << " attemptID=" << task.attempt_id()
                 << " deadlineUnixMs=" << task.deadline_unix_ms()
                 << " profilerType=" << ptype
                 << " pid=" << task.sampleargv().pid()
                 << " hz=" << task.sampleargv().hz()
                 << " duration=" << task.sampleargv().duration() << endl;

            // Phase 5：统一任务目录隔离，每个 (taskID, attemptID) 各有独立目录，
            // 不再靠文件名拼接 profilerType+taskID 去重（同一 taskID 的重试
            // attempt 用子目录区分，不会互相覆盖产物）。
            string taskDir = TaskAttemptDir("/tmp/drop_agent/tasks", task.taskid(), task.attempt_id());
            EnsureDirRecursive(taskDir);
            string outputPath = taskDir + "output";

            auto collectStart = steady_clock::now();
            RunnerOutcome outcome = RunTaskLifecycle(ptype, task, outputPath, running_, pidRegistry_, attemptTracker_);
            auto collectMs = duration_cast<milliseconds>(steady_clock::now() - collectStart).count();
            drop::log_event("drop_agent",
                             outcome.resultCode == 0 ? "collection_succeeded" : "collection_failed",
                             {{"task_id", task.taskid()},
                              {"profiler", outcome.profilerName},
                              {"result_code", to_string(outcome.resultCode)},
                              {"duration_ms", to_string(collectMs)}});

            // 采集刚结束就采样一次自身/子进程 PidStats——比在 UploadWorker
            // 里再采更准确（那时采集进程可能已退出，子进程列表会是空的）。
            UploadJob job;
            job.task = task;
            job.outcome = outcome;
            job.selfPs = drop::collect_self_pidstats();
            job.childrenPs = drop::collect_children_pidstats();
            uploadQueue_.Push(std::move(job));
        }
    }

} // namespace drop_agent

// ============================================================
// agent/RunnerOutcome.h — 一次采集的结果（WorkerThread 产出，UploadQueue 消费）
// ============================================================
// Phase 3 时是 WorkerThread.cpp 匿名命名空间里的私有结构体；Phase 4 把
// 上传/NotifyResult 拆到 UploadWorker 后，两个线程都要用到这个类型，
// 所以提出来做共享头文件。
// ============================================================

#pragma once

#include <string>

namespace drop_agent
{

    struct RunnerOutcome
    {
        int resultCode = 0;
        std::string profilerName;
        std::string outputPath;
        std::string remoteKey;
        std::string contentType;
        bool partial = false;
    };

} // namespace drop_agent

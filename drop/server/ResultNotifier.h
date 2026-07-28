// ============================================================
// server/ResultNotifier.h — 结果回调通知 声明（A3）
// ============================================================

#pragma once

#include "common/proto/hotmethod.pb.h"

namespace drop_server
{

    /// 把任务结果通过 HTTP POST 推送给 apiserver。
    /// 这是"锦上添花"的快速通知，不是唯一的完成信号来源——
    /// apiserver 自己的轮询仍会兜底发现任务完成，通知失败不影响主流程。
    /// 返回 true 表示 HTTP 请求成功发出并收到 2xx。
    bool notify_apiserver_task_result(const hotmethod::TaskResult &result);

} // namespace drop_server

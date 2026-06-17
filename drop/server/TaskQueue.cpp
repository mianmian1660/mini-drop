// ============================================================
// server/TaskQueue.cpp — 任务队列 全局变量
// ============================================================

#include "server/TaskQueue.h"

namespace drop_server
{

    std::mutex tasks_mutex;
    std::unordered_map<std::string, std::queue<hotmethod::TaskDesc>> tasks_;

} // namespace drop_server

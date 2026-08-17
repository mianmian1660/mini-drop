// ============================================================
// common/LoggerIface.h — 结构化日志的接口包装
// ============================================================
// 指南 5.8 节要求 TaskContext 暴露 Logger。命名故意避开 Log.h 已有的
// 自由函数 drop::log_event，避免和现有轻量日志机制混淆——RealLogger
// 只是它的一层接口包装，方便依赖注入/单元测试用 FakeLogger 断言
// Runner 打了哪些事件，不用真的写 stderr。
// ============================================================

#pragma once

#include <string>
#include <utility>
#include <vector>

namespace drop
{

    class Logger
    {
    public:
        virtual ~Logger() = default;
        virtual void Event(const std::string &event,
                            const std::vector<std::pair<std::string, std::string>> &fields = {}) = 0;
    };

    /// 生产实现：转发给 drop::log_event("drop_agent", event, fields)。
    class RealLogger : public Logger
    {
    public:
        void Event(const std::string &event,
                    const std::vector<std::pair<std::string, std::string>> &fields) override;
    };

} // namespace drop

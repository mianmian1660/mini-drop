// ============================================================
// common/Utils.h — 通用工具函数声明
// ============================================================
// 提供 trim、shell_exec、read_file_content 等基础工具
// ============================================================

#pragma once

#include <string>

namespace drop
{

    /// 去除字符串两端的空白字符
    std::string trim(const std::string &s);

    /// 执行 shell 命令并返回标准输出（最多 maxLen 字节）
    std::string shell_exec(const std::string &cmd, size_t maxLen = 4096);

    /// 读取文件全部内容（二进制安全）
    std::string read_file_content(const std::string &path);

} // namespace drop

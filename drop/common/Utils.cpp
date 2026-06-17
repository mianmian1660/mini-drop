// ============================================================
// common/Utils.cpp — 通用工具函数实现
// ============================================================

#include "common/Utils.h"
#include <cstdio>  // popen, pclose
#include <fstream> // ifstream
#include <ios>     // ios::binary, ios::ate
#include <cctype>  // isspace

namespace drop
{

    std::string trim(const std::string &s)
    {
        size_t start = 0;
        while (start < s.size() && std::isspace((unsigned char)s[start]))
            start++;
        size_t end = s.size();
        while (end > start && std::isspace((unsigned char)s[end - 1]))
            end--;
        return s.substr(start, end - start);
    }

    std::string shell_exec(const std::string &cmd, size_t maxLen)
    {
        FILE *fp = popen(cmd.c_str(), "r");
        if (!fp)
            return "";
        std::string result(maxLen, '\0');
        size_t n = fread(&result[0], 1, maxLen - 1, fp);
        pclose(fp);
        result.resize(n);
        return result;
    }

    std::string read_file_content(const std::string &path)
    {
        std::ifstream file(path, std::ios::binary | std::ios::ate);
        if (!file.is_open())
            return "";
        std::streamsize size = file.tellg();
        file.seekg(0, std::ios::beg);
        std::string content(size, '\0');
        file.read(&content[0], size);
        return content;
    }

} // namespace drop

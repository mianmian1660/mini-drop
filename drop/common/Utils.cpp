// ============================================================
// common/Utils.cpp — 通用工具函数实现
// ============================================================

#include "common/Utils.h"
#include <cstdio>  // popen, pclose
#include <fstream> // ifstream
#include <ios>     // ios::binary, ios::ate
#include <cctype>  // isspace
#include <unistd.h>
#include <sys/wait.h>

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

    int exec_capture(const std::vector<std::string> &argv, std::string *output, size_t maxLen)
    {
        if (output)
            output->clear();
        if (argv.empty())
            return 127;

        int pipefd[2];
        if (pipe(pipefd) != 0)
            return 127;

        pid_t pid = fork();
        if (pid < 0)
        {
            close(pipefd[0]);
            close(pipefd[1]);
            return 127;
        }

        if (pid == 0)
        {
            close(pipefd[0]);
            dup2(pipefd[1], STDOUT_FILENO);
            dup2(pipefd[1], STDERR_FILENO);
            close(pipefd[1]);

            std::vector<char *> args;
            args.reserve(argv.size() + 1);
            for (const auto &arg : argv)
                args.push_back(const_cast<char *>(arg.c_str()));
            args.push_back(nullptr);
            execvp(args[0], args.data());
            _exit(127);
        }

        close(pipefd[1]);
        char buf[512];
        size_t total = 0;
        while (true)
        {
            ssize_t n = read(pipefd[0], buf, sizeof(buf));
            if (n <= 0)
                break;
            if (output && total < maxLen)
            {
                size_t take = static_cast<size_t>(n);
                if (total + take > maxLen)
                    take = maxLen - total;
                output->append(buf, take);
                total += take;
            }
        }
        close(pipefd[0]);

        int status = 0;
        waitpid(pid, &status, 0);
        if (WIFEXITED(status))
            return WEXITSTATUS(status);
        if (WIFSIGNALED(status))
            return 128 + WTERMSIG(status);
        return 127;
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

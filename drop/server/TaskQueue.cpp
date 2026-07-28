// ============================================================
// server/TaskQueue.cpp — 任务队列 全局变量 + 快照持久化（A3）
// ============================================================
// 快照格式：连续记录 [ip_len(4B)][ip][task_len(4B)][TaskDesc protobuf 二进制]，
// 直到文件结尾。写入时先写临时文件再 rename 替换，避免进程被杀时留下半截文件。
// ============================================================

#include "server/TaskQueue.h"

#include <cstdint>
#include <cstdio> // rename
#include <fstream>
#include <iostream>

using namespace std;

namespace drop_server
{

    std::mutex tasks_mutex;
    std::unordered_map<std::string, std::queue<hotmethod::TaskDesc>> tasks_;

    void snapshot_tasks_to_disk(const string &path)
    {
        string tmpPath = path + ".tmp";
        ofstream out(tmpPath, ios::binary | ios::trunc);
        if (!out.is_open())
        {
            cerr << "[queue] 无法写快照文件: " << tmpPath << endl;
            return;
        }

        {
            lock_guard<mutex> lock(tasks_mutex);
            for (auto &pair : tasks_)
            {
                const string &ip = pair.first;
                queue<hotmethod::TaskDesc> copy = pair.second; // 拷贝一份用于遍历，不动原队列
                while (!copy.empty())
                {
                    const hotmethod::TaskDesc &task = copy.front();
                    string taskBytes;
                    task.SerializeToString(&taskBytes);

                    uint32_t ipLen = static_cast<uint32_t>(ip.size());
                    uint32_t taskLen = static_cast<uint32_t>(taskBytes.size());
                    out.write(reinterpret_cast<const char *>(&ipLen), sizeof(ipLen));
                    out.write(ip.data(), ipLen);
                    out.write(reinterpret_cast<const char *>(&taskLen), sizeof(taskLen));
                    out.write(taskBytes.data(), taskLen);

                    copy.pop();
                }
            }
        }
        out.close();

        if (rename(tmpPath.c_str(), path.c_str()) != 0)
        {
            cerr << "[queue] 快照文件替换失败: " << path << endl;
        }
    }

    void restore_tasks_from_disk(const string &path)
    {
        ifstream in(path, ios::binary);
        if (!in.is_open())
        {
            cout << "[queue] 没有找到快照文件，跳过恢复: " << path << endl;
            return;
        }

        int restored = 0;
        while (true)
        {
            uint32_t ipLen = 0;
            in.read(reinterpret_cast<char *>(&ipLen), sizeof(ipLen));
            if (in.eof())
                break;
            if (!in || ipLen == 0 || ipLen > 1024)
                break; // 记录损坏，停止解析，已恢复的部分仍然有效

            string ip(ipLen, '\0');
            in.read(&ip[0], ipLen);

            uint32_t taskLen = 0;
            in.read(reinterpret_cast<char *>(&taskLen), sizeof(taskLen));
            if (!in || taskLen == 0 || taskLen > 10 * 1024 * 1024)
                break;

            string taskBytes(taskLen, '\0');
            in.read(&taskBytes[0], taskLen);
            if (!in)
                break;

            hotmethod::TaskDesc task;
            if (!task.ParseFromString(taskBytes))
            {
                cerr << "[queue] 快照记录解析失败，跳过该条" << endl;
                continue;
            }

            {
                lock_guard<mutex> lock(tasks_mutex);
                tasks_[ip].push(task);
            }
            restored++;
        }

        if (restored > 0)
            cout << "[queue] 从快照恢复了 " << restored << " 个未派发任务" << endl;
        else
            cout << "[queue] 快照文件存在但没有未派发任务" << endl;
    }

} // namespace drop_server

// ============================================================
// server/TaskQueue.cpp — 任务队列 全局变量 + 快照持久化（A3）
// ============================================================
// 快照格式：连续记录 [ip_len(4B)][ip][task_len(4B)][TaskDesc protobuf 二进制]
// [priority(8B)][deadline(8B)][enqueued(8B)][canceled(1B)]，直到文件结尾。
// 写入时先写临时文件再 rename 替换，避免进程被杀时留下半截文件。
// ============================================================

#include "server/TaskQueue.h"

#include <algorithm>
#include <chrono>
#include <cstdint>
#include <cstdio> // rename
#include <fstream>
#include <iostream>

using namespace std;

namespace drop_server
{

    std::mutex tasks_mutex;
    std::unordered_map<std::string, std::deque<QueuedTask>> tasks_;
    std::unordered_set<std::string> completed_attempts_;

    namespace
    {
        const size_t MAX_QUEUE_LENGTH_PER_AGENT = 1000;

        int64_t now_unix_ms()
        {
            return chrono::duration_cast<chrono::milliseconds>(
                       chrono::system_clock::now().time_since_epoch())
                .count();
        }

        int infer_priority(const hotmethod::TaskDesc &task)
        {
            if (task.resource_budget().find("\"priority\"") != string::npos)
                return 10;
            return 0;
        }

        void write_i64(ofstream &out, int64_t v)
        {
            out.write(reinterpret_cast<const char *>(&v), sizeof(v));
        }

        bool read_i64(ifstream &in, int64_t *v)
        {
            in.read(reinterpret_cast<char *>(v), sizeof(*v));
            return static_cast<bool>(in);
        }
    }

    string task_attempt_key(const hotmethod::TaskDesc &task)
    {
        return task.taskid() + "#" + to_string(task.attempt_id());
    }

    bool is_attempt_completed_locked(const hotmethod::TaskDesc &task)
    {
        return completed_attempts_.find(task_attempt_key(task)) != completed_attempts_.end();
    }

    void mark_attempt_completed_locked(const hotmethod::TaskResult &result)
    {
        completed_attempts_.insert(result.taskid() + "#" + to_string(result.attempt_id()));
    }

    EnqueueResult enqueue_task_locked(const string &targetIP, const hotmethod::TaskDesc &task)
    {
        EnqueueResult result;
        if (task.taskid().empty())
        {
            result.message = "task_id is required";
            return result;
        }
        if (task.deadline_unix_ms() > 0 && task.deadline_unix_ms() <= now_unix_ms())
        {
            result.message = "task deadline has expired";
            return result;
        }
        if (is_attempt_completed_locked(task))
        {
            result.ok = true;
            result.duplicate = true;
            result.message = "attempt already completed";
            return result;
        }

        auto &queue = tasks_[targetIP];
        string key = task_attempt_key(task);
        for (const auto &item : queue)
        {
            if (task_attempt_key(item.task) == key)
            {
                result.ok = true;
                result.duplicate = true;
                result.message = "duplicate queued attempt";
                return result;
            }
        }
        if (queue.size() >= MAX_QUEUE_LENGTH_PER_AGENT)
        {
            result.message = "agent queue is full";
            return result;
        }

        QueuedTask item;
        item.task.CopyFrom(task);
        item.priority = infer_priority(task);
        item.deadline_unix_ms = task.deadline_unix_ms();
        item.enqueued_unix_ms = now_unix_ms();
        queue.push_back(item);
        stable_sort(queue.begin(), queue.end(), [](const QueuedTask &a, const QueuedTask &b) {
            if (a.priority != b.priority)
                return a.priority > b.priority;
            if (a.deadline_unix_ms != b.deadline_unix_ms)
            {
                if (a.deadline_unix_ms == 0)
                    return false;
                if (b.deadline_unix_ms == 0)
                    return true;
                return a.deadline_unix_ms < b.deadline_unix_ms;
            }
            return a.enqueued_unix_ms < b.enqueued_unix_ms;
        });
        result.ok = true;
        result.message = "ok";
        return result;
    }

    bool cancel_task_locked(const string &targetIP, const string &taskID, uint64_t attemptID)
    {
        bool found = false;
        auto applyCancel = [&](deque<QueuedTask> &queue) {
            for (auto &item : queue)
            {
                if (item.task.taskid() != taskID)
                    continue;
                if (attemptID != 0 && item.task.attempt_id() != attemptID)
                    continue;
                item.canceled = true;
                found = true;
            }
        };
        if (!targetIP.empty())
        {
            auto it = tasks_.find(targetIP);
            if (it != tasks_.end())
                applyCancel(it->second);
            return found;
        }
        for (auto &pair : tasks_)
            applyCancel(pair.second);
        return found;
    }

    bool pop_next_dispatchable_task_locked(const string &targetIP, hotmethod::TaskDesc *task)
    {
        auto it = tasks_.find(targetIP);
        if (it == tasks_.end())
            return false;
        auto &queue = it->second;
        int64_t now = now_unix_ms();
        while (!queue.empty())
        {
            QueuedTask item = queue.front();
            queue.pop_front();
            if (item.canceled)
            {
                cout << "[queue] 跳过已取消任务: taskID=" << item.task.taskid()
                     << " attemptID=" << item.task.attempt_id() << endl;
                continue;
            }
            if (item.deadline_unix_ms > 0 && item.deadline_unix_ms <= now)
            {
                cout << "[queue] 跳过过期任务: taskID=" << item.task.taskid()
                     << " attemptID=" << item.task.attempt_id() << endl;
                continue;
            }
            if (is_attempt_completed_locked(item.task))
            {
                cout << "[queue] 跳过已完成 attempt: taskID=" << item.task.taskid()
                     << " attemptID=" << item.task.attempt_id() << endl;
                continue;
            }
            task->CopyFrom(item.task);
            if (queue.empty())
                tasks_.erase(it);
            return true;
        }
        tasks_.erase(it);
        return false;
    }

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
                deque<QueuedTask> copy = pair.second; // 拷贝一份用于遍历，不动原队列
                for (const auto &item : copy)
                {
                    const hotmethod::TaskDesc &task = item.task;
                    string taskBytes;
                    task.SerializeToString(&taskBytes);

                    uint32_t ipLen = static_cast<uint32_t>(ip.size());
                    uint32_t taskLen = static_cast<uint32_t>(taskBytes.size());
                    out.write(reinterpret_cast<const char *>(&ipLen), sizeof(ipLen));
                    out.write(ip.data(), ipLen);
                    out.write(reinterpret_cast<const char *>(&taskLen), sizeof(taskLen));
                    out.write(taskBytes.data(), taskLen);
                    write_i64(out, item.priority);
                    write_i64(out, item.deadline_unix_ms);
                    write_i64(out, item.enqueued_unix_ms);
                    uint8_t canceled = item.canceled ? 1 : 0;
                    out.write(reinterpret_cast<const char *>(&canceled), sizeof(canceled));
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

            QueuedTask item;
            if (!item.task.ParseFromString(taskBytes))
            {
                cerr << "[queue] 快照记录解析失败，跳过该条" << endl;
                continue;
            }

            streampos beforeMeta = in.tellg();
            uint8_t canceled = 0;
            int64_t priority = 0;
            if (!read_i64(in, &priority))
            {
                in.clear();
                in.seekg(beforeMeta);
            }
            else if (!read_i64(in, &item.deadline_unix_ms))
            {
                in.clear();
                in.seekg(beforeMeta);
            }
            else if (!read_i64(in, &item.enqueued_unix_ms))
            {
                in.clear();
                in.seekg(beforeMeta);
            }
            else
            {
                in.read(reinterpret_cast<char *>(&canceled), sizeof(canceled));
                if (!in)
                {
                    in.clear();
                    in.seekg(beforeMeta);
                }
                item.priority = static_cast<int>(priority);
                item.canceled = canceled != 0;
            }
            if (item.enqueued_unix_ms == 0)
                item.enqueued_unix_ms = now_unix_ms();
            if (item.deadline_unix_ms == 0)
                item.deadline_unix_ms = item.task.deadline_unix_ms();
            if (item.priority == 0)
                item.priority = infer_priority(item.task);

            {
                lock_guard<mutex> lock(tasks_mutex);
                (void)enqueue_task_locked(ip, item.task);
            }
            restored++;
        }

        if (restored > 0)
            cout << "[queue] 从快照恢复了 " << restored << " 个未派发任务" << endl;
        else
            cout << "[queue] 快照文件存在但没有未派发任务" << endl;
    }

} // namespace drop_server

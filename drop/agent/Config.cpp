// ============================================================
// agent/Config.cpp — Agent 配置文件读取 实现
// ============================================================

#include "agent/Config.h"

#include <iostream>
#include <fstream>
#include <sstream>
#include <string>
#include <vector>

using namespace std;

namespace drop_agent
{

    // 简易 JSON 解析器（不引入第三方库，仅支持 agent_config.json 格式）
    // 支持格式：{"key": "value", "key2": ["v1","v2"], "key3": 123}
    static string json_get_string(const string &json, const string &key)
    {
        string searchKey = "\"" + key + "\"";
        size_t pos = json.find(searchKey);
        if (pos == string::npos)
            return "";
        pos = json.find(':', pos + searchKey.size());
        if (pos == string::npos)
            return "";
        // 跳过冒号和空白
        pos++;
        while (pos < json.size() && (json[pos] == ' ' || json[pos] == '\t' || json[pos] == '\n'))
            pos++;
        if (pos >= json.size())
            return "";
        // 字符串值
        if (json[pos] == '"')
        {
            pos++;
            size_t end = json.find('"', pos);
            if (end == string::npos)
                return "";
            return json.substr(pos, end - pos);
        }
        // 数字值
        if (json[pos] >= '0' && json[pos] <= '9')
        {
            size_t end = pos;
            while (end < json.size() && json[end] >= '0' && json[end] <= '9')
                end++;
            return json.substr(pos, end - pos);
        }
        return "";
    }

    static vector<string> json_get_string_array(const string &json, const string &key)
    {
        vector<string> result;
        string searchKey = "\"" + key + "\"";
        size_t pos = json.find(searchKey);
        if (pos == string::npos)
            return result;
        pos = json.find('[', pos + searchKey.size());
        if (pos == string::npos)
            return result;
        size_t end = json.find(']', pos);
        if (end == string::npos)
            return result;
        string arrContent = json.substr(pos + 1, end - pos - 1);
        // 解析引号内的字符串
        size_t i = 0;
        while (i < arrContent.size())
        {
            if (arrContent[i] == '"')
            {
                i++;
                size_t strEnd = arrContent.find('"', i);
                if (strEnd == string::npos)
                    break;
                result.push_back(arrContent.substr(i, strEnd - i));
                i = strEnd + 1;
            }
            else
            {
                i++;
            }
        }
        return result;
    }

    static string read_entire_file(const string &path)
    {
        ifstream f(path);
        if (!f.is_open())
            return "";
        stringstream buf;
        buf << f.rdbuf();
        return buf.str();
    }

    AgentConfig AgentConfig::LoadFromFile(const string &configPath)
    {
        AgentConfig cfg;
        string content = read_entire_file(configPath);
        if (content.empty())
        {
            cerr << "[config] 无法读取配置文件: " << configPath << "，使用默认配置" << endl;
            return Default();
        }

        cfg.hostname = json_get_string(content, "hostname");
        cfg.uid = json_get_string(content, "uid");
        cfg.agentVersion = json_get_string(content, "agentVersion");
        cfg.serverAddrs = json_get_string_array(content, "serverAddrs");

        string interval = json_get_string(content, "heartbeatIntervalSec");
        if (!interval.empty())
            cfg.heartbeatIntervalSec = (uint32_t)stoul(interval);

        string regTimeout = json_get_string(content, "registerTimeoutSec");
        if (!regTimeout.empty())
            cfg.registerTimeoutSec = (uint32_t)stoul(regTimeout);

        // 默认值填充
        if (cfg.hostname.empty())
            cfg.hostname = "demo-host";
        if (cfg.uid.empty())
            cfg.uid = "agent-001";
        if (cfg.agentVersion.empty())
            cfg.agentVersion = "1.0.0";
        if (cfg.heartbeatIntervalSec == 0)
            cfg.heartbeatIntervalSec = 5;
        if (cfg.registerTimeoutSec == 0)
            cfg.registerTimeoutSec = 10;

        cout << "[config] 加载配置文件: " << configPath << endl;
        cout << "[config]   hostname=" << cfg.hostname << endl;
        cout << "[config]   uid=" << cfg.uid << endl;
        cout << "[config]   version=" << cfg.agentVersion << endl;
        cout << "[config]   serverAddrs=" << cfg.serverAddrs.size() << " 个" << endl;
        for (const auto &addr : cfg.serverAddrs)
            cout << "[config]     - " << addr << endl;

        return cfg;
    }

    AgentConfig AgentConfig::Default(const string &serverAddr)
    {
        AgentConfig cfg;
        cfg.hostname = "demo-host";
        cfg.uid = "agent-001";
        cfg.agentVersion = "1.0.0";
        cfg.serverAddrs.push_back(serverAddr);
        cfg.heartbeatIntervalSec = 5;
        cfg.registerTimeoutSec = 10;
        return cfg;
    }

} // namespace drop_agent

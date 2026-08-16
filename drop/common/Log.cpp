// ============================================================
// common/Log.cpp — 轻量结构化日志（JSON 行）实现
// ============================================================

#include "common/Log.h"

#include <chrono>
#include <cstdio>
#include <ctime>
#include <iostream>
#include <sstream>

namespace drop
{

    namespace
    {
        std::string json_escape(const std::string &s)
        {
            std::string out;
            out.reserve(s.size() + 8);
            for (char c : s)
            {
                switch (c)
                {
                case '"':
                    out += "\\\"";
                    break;
                case '\\':
                    out += "\\\\";
                    break;
                case '\n':
                    out += "\\n";
                    break;
                case '\r':
                    out += "\\r";
                    break;
                case '\t':
                    out += "\\t";
                    break;
                default:
                    if (static_cast<unsigned char>(c) < 0x20)
                    {
                        char buf[8];
                        snprintf(buf, sizeof(buf), "\\u%04x", static_cast<unsigned char>(c));
                        out += buf;
                    }
                    else
                    {
                        out += c;
                    }
                }
            }
            return out;
        }

        std::string now_iso8601()
        {
            using namespace std::chrono;
            auto now = system_clock::now();
            auto ms = duration_cast<milliseconds>(now.time_since_epoch()).count() % 1000;
            time_t t = system_clock::to_time_t(now);
            tm utc{};
            gmtime_r(&t, &utc);
            char buf[32];
            snprintf(buf, sizeof(buf), "%04d-%02d-%02dT%02d:%02d:%02d.%03dZ",
                     utc.tm_year + 1900, utc.tm_mon + 1, utc.tm_mday,
                     utc.tm_hour, utc.tm_min, utc.tm_sec, static_cast<int>(ms));
            return std::string(buf);
        }
    } // namespace

    void log_event(const std::string &component, const std::string &event,
                    const std::vector<std::pair<std::string, std::string>> &fields)
    {
        std::ostringstream out;
        out << "{\"component\":\"" << json_escape(component) << "\","
            << "\"event\":\"" << json_escape(event) << "\","
            << "\"ts\":\"" << now_iso8601() << "\"";
        for (const auto &kv : fields)
        {
            out << ",\"" << json_escape(kv.first) << "\":\"" << json_escape(kv.second) << "\"";
        }
        out << "}";
        std::cerr << "[drop_observe] " << out.str() << std::endl;
    }

} // namespace drop

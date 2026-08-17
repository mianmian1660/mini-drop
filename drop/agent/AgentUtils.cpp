#include "agent/AgentUtils.h"

#include <cstdlib>
#include <algorithm>
#include <cctype>
#include <sys/stat.h>

using namespace std;

namespace drop_agent
{

    bool FileExists(const string &path)
    {
        struct stat st;
        return stat(path.c_str(), &st) == 0;
    }

    string JsonEscape(const string &s)
    {
        string out;
        out.reserve(s.size());
        for (char c : s)
        {
            if (c == '"' || c == '\\')
                out += '\\';
            if (c == '\n')
            {
                out += "\\n";
                continue;
            }
            out += c;
        }
        return out;
    }

    bool EnvEnabled(const char *name)
    {
        const char *v = getenv(name);
        if (!v)
            return false;
        string s(v);
        transform(s.begin(), s.end(), s.begin(), [](unsigned char c)
                  { return static_cast<char>(tolower(c)); });
        return s == "1" || s == "true" || s == "yes" || s == "on";
    }

    string EnvString(const char *name, const string &fallback)
    {
        const char *v = getenv(name);
        if (v && *v)
            return v;
        return fallback;
    }

} // namespace drop_agent

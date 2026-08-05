package util

import (
	"net/url"
	"regexp"
	"strings"
)

var (
	dsnPasswordRE = regexp.MustCompile(`(?i)(password=)([^ \t\n\r]+)`)
	urlSecretRE   = regexp.MustCompile(`(?i)([?&](token|access_token|secret|signature|X-Amz-Signature)=)([^&\s]+)`)
	bearerRE      = regexp.MustCompile(`(?i)(bearer\s+)([A-Za-z0-9._~+/=-]+)`)
	urlUserInfoRE = regexp.MustCompile(`(?i)(https?://[^/\s:@]+:)([^@\s/]+)(@)`)
)

// RedactSecret masks credentials and bearer-style tokens before values reach logs.
func RedactSecret(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return value
	}
	value = dsnPasswordRE.ReplaceAllString(value, "${1}***")
	value = urlSecretRE.ReplaceAllString(value, "${1}***")
	value = bearerRE.ReplaceAllString(value, "${1}***")
	value = urlUserInfoRE.ReplaceAllString(value, "${1}***${3}")
	if strings.Contains(value, "://") {
		if parsed, err := url.Parse(value); err == nil && parsed.User != nil {
			username := parsed.User.Username()
			if username != "" {
				parsed.User = url.UserPassword(username, "***")
				value = parsed.String()
			}
		}
	}
	return value
}

// RedactObjectKey keeps the task prefix and file name useful while hiding
// sensitive middle path fragments that may contain tenant, PID or host details.
func RedactObjectKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return key
	}
	parts := strings.Split(key, "/")
	if len(parts) <= 2 {
		return key
	}
	return parts[0] + "/***/" + parts[len(parts)-1]
}

package util

import (
	"strings"
	"testing"
)

func TestRedactSecretMasksCredentialsAndTokens(t *testing.T) {
	raw := "host=db user=drop password=dev dbname=drop https://u:p@example.test/a?token=abc&ok=1 Authorization: Bearer secret-token"
	got := RedactSecret(raw)
	for _, forbidden := range []string{"password=dev", "token=abc", "Bearer secret-token", "u:p@"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("redacted value still contains %q: %s", forbidden, got)
		}
	}
	for _, want := range []string{"password=***", "token=***", "Bearer ***"} {
		if !strings.Contains(got, want) {
			t.Fatalf("redacted value missing %q: %s", want, got)
		}
	}
}

func TestRedactObjectKeyKeepsUsefulEdges(t *testing.T) {
	got := RedactObjectKey("tid-1/private/tenant/pid-123/perf.data")
	if got != "tid-1/***/perf.data" {
		t.Fatalf("redacted key=%q", got)
	}
	if RedactObjectKey("tid-1/perf.data") != "tid-1/perf.data" {
		t.Fatalf("short keys should stay readable")
	}
}

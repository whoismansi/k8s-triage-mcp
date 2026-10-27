package triage

import (
	"strings"
	"testing"
)

// fake credentials are assembled at runtime so the source never contains a
// complete token literal. secret scanners match on shape and would otherwise
// flag these fixtures, or block a push, even though they are not real.
var (
	fakeAWSKey      = "AKIA" + strings.Repeat("X", 16)
	fakeGitHubToken = "ghp" + "_" + strings.Repeat("a", 36)
	fakeJWT         = "eyJ" + strings.Repeat("h", 12) + "." + strings.Repeat("p", 12) + "." + strings.Repeat("s", 12)
)

func TestRedactLine(t *testing.T) {
	tests := []struct {
		name string
		in   string
		// mustHide are substrings that must not survive redaction.
		mustHide []string
		// mustKeep are substrings that must survive, so the line stays useful.
		mustKeep []string
	}{
		{
			name:     "password in a database URL keeps host and user",
			in:       "connecting to postgres://app:s3cr3tPass@db.internal:5432/orders",
			mustHide: []string{"s3cr3tPass"},
			mustKeep: []string{"postgres://app:", "@db.internal:5432/orders"},
		},
		{
			name:     "bearer token",
			in:       "request failed: Authorization: Bearer abcDEF123456789ghiJKL",
			mustHide: []string{"abcDEF123456789ghiJKL"},
			mustKeep: []string{"Bearer"},
		},
		{
			name:     "jwt",
			in:       "token=" + fakeJWT,
			mustHide: []string{fakeJWT},
		},
		{
			name:     "env style secret keeps the key name",
			in:       "loaded config DB_PASSWORD=hunter2 PORT=8080",
			mustHide: []string{"hunter2"},
			mustKeep: []string{"DB_PASSWORD=", "PORT=8080"},
		},
		{
			name:     "quoted json secret",
			in:       `{"api_key": "abc123secretvalue", "region": "us-east-1"}`,
			mustHide: []string{"abc123secretvalue"},
			mustKeep: []string{`"region": "us-east-1"`},
		},
		{
			name:     "aws access key id",
			in:       "using credentials " + fakeAWSKey,
			mustHide: []string{fakeAWSKey},
		},
		{
			name:     "github token",
			in:       "clone with " + fakeGitHubToken,
			mustHide: []string{fakeGitHubToken},
		},
		{
			name:     "private key header",
			in:       "-----BEGIN RSA PRIVATE KEY-----",
			mustHide: []string{"BEGIN RSA PRIVATE KEY"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, count := redactLine(test.in)
			if count == 0 {
				t.Errorf("redactLine(%q) redacted nothing", test.in)
			}
			for _, secret := range test.mustHide {
				if strings.Contains(got, secret) {
					t.Errorf("secret %q survived: %q", secret, got)
				}
			}
			for _, context := range test.mustKeep {
				if !strings.Contains(got, context) {
					t.Errorf("useful context %q was lost: %q", context, got)
				}
			}
		})
	}
}

// TestRedactLineLeavesDiagnosticsAlone guards against over-redaction: lines a
// human needs to diagnose a crash must pass through untouched.
func TestRedactLineLeavesDiagnosticsAlone(t *testing.T) {
	lines := []string{
		`panic: runtime error: invalid memory address or nil pointer dereference`,
		`exec /app/server: no such file or directory`,
		`listening on http://0.0.0.0:8080`,
		`error: missing required env var DATABASE_URL`,
		`token bucket refilled, capacity 100`,
		`java.lang.OutOfMemoryError: Java heap space`,
	}
	for _, line := range lines {
		got, count := redactLine(line)
		if count != 0 || got != line {
			t.Errorf("redactLine(%q) = %q (%d redactions), want unchanged", line, got, count)
		}
	}
}

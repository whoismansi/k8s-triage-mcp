package triage

import "regexp"

// redaction hides credentials that applications commonly print to their logs.
//
// the server's RBAC denies access to Secrets, but logs are a side channel:
// apps print connection strings, bearer tokens and keys during startup, and
// returning them verbatim would ship that material to the model provider. this
// is a best-effort filter over well-known shapes, not a guarantee, which is why
// the output says how many values were redacted rather than claiming the log
// is clean.

// RedactedPlaceholder replaces each redacted value.
const RedactedPlaceholder = "[REDACTED]"

type redactionRule struct {
	pattern *regexp.Regexp
	// replacement keeps non-secret context (key names, URL scheme and host) so
	// the line is still useful for diagnosis.
	replacement string
}

var redactionRules = []redactionRule{
	// password inside a URL: scheme://user:secret@host
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^\s:/@]+:)[^\s@/]+@`), "${1}" + RedactedPlaceholder + "@"},
	// authorization headers and bearer/basic tokens
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`), "${1} " + RedactedPlaceholder},
	// json web tokens
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\b`), RedactedPlaceholder},
	// pem private key headers
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`), RedactedPlaceholder},
	// well-known provider token shapes
	{regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`), RedactedPlaceholder},
	{regexp.MustCompile(`\b(ghp|gho|ghu|ghs|ghr|github_pat)_[A-Za-z0-9_]{20,}\b`), RedactedPlaceholder},
	{regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`), RedactedPlaceholder},
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`), RedactedPlaceholder},
	// key=value or key: value where the key name says it is secret. the key is
	// kept so the model can still see that, say, DB_PASSWORD was set. the
	// optional quote after the key covers JSON, which most structured loggers
	// emit: "api_key": "value".
	{regexp.MustCompile(`(?i)\b([A-Za-z0-9_.-]*(password|passwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credential)[A-Za-z0-9_.-]*["']?)(\s*[=:]\s*)("[^"]*"|'[^']*'|[^\s,;]+)`), "${1}${3}" + RedactedPlaceholder},
}

// redactLine applies every rule to one line and reports how many values were
// replaced.
func redactLine(line string) (string, int) {
	count := 0
	for _, rule := range redactionRules {
		matches := len(rule.pattern.FindAllStringIndex(line, -1))
		if matches == 0 {
			continue
		}
		count += matches
		line = rule.pattern.ReplaceAllString(line, rule.replacement)
	}
	return line, count
}

// secretNamePattern matches variable names that conventionally hold secrets.
var secretNamePattern = regexp.MustCompile(`(?i)(password|passwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credential)`)

// redactValue hides a value entirely when its name says it is secret, and
// otherwise applies the line rules (which catch tokens embedded in URLs).
func redactValue(name, value string) string {
	if value == "" {
		return value
	}
	if secretNamePattern.MatchString(name) {
		return RedactedPlaceholder
	}
	redacted, _ := redactLine(value)
	return redacted
}

// redactLines redacts a slice in place and returns the total replacements.
func redactLines(lines []string) int {
	total := 0
	for index, line := range lines {
		redacted, count := redactLine(line)
		lines[index] = redacted
		total += count
	}
	return total
}

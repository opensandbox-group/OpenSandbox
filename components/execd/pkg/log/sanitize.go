// Copyright 2026 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package log

import "regexp"

var sanitizePatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	// === URL / header / connection-string patterns ===

	// URL credentials: scheme://user:password@host
	{
		regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s:@]+:[^/\s:@]+@`),
		`${1}****:****@`,
	},
	// Authorization: Bearer/Basic/Token/ApiKey header value
	{
		regexp.MustCompile(`(?i)(Authorization\s*:\s*(?:Bearer|Basic|Token|ApiKey|apiKey)\s+)\S+`),
		`${1}****`,
	},
	// Azure storage AccountKey / SharedAccessKey (stop at ; " ' whitespace)
	{
		regexp.MustCompile(`((?:Account|SharedAccess)Key\s*=\s*)[^;"'\s]+`),
		`${1}****`,
	},

	// === Long-flag sensitive options: --flag value or --flag=value ===
	// Flag names listed longest-first so RE2 picks the right match.
	// Requiring [\s=] after the flag name prevents --password from matching
	// --password-stdin (the trailing - isn't whitespace or =).

	{
		regexp.MustCompile(
			`(--(?:access-key-id|access-key-secret|access-token|access-key|` +
				`secret-access-key|secret-key|secret-id|secret|` +
				`aws-secret-access-key|private-key|client-secret|refresh-token|` +
				`sentry-token|credential|password|passwd|` +
				`api-key|apikey|token|` +
				`ak|sk))` +
				`([\s=])\s*` +
				`(?:"[^"]*"|'[^']*'|\S+)?`,
		),
		`${1}${2}****`,
	},

	// === Environment variable assignments with sensitive names ===

	{
		regexp.MustCompile(
			`\b(PASSWORD|PASSWD|SECRET|TOKEN|API_KEY|APIKEY|AUTH_TOKEN|ACCESS_TOKEN|` +
				`PRIVATE_KEY|CLIENT_SECRET|REFRESH_TOKEN|CREDENTIAL|AUTH_KEY|SECRET_KEY|SECRET_ID|` +
				// Cloud-provider specific
				`ACCESS_KEY_ID|ACCESS_KEY_SECRET|SECRET_ACCESS_KEY|` +
				`AWS_ACCESS_KEY_ID|AWS_SECRET_ACCESS_KEY|AWS_SESSION_TOKEN|` +
				`ALIBABA_CLOUD_ACCESS_KEY_ID|ALIBABA_CLOUD_ACCESS_KEY_SECRET|ALIBABA_CLOUD_SECRET_KEY|` +
				`ALICLOUD_ACCESS_KEY_ID|ALICLOUD_ACCESS_KEY_SECRET|ALICLOUD_SECRET_KEY|` +
				`TENCENTCLOUD_SECRET_ID|TENCENTCLOUD_SECRET_KEY|` +
				`HUAWEICLOUD_ACCESS_KEY_ID|HUAWEICLOUD_SECRET_ACCESS_KEY|` +
				`CLOUD_ACCESS_KEY_ID|CLOUD_SECRET_KEY|CLOUD_API_KEY|CLOUD_API_SECRET|` +
				`RAM_ACCESS_KEY_ID|RAM_ACCESS_KEY_SECRET|` +
				`OTS_ACCESS_KEY_ID|OTS_ACCESS_KEY_SECRET|` +
				`ACCOUNT_KEY|SHARED_ACCESS_KEY|AZURE_STORAGE_KEY|` +
				`GCP_CREDENTIALS|GOOGLE_APPLICATION_CREDENTIALS|` +
				`DOCKER_PASSWORD|DOCKER_TOKEN|REGISTRY_PASSWORD` +
				`)\s*=\s*(?:"[^"]*"|'[^']*'|\S+)`,
		),
		`${1}=****`,
	},

	// === Bare cloud access key matching (prefix-specific, low false-positive) ===

	// Alibaba Cloud AccessKey ID: LTAI + 16~32 alphanumeric
	{
		regexp.MustCompile(`\bLTAI[a-zA-Z0-9]{16,32}\b`),
		`LTAI****`,
	},
	// AWS Access Key ID: AKIA + 16 uppercase
	{
		regexp.MustCompile(`\bAKIA[A-Z0-9]{16}\b`),
		`AKIA****`,
	},
	// Tencent Cloud Secret ID: AKID + 16~48 alphanumeric
	{
		regexp.MustCompile(`\bAKID[a-zA-Z0-9]{16,48}\b`),
		`AKID****`,
	},
}

// SanitizeCommand masks sensitive values (passwords, tokens, keys, credentials)
// in a shell command string so it is safe to log.
func SanitizeCommand(cmd string) string {
	for _, p := range sanitizePatterns {
		cmd = p.re.ReplaceAllString(cmd, p.repl)
	}
	return cmd
}

// MaskToken returns a partially masked token for logging.
// Shows first 4 and last 4 characters; returns "****" for tokens 8 chars or shorter.
func MaskToken(token string) string {
	if len(token) <= 8 {
		return "****"
	}
	return token[:4] + "****" + token[len(token)-4:]
}

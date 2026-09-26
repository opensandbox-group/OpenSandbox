// Copyright 2026 The OpenSandbox Authors
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

package opensandbox

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// envKeyPattern matches valid environment variable names persisted via SetEnv.
var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// shellQuote quotes s as a single POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// escapeDoubleQuoted escapes a value for the runtime env file's double-quoted form.
func escapeDoubleQuoted(value string) string {
	return strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"\n", `\n`,
		"\r", `\r`,
		"\t", `\t`,
	).Replace(value)
}

// buildSetEnvCommand builds the sandbox-side snippet that appends KEY=VALUE to
// the env file named by the sandbox's EXECD_ENVS variable. Values without a
// single quote use the env file's lossless single-quoted form; otherwise the
// double-quoted form is used (shell-style $NAME sequences in such values may
// be expanded when the runtime loads the file).
func buildSetEnvCommand(key, value string) (string, error) {
	if !envKeyPattern.MatchString(key) {
		return "", fmt.Errorf("opensandbox: SetEnv key must match [A-Za-z_][A-Za-z0-9_]*, got %q", key)
	}
	if strings.ContainsRune(value, '\x00') {
		return "", fmt.Errorf("opensandbox: SetEnv value cannot contain NUL bytes")
	}
	if !utf8.ValidString(value) {
		return "", fmt.Errorf("opensandbox: SetEnv value must be valid UTF-8")
	}
	var entry string
	if strings.ContainsRune(value, '\'') {
		entry = fmt.Sprintf(`%s="%s"`, key, escapeDoubleQuoted(value))
	} else {
		entry = fmt.Sprintf(`%s='%s'`, key, value)
	}
	return strings.Join([]string{
		fmt.Sprintf("if [ -z \"${EXECD_ENVS:-}\" ]; then printf '%%s\\n' 'EXECD_ENVS is not set; cannot persist environment variable %s' >&2; exit 1; fi", key),
		`mkdir -p "$(dirname "$EXECD_ENVS")"`,
		fmt.Sprintf("printf '%%s\\n' %s >> \"$EXECD_ENVS\"", shellQuote(entry)),
	}, "\n"), nil
}

// SetEnv persists an environment variable for future commands and sessions.
//
// It appends KEY=VALUE to the sandbox env file that the runtime loads for
// every command and session (the file named by the sandbox's EXECD_ENVS
// variable, resolved inside the sandbox). Keys must match
// [A-Za-z_][A-Za-z0-9_]*. Values without a single quote are stored verbatim;
// values containing a single quote use the env file's double-quoted form, in
// which shell-style $NAME sequences may be expanded when the runtime loads
// the file. The file is append-only: the last write for a key wins. Returns
// an error if the sandbox fails to persist the variable.
func (s *Sandbox) SetEnv(ctx context.Context, key, value string) error {
	command, err := buildSetEnvCommand(key, value)
	if err != nil {
		return err
	}

	exec, err := s.RunCommand(ctx, command, nil)
	if err != nil {
		return fmt.Errorf("opensandbox: SetEnv %q: %w", key, err)
	}
	// A foreground command only reports ExitCode 0 after a confirmed
	// execution_complete event; a missing exit code (e.g. a dropped stream)
	// is treated as failure because the append was never confirmed.
	if exec.Error == nil && exec.ExitCode != nil && *exec.ExitCode == 0 {
		return nil
	}

	var stderr strings.Builder
	for _, m := range exec.Stderr {
		stderr.WriteString(m.Text)
	}
	detail := strings.TrimSpace(stderr.String())
	if detail == "" && exec.Error != nil {
		detail = strings.TrimSpace(exec.Error.Value)
	}
	if detail != "" {
		return fmt.Errorf("opensandbox: SetEnv %q failed: %s", key, detail)
	}
	return fmt.Errorf("opensandbox: SetEnv %q failed", key)
}

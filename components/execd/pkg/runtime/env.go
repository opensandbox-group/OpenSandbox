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

package runtime

import (
	"fmt"
	"os"
	"strings"

	"github.com/alibaba/opensandbox/execd/pkg/binding"
	"github.com/alibaba/opensandbox/execd/pkg/isolation"
	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/util/pathutil"
)

// loadExtraEnvFromFile reads key=value entries from the file named by EXECD_ENVS (if set).
func loadExtraEnvFromFile() map[string]string {
	path := os.Getenv("EXECD_ENVS")
	if path == "" {
		return nil
	}
	resolvedPath, err := pathutil.ExpandPath(path)
	if err != nil {
		log.Warn("EXECD_ENVS: failed to resolve file path %s: %v", path, err)
		return nil
	}

	data, err := os.ReadFile(resolvedPath)
	if err != nil {
		log.Warn("EXECD_ENVS: failed to read file %s: %v", resolvedPath, err)
		return nil
	}

	return parseEnvFile(string(data))
}

// parseEnvFile parses KEY=VALUE entries from an EXECD_ENVS file body.
// Blank lines and '#' comment lines are ignored. Malformed and unterminated
// entries are skipped with a warning.
//
//	KEY=value     trimmed, then $NAME / ${NAME} expanded from the daemon env
//	KEY="v a l"   whitespace kept; \n \r \t \\ \" escapes; $NAME expands
//	KEY='v a l'   fully literal; may span lines; the lossless form
func parseEnvFile(data string) map[string]string {
	p := &envFileParser{data: data, lineNo: 1}
	envs := make(map[string]string)
	for p.pos < len(p.data) {
		if key, value, ok := p.parseEntry(); ok {
			envs[pathutil.EnvKey(key)] = value
		}
	}
	return envs
}

type envFileParser struct {
	data   string
	pos    int
	lineNo int
}

// parseEntry parses one entry, advancing past it on success or past its
// first physical line on failure.
func (p *envFileParser) parseEntry() (key, value string, ok bool) {
	lineStart := p.pos
	eol := strings.IndexByte(p.data[lineStart:], '\n')
	lineEnd := len(p.data)
	if eol >= 0 {
		lineEnd = lineStart + eol
	}
	line := p.data[lineStart:lineEnd]

	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		p.skipLine()
		return "", "", false
	}

	eq := strings.IndexByte(line, '=')
	if eq < 0 || strings.TrimSpace(line[:eq]) == "" {
		log.Warn("EXECD_ENVS: skip malformed line %d: %s", p.lineNo, trimmed)
		p.skipLine()
		return "", "", false
	}
	key = strings.TrimSpace(line[:eq])

	valuePart := line[eq+1:]
	qs := indexNonSpace(valuePart)
	if qs < len(valuePart) && (valuePart[qs] == '"' || valuePart[qs] == '\'') {
		return p.parseQuotedEntry(key, valuePart[qs], lineStart+eq+1+qs+1)
	}

	value = os.ExpandEnv(strings.TrimSpace(valuePart))
	p.skipLine()
	return key, value, true
}

// parseQuotedEntry parses a quoted value whose content starts at contentStart.
func (p *envFileParser) parseQuotedEntry(key string, quote byte, contentStart int) (string, string, bool) {
	var value string
	var afterQuote int
	if quote == '\'' {
		rel := strings.IndexByte(p.data[contentStart:], '\'')
		if rel < 0 {
			log.Warn("EXECD_ENVS: skip unterminated single-quoted value for %s at line %d", key, p.lineNo)
			p.skipLine()
			return "", "", false
		}
		value = p.data[contentStart : contentStart+rel]
		afterQuote = contentStart + rel + 1
	} else {
		i := contentStart
		closed := false
		for i < len(p.data) {
			if p.data[i] == '\\' && i+1 < len(p.data) {
				i += 2
				continue
			}
			if p.data[i] == '"' {
				closed = true
				break
			}
			i++
		}
		if !closed {
			log.Warn("EXECD_ENVS: skip unterminated double-quoted value for %s at line %d", key, p.lineNo)
			p.skipLine()
			return "", "", false
		}
		value = os.ExpandEnv(decodeDoubleQuoted(p.data[contentStart:i]))
		afterQuote = i + 1
	}
	return p.finishQuotedEntry(key, afterQuote, value)
}

// finishQuotedEntry validates the rest of the closing-quote line and advances past it.
func (p *envFileParser) finishQuotedEntry(key string, afterQuote int, value string) (string, string, bool) {
	nlRel := strings.IndexByte(p.data[afterQuote:], '\n')
	lineEnd := len(p.data)
	if nlRel >= 0 {
		lineEnd = afterQuote + nlRel
	}
	if tail := strings.TrimSpace(p.data[afterQuote:lineEnd]); tail != "" {
		log.Warn("EXECD_ENVS: skip entry %s: unexpected text after closing quote at line %d: %s", key, p.lineNo, tail)
		p.skipLine()
		return "", "", false
	}
	next := lineEnd
	if nlRel >= 0 {
		next = lineEnd + 1
	}
	p.lineNo += strings.Count(p.data[p.pos:next], "\n")
	p.pos = next
	return key, value, true
}

// skipLine advances past the current physical line.
func (p *envFileParser) skipLine() {
	if nl := strings.IndexByte(p.data[p.pos:], '\n'); nl >= 0 {
		p.pos += nl + 1
	} else {
		p.pos = len(p.data)
	}
	p.lineNo++
}

// indexNonSpace returns the index of the first non-space byte in s, or len(s).
func indexNonSpace(s string) int {
	for i := range len(s) {
		switch s[i] {
		case ' ', '\t', '\r', '\v', '\f':
		default:
			return i
		}
	}
	return len(s)
}

// decodeDoubleQuoted unescapes \n \r \t \\ \" sequences; unknown escapes are kept verbatim.
func decodeDoubleQuoted(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n':
				b.WriteByte('\n')
				i++
				continue
			case 'r':
				b.WriteByte('\r')
				i++
				continue
			case 't':
				b.WriteByte('\t')
				i++
				continue
			case '\\', '"':
				b.WriteByte(s[i+1])
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// mergeEnvs overlays extra into base and returns a merged slice.
func mergeEnvs(base []string, extra map[string]string) []string {
	if len(extra) == 0 {
		return base
	}

	merged := make(map[string]string, len(base)+len(extra))
	for _, kv := range base {
		pair := strings.SplitN(kv, "=", 2)
		if len(pair) == 2 {
			merged[pathutil.EnvKey(pair[0])] = pair[1]
		}
	}

	for k, v := range extra {
		merged[pathutil.EnvKey(k)] = v
	}

	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, fmt.Sprintf("%s=%s", k, v))
	}

	return out
}

// bindingSandboxEnvs returns the sandbox-level envs provided by
// POST /internal/init, or nil when no RuntimeBinding (or no envs) is applied.
func bindingSandboxEnvs() map[string]string {
	b := binding.Current()
	if b == nil || len(b.Envs) == 0 {
		return nil
	}
	return b.Envs
}

// UserEnvOverlay builds the standard user-workload env overlay, layered
// with the /internal/init RuntimeBinding as the authoritative source:
//
//	sandbox envs (/internal/init) < EXECD_ENVS file < extras (session/request)
//
// Binding-authoritative values (OPENSANDBOX_ID) are forced on top so user
// envs cannot spoof sandbox attribution.
func UserEnvOverlay(extras ...map[string]string) map[string]string {
	layers := make([]map[string]string, 0, len(extras)+2)
	if envs := bindingSandboxEnvs(); envs != nil {
		layers = append(layers, envs)
	}
	if fileEnvs := loadExtraEnvFromFile(); len(fileEnvs) > 0 {
		layers = append(layers, fileEnvs)
	}
	layers = append(layers, extras...)

	merged := make(map[string]string)
	for _, layer := range layers {
		for k, v := range layer {
			merged[pathutil.EnvKey(k)] = v
		}
	}
	if b := binding.Current(); b != nil && b.SandboxID != "" {
		merged["OPENSANDBOX_ID"] = b.SandboxID
	}
	return merged
}

// filterEnvBlacklist removes execd's own config/credential env entries from a
// base environ slice.
func filterEnvBlacklist(env []string) []string {
	return filterEnvNames(env, isolation.ExecdConfigEnvBlacklist())
}

// filterEnvNames removes the named env entries from a base environ slice
// (case-insensitive on names).
func filterEnvNames(env []string, names []string) []string {
	blocked := make(map[string]struct{}, len(names))
	for _, name := range names {
		blocked[strings.ToUpper(name)] = struct{}{}
	}

	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, found := blocked[strings.ToUpper(name)]; found {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

// UserProcessEnvironment returns the environment for user processes started
// outside a request (PTY/pipe sessions, lifecycle hooks): the daemon
// environment minus execd config/credential vars, overlaid with the standard
// user env (sandbox binding envs < EXECD_ENVS file).
func UserProcessEnvironment() []string {
	return mergeEnvs(filterEnvBlacklist(os.Environ()), UserEnvOverlay())
}

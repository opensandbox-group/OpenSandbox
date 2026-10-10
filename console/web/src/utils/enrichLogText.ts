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

import { LOG_ANSI, logLevelAnsi } from './semanticTheme';

/** 去掉行内已有 ANSI，避免业务日志（如 Jupyter）局部着色导致整段跳过我们的规则。 */
const STRIP_ANSI = /\x1b\[[0-9;]*m/g;

const TIMESTAMP_SOURCE =
  String.raw`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:[.,]\d+)?(?:Z|[+-]\d{2}(?::?\d{2})?)?`;

const LEVEL_SOURCE = String.raw`\b(ERROR|ERR|WARN|WARNING|INFO|DEBUG|TRACE|FATAL|PANIC|CRITICAL)\b`;

/** Python / Jupyter：`[I 2026-09-29 07:21:50,001 ServerApp]` */
const PYTHON_LOG_RECORD =
  String.raw`\[([IWEDCTF])\s*(\d{4}-\d{2}-\d{2}\s+\d{2}:\d{2}:\d{2}[,\.]\d+)([A-Za-z][\w.]*)\]`;

const UNIX_PATH_SOURCE = String.raw`(?:^|[\s"'(])(\/(?:[\w.@~-]+|\/)+[\w.@~-]*)`;

const KEYWORDS_SOURCE =
  String.raw`\b(failed|failure|error|exception|panic|starting|started|ready|listening|shutdown|timeout|denied|unauthorized|forbidden)\b`;

const { reset: RESET, dim: DIM, cyan: CYAN, magenta: MAGENTA, red: RED, yellow: YELLOW, green: GREEN } =
  LOG_ANSI;

function levelColor(level: string): string {
  return logLevelAnsi(level);
}

function keywordColor(word: string): string {
  const u = word.toLowerCase();
  if (u === 'failed' || u === 'failure' || u === 'error' || u === 'exception' || u === 'panic') {
    return RED;
  }
  if (u === 'denied' || u === 'unauthorized' || u === 'forbidden' || u === 'timeout') {
    return YELLOW;
  }
  if (u === 'starting' || u === 'started' || u === 'ready' || u === 'listening') {
    return GREEN;
  }
  return MAGENTA;
}

function dimTimestamps(text: string): string {
  return text.replace(new RegExp(TIMESTAMP_SOURCE, 'g'), (m) => `${DIM}${m}${RESET}`);
}

function applyKeywordHighlights(text: string): string {
  return text.replace(new RegExp(KEYWORDS_SOURCE, 'gi'), (m) => `${keywordColor(m)}${m}${RESET}`);
}

/** K8s 采集前缀 + zap/uber JSON：`2026-09-29T…+08:00{"level":"warn","ts":"…","msg":"…"}` */
function colorizeJsonLine(line: string): string | null {
  const jsonStart = line.indexOf('{');
  if (jsonStart < 0) return null;
  const prefix = line.slice(0, jsonStart);
  const jsonPart = line.slice(jsonStart).trimEnd();
  if (!jsonPart.startsWith('{')) return null;
  try {
    JSON.parse(jsonPart);
  } catch {
    return null;
  }

  let jsonOut = jsonPart;
  jsonOut = jsonOut.replace(
    /"level"\s*:\s*"([^"]+)"/gi,
    (_m, lvl: string) => `"level":${logLevelAnsi(lvl)}"${lvl}"${RESET}`,
  );
  jsonOut = jsonOut.replace(
    /"(ts|time|timestamp)"\s*:\s*"([^"]+)"/gi,
    (_m, key: string, ts: string) => `"${key}":${DIM}"${ts}"${RESET}`,
  );
  jsonOut = jsonOut.replace(
    /"(msg|message)"\s*:\s*"((?:\\.|[^"\\])*)"/gi,
    (_m, key: string, msg: string) => `"${key}":"${applyKeywordHighlights(msg)}"`,
  );

  return dimTimestamps(prefix.replace(STRIP_ANSI, '')) + jsonOut;
}

/** 为纯文本日志注入颜色，供 LazyLog 解析展示。 */
function colorizePlainLine(line: string): string {
  const stripped = line.replace(STRIP_ANSI, '');
  const jsonColored = colorizeJsonLine(stripped);
  if (jsonColored !== null) return jsonColored;

  let out = stripped;

  out = out.replace(new RegExp(PYTHON_LOG_RECORD, 'g'), (_m, lvl: string, ts: string, logger: string) => {
    const bracket = `[${lvl} ${ts} ${logger}]`;
    return `${logLevelAnsi(lvl)}${bracket}${RESET}`;
  });
  out = dimTimestamps(out);
  out = out.replace(/OpenSandbox/g, `${MAGENTA}OpenSandbox${RESET}`);
  out = out.replace(new RegExp(LEVEL_SOURCE, 'gi'), (m) => `${levelColor(m)}${m}${RESET}`);
  out = applyKeywordHighlights(out);
  out = out.replace(new RegExp(UNIX_PATH_SOURCE, 'g'), (m, path: string) =>
    m.replace(path, `${CYAN}${path}${RESET}`),
  );

  return out;
}

/** 按行着色；混有业务进程自带 ANSI 的容器日志也会统一处理。 */
export function enrichLogTextForDisplay(raw: string | null | undefined): string {
  const text = raw?.trim() ? raw : '(empty)';
  return text.split('\n').map(colorizePlainLine).join('\n');
}

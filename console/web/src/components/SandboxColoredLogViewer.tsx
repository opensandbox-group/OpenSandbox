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

import { LazyLog } from '@melloware/react-logviewer';
import { useMemo } from 'react';

import { enrichLogTextForDisplay } from '../utils/enrichLogText';

import './SandboxColoredLogViewer.css';

type Props = {
  content: string;
  maxHeight?: number;
  /** 刷新后强制 remount，避免 LazyLog 缓存旧文本 */
  contentKey?: string;
};

const LOG_VIEWER_STYLE = {
  backgroundColor: '#1e1e1e',
  borderRadius: 6,
  fontSize: 12,
  fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace',
} as const;

export function SandboxColoredLogViewer({ content, maxHeight = 420, contentKey }: Props) {
  const text = useMemo(() => enrichLogTextForDisplay(content), [content]);

  return (
    <div className="sandbox-log-viewer">
      <LazyLog
        key={contentKey ?? text.length}
        text={text}
        height={maxHeight}
        width="100%"
        enableSearch
        enableSearchNavigation
        selectableLines
        rowHeight={18}
        style={LOG_VIEWER_STYLE}
        containerStyle={{ background: '#1e1e1e', borderRadius: 6, maxWidth: '100%' }}
        searchBarClassName="sandbox-log-search"
        internacionalization={{
          searchBar: {
            searchPlaceholder: '搜索日志…',
            filterLinesTitle: '仅显示匹配行',
            previousButtonTitle: '上一处',
            nextButtonTitle: '下一处',
          },
        }}
      />
    </div>
  );
}

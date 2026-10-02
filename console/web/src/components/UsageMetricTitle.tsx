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

import { QuestionCircleOutlined } from '@ant-design/icons';
import { Space, Tooltip } from 'antd';
import type { ReactNode } from 'react';

export const USAGE_METRIC_HINTS = {
  sandboxCount:
    '统计区间内，生命周期与窗口 [from, to) 有交集的沙箱条数（与「占用时长」同一批实例；每条 history 计 1）。' +
    '点击数字可跳转申请历史并带上相同区间筛选。' +
    '不含未写入 sandbox_lifecycle_history 的实例（需 Server 生命周期审计或 Console/对账）；不等于概览「列表中的沙箱数」。',
  overlapSeconds:
    '统计区间内，每条沙箱从创建到结束（销毁/终止）的生命周期与区间交集时长之和（秒）。',
  cpuCoreSeconds:
    'Σ ( CPU limits 换算成核数 × 该沙箱占用秒数 )。limits 来自创建时的 resourceLimits.cpu（如 500m→0.5 核）。按 limit 满额计算，不是实际 CPU 利用率。例：0.5 核运行 10 分钟 → 0.5 × 600 = 300 CPU·秒。',
  memoryGiSeconds:
    'Σ ( 内存 limits 换算成 GiB × 该沙箱占用秒数 )。limits 来自 resourceLimits.memory（如 10Gi→10）。按 limit 满额计算。例：10 GiB 运行 10 分钟 → 10 × 600 = 6000 内存·Gi·秒。',
  shareTime: '该租户占用时长 ÷ 区间内全部 Console 历史沙箱占用时长 × 100%。',
  shareCpu: '该租户 CPU·秒 ÷ 全集群同区间 CPU·秒合计 × 100%。',
  shareMemory: '该租户内存·Gi·秒 ÷ 全集群同区间内存·Gi·秒合计 × 100%。',
  shareComposite:
    '综合分摊占比 = (T_i/T_总 + C_i/C_总 + M_i/M_总) / k × 100%，k 为区间内有效的维度数（通常 k=3，即时长、CPU·秒、内存·Gi·秒三者等权）。' +
    '各租户综合占比相加为 100%。CPU/内存指标已含 limits×时间，与纯时长维度互补；若规格差异大，三项占比会不一致，综合值折中三者。',
} as const;

export function UsageMetricTitle({ label, hint }: { label: ReactNode; hint: string }) {
  return (
    <Space size={4}>
      <span>{label}</span>
      <Tooltip title={<span style={{ whiteSpace: 'pre-wrap' }}>{hint}</span>}>
        <QuestionCircleOutlined style={{ color: 'rgba(0,0,0,0.45)', fontSize: 12, cursor: 'help' }} />
      </Tooltip>
    </Space>
  );
}

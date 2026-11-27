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

import type { Messages } from './en';

/** Simplified Chinese messages. Technical names (BFF, Lifecycle, OSS, K8s, IDs) stay as-is. */
export const zhCN: Messages = {
  'common.language': '语言',
  'common.refresh': '刷新',
  'common.cancel': '取消',
  'common.status': '状态',
  'common.image': '镜像',
  'common.uptime': '运行时长',
  'common.remaining': '剩余',
  'common.actions': '操作',
  'common.overview': '概览',
  'common.logs': '日志',
  'common.createSandbox': '创建沙箱',
  'common.renew': '续期',
  'common.delete': '删除',
  'common.pause': '暂停',
  'common.resume': '恢复',
  'common.shell': '终端',

  'nav.sandboxes': '沙箱',
  'nav.sandboxesAllTenants': '沙箱（所有租户）',
  'nav.snapshots': '快照',
  'nav.pool': '资源池',
  'nav.diagnostics': '诊断',
  'nav.platform': '平台',
  'nav.componentHealth': '组件健康',
  'nav.version': '版本',
  'nav.k8sWorkloads': 'K8s 工作负载',
  'nav.k8sEvents': 'K8s 事件',
  'nav.admin': '管理员',
  'nav.platformFallback': '平台',
  'nav.signOut': '退出登录',

  // Login page.
  'login.title': 'OpenSandbox 控制台',
  'login.apiKeyNotice': 'API 密钥仅发送到 BFF，浏览器不会存储 Lifecycle 密钥。',
  'login.tenantTab': '租户用户',
  'login.tenantApiKey': '租户 API 密钥',
  'login.tenantApiKeyRequired': '请输入 API 密钥',
  'login.adminTab': '平台管理员',
  'login.adminToken': 'BFF 管理员令牌',
  'login.adminTokenRequired': '请输入管理员令牌',
  'login.signIn': '登录',
  'login.tenantSuccess': '租户登录成功',
  'login.adminSuccess': '管理员登录成功',
  'login.failed': '登录失败',

  // Overview page.
  'overview.loadFailed': '加载概览失败',
  'overview.running': '运行中',
  'overview.averageUptime': '平均运行时长',
  'overview.expiresIn30Minutes': '30 分钟内到期',
  'overview.maximumUptime': '最长运行时长',
  'overview.totalWallClockTime': '总运行时长',
  'overview.statsUpdatedAt': '统计更新时间',
  'overview.totalSandboxes': '沙箱总数',

  // Sandbox list page.
  'sandboxList.title': '沙箱',
  'sandboxList.nameOrId': '名称 / ID',
  'sandboxList.createdAt': '创建时间',
  'sandboxList.filterByStatus': '按状态筛选',
  'sandboxList.loadFailed': '加载失败',

  // Sandbox create page.
  'sandboxCreate.created': '沙箱创建成功',
  'sandboxCreate.failed': '创建失败',
  'sandboxCreate.imageUri': '镜像 URI',
  'sandboxCreate.imageRequired': '请输入镜像',
  'sandboxCreate.entrypoint': '入口命令（空格分隔）',
  'sandboxCreate.entrypointRequired': '请输入入口命令',
  'sandboxCreate.cpu': 'CPU',
  'sandboxCreate.cpuRequired': '请输入 CPU',
  'sandboxCreate.memory': '内存',
  'sandboxCreate.memoryRequired': '请输入内存',
  'sandboxCreate.timeout': '超时时间（秒）',
  'sandboxCreate.environmentVariables': '环境变量',
  'sandboxCreate.addEnvironmentVariable': '添加环境变量',
  'sandboxCreate.metadata': '元数据',
  'sandboxCreate.addMetadata': '添加元数据',
  'sandboxCreate.submit': '提交',
  'sandboxCreate.valuePlaceholder': '值',
  'sandboxCreate.namePlaceholder': '名称',

  // Sandbox detail page.
  'sandboxDetail.loadFailed': '加载失败',
  'sandboxDetail.adminTenantWarning': '管理员必须提供 tenant= URL 参数才能查看详情',
  'sandboxDetail.deleteTitle': '删除此沙箱？',
  'sandboxDetail.deleteContent': '此操作无法撤销。',
  'sandboxDetail.deleted': '已删除',
  'sandboxDetail.deleteFailed': '删除失败',
  'sandboxDetail.renewed': '已续期',
  'sandboxDetail.renewFailed': '续期失败',
  'sandboxDetail.pauseRequested': '已请求暂停',
  'sandboxDetail.resumeRequested': '已请求恢复',
  'sandboxDetail.actionFailed': '操作失败',
  'sandboxDetail.endpointFailed': '获取端点失败',
  'sandboxDetail.missingTenant': '缺少租户参数',
  'sandboxDetail.missingTenantDescription': '请从“所有租户”沙箱列表打开此页面，或访问 {path}',
  'sandboxDetail.runtimeLogs': '运行时日志',
  'sandboxDetail.liveLifecycle': '实时（Lifecycle）',
  'sandboxDetail.archivedNodeAgent': '归档（Node Agent）',
  'sandboxDetail.endpoint': '端点',
  'sandboxDetail.message': '消息',
  'sandboxDetail.getEndpoint': '获取端点',
  'sandboxDetail.renewDescription':
    '在当前 expiresAt 值的基础上增加指定小时数。若未设置到期时间，则从当前时间开始计算。',
  'sandboxDetail.hours': '小时',

  // Sandbox lifecycle states (display only; API values stay canonical).
  'sandbox.status.pending': '等待中',
  'sandbox.status.running': '运行中',
  'sandbox.status.pausing': '暂停中',
  'sandbox.status.paused': '已暂停',
  'sandbox.status.resuming': '恢复中',
  'sandbox.status.stopping': '停止中',
  'sandbox.status.terminated': '已终止',
  'sandbox.status.failed': '失败',

  // Diagnostic scopes (display only; API values stay canonical).
  'scope.container': '容器',
  'scope.lifecycle': '生命周期',
  'scope.runtime': '运行时',
  'scope.network': '网络',
  'scope.process': '进程',
  'scope.all': '全部',

  // Live log panel.
  'logPanel.adminTenantWarning': '管理员必须指定租户才能查看日志',
  'logPanel.loadFailed': '加载日志失败',
  'logPanel.selectRefresh': '点击“刷新”加载日志',
  'logPanel.tooLarge': '日志过大，请下载后查看。',
  'logPanel.download': '下载日志（{length} 字节）',
  'logPanel.autoRefresh': '自动刷新：10 秒',
  'logPanel.truncated': '服务器已截断响应。请使用 CLI 或提高保留设置以获取完整日志。',

  // Archived log panel.
  'archivePanel.adminTenantWarning': '管理员必须指定租户才能查看归档日志',
  'archivePanel.loadFailed': '加载归档日志失败',
  'archivePanel.notEnabled': '未启用归档日志',
  'archivePanel.enableHint':
    '请在集群中启用 node-agent，并在 BFF 中配置只读 OSS 凭据以使用归档日志。BFF 无法集中读取文件 sink。你仍可使用“实时（Lifecycle）”标签页。',
  'archivePanel.retry': '重试',
  'archivePanel.refresh': '刷新归档',
  'archivePanel.objectKey': 'OSS: {objectKey}',
  'archivePanel.truncated': '内容已截断至 maxBytes，请从 OSS 下载完整对象。',
  'archivePanel.selectRefresh': '点击“刷新归档”从 node-agent 存储加载日志',

  // Log viewer search controls.
  'logViewer.searchPlaceholder': '搜索日志…',
  'logViewer.filterLinesTitle': '仅显示匹配行',
  'logViewer.previousMatch': '上一个匹配项',
  'logViewer.nextMatch': '下一个匹配项',
};

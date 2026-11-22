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

/**
 * English message dictionary. These values intentionally match the console's
 * existing wording so selecting English does not change any displayed text.
 *
 * This object is the source of truth for message keys: both the locale
 * dictionaries and `t()` are typed from it, so a missing key fails the build.
 */
export const en = {
  'common.language': 'Language',
  'common.refresh': 'Refresh',
  'common.cancel': 'Cancel',
  'common.status': 'Status',
  'common.image': 'Image',
  'common.uptime': 'Uptime',
  'common.remaining': 'Remaining',
  'common.actions': 'Actions',
  'common.overview': 'Overview',
  'common.logs': 'Logs',
  'common.createSandbox': 'Create Sandbox',
  'common.renew': 'Renew',
  'common.delete': 'Delete',
  'common.pause': 'Pause',
  'common.resume': 'Resume',
  'common.shell': 'Shell',

  'nav.sandboxes': 'Sandboxes',
  'nav.sandboxesAllTenants': 'Sandboxes (All Tenants)',
  'nav.snapshots': 'Snapshots',
  'nav.pool': 'Pool',
  'nav.diagnostics': 'Diagnostics',
  'nav.platform': 'Platform',
  'nav.componentHealth': 'Component Health',
  'nav.version': 'Version',
  'nav.k8sWorkloads': 'K8s Workloads',
  'nav.k8sEvents': 'K8s Events',
  'nav.admin': 'Admin',
  'nav.platformFallback': 'platform',
  'nav.signOut': 'Sign Out',

  // Login page.
  'login.title': 'OpenSandbox Console',
  'login.apiKeyNotice':
    'The API key is sent only to the BFF. The browser does not store the Lifecycle key.',
  'login.tenantTab': 'Tenant User',
  'login.tenantApiKey': 'Tenant API Key',
  'login.tenantApiKeyRequired': 'Enter an API key',
  'login.adminTab': 'Platform Admin',
  'login.adminToken': 'BFF Admin Token',
  'login.adminTokenRequired': 'Enter an admin token',
  'login.signIn': 'Sign In',
  'login.tenantSuccess': 'Tenant login successful',
  'login.adminSuccess': 'Admin login successful',
  'login.failed': 'Login failed',

  // Overview page.
  'overview.loadFailed': 'Failed to load overview',
  'overview.running': 'Running',
  'overview.averageUptime': 'Average Uptime',
  'overview.expiresIn30Minutes': 'Expires in 30 Minutes',
  'overview.maximumUptime': 'Maximum Uptime',
  'overview.totalWallClockTime': 'Total Wall-Clock Time',
  'overview.statsUpdatedAt': 'Stats Updated At',
  'overview.totalSandboxes': 'Total Sandboxes',

  // Sandbox list page.
  'sandboxList.title': 'Sandbox',
  'sandboxList.nameOrId': 'Name / ID',
  'sandboxList.createdAt': 'Created At',
  'sandboxList.filterByStatus': 'Filter by status',
  'sandboxList.loadFailed': 'Failed to load',

  // Sandbox create page.
  'sandboxCreate.created': 'Sandbox created',
  'sandboxCreate.failed': 'Failed to create',
  'sandboxCreate.imageUri': 'Image URI',
  'sandboxCreate.imageRequired': 'Enter an image',
  'sandboxCreate.entrypoint': 'Entrypoint (space-separated)',
  'sandboxCreate.entrypointRequired': 'Enter an entrypoint',
  'sandboxCreate.cpu': 'CPU',
  'sandboxCreate.cpuRequired': 'Enter CPU',
  'sandboxCreate.memory': 'Memory',
  'sandboxCreate.memoryRequired': 'Enter memory',
  'sandboxCreate.timeout': 'Timeout (seconds)',
  'sandboxCreate.environmentVariables': 'Environment Variables',
  'sandboxCreate.addEnvironmentVariable': 'Add Environment Variable',
  'sandboxCreate.metadata': 'Metadata',
  'sandboxCreate.addMetadata': 'Add Metadata',
  'sandboxCreate.submit': 'Submit',
  'sandboxCreate.valuePlaceholder': 'value',
  'sandboxCreate.namePlaceholder': 'name',

  // Sandbox detail page.
  'sandboxDetail.loadFailed': 'Failed to load',
  'sandboxDetail.adminTenantWarning':
    'Admins must include the tenant= URL parameter to view details',
  'sandboxDetail.deleteTitle': 'Delete this sandbox?',
  'sandboxDetail.deleteContent': 'This action cannot be undone.',
  'sandboxDetail.deleted': 'Deleted',
  'sandboxDetail.deleteFailed': 'Failed to delete',
  'sandboxDetail.renewed': 'Renewed',
  'sandboxDetail.renewFailed': 'Failed to renew',
  'sandboxDetail.pauseRequested': 'Pause requested',
  'sandboxDetail.resumeRequested': 'Resume requested',
  'sandboxDetail.actionFailed': 'Action failed',
  'sandboxDetail.endpointFailed': 'Failed to get endpoint',
  'sandboxDetail.missingTenant': 'Missing tenant parameter',
  'sandboxDetail.missingTenantDescription':
    'Open this page from the All Tenants sandbox list, or visit {path}',
  'sandboxDetail.runtimeLogs': 'Runtime Logs',
  'sandboxDetail.liveLifecycle': 'Live (Lifecycle)',
  'sandboxDetail.archivedNodeAgent': 'Archived (Node Agent)',
  'sandboxDetail.endpoint': 'Endpoint',
  'sandboxDetail.message': 'Message',
  'sandboxDetail.getEndpoint': 'Get Endpoint',
  'sandboxDetail.renewDescription':
    'Adds the specified number of hours to the current expiresAt value. If no expiration is set, the duration starts now.',
  'sandboxDetail.hours': 'hours',

  // Sandbox lifecycle states (display only; API values stay canonical).
  'sandbox.status.pending': 'Pending',
  'sandbox.status.running': 'Running',
  'sandbox.status.pausing': 'Pausing',
  'sandbox.status.paused': 'Paused',
  'sandbox.status.resuming': 'Resuming',
  'sandbox.status.stopping': 'Stopping',
  'sandbox.status.terminated': 'Terminated',
  'sandbox.status.failed': 'Failed',

  // Diagnostic scopes (display only; API values stay canonical).
  'scope.container': 'container',
  'scope.lifecycle': 'lifecycle',
  'scope.runtime': 'runtime',
  'scope.network': 'network',
  'scope.process': 'process',
  'scope.all': 'all',

  // Live log panel.
  'logPanel.adminTenantWarning': 'Admins must specify a tenant to view logs',
  'logPanel.loadFailed': 'Failed to load logs',
  'logPanel.selectRefresh': 'Select “Refresh” to load logs',
  'logPanel.tooLarge': 'Log is too large. Download it to view.',
  'logPanel.download': 'Download logs ({length} bytes)',
  'logPanel.autoRefresh': 'Auto-refresh: 10s',
  'logPanel.truncated':
    'The server truncated the response. Use the CLI or increase the retention setting to retrieve the full log.',

  // Archived log panel.
  'archivePanel.adminTenantWarning': 'Admins must specify a tenant to view archived logs',
  'archivePanel.loadFailed': 'Failed to load archived logs',
  'archivePanel.notEnabled': 'Archived logs are not enabled',
  'archivePanel.enableHint':
    'Enable node-agent in the cluster and configure read-only OSS credentials in the BFF to use archived logs. The BFF cannot centrally read file sinks. You can still use the Live (Lifecycle) tab under Runtime Logs.',
  'archivePanel.retry': 'Retry',
  'archivePanel.refresh': 'Refresh Archive',
  'archivePanel.objectKey': 'OSS: {objectKey}',
  'archivePanel.truncated':
    'Content was truncated to maxBytes. Download the complete object from OSS.',
  'archivePanel.selectRefresh':
    'Select “Refresh Archive” to load logs from node-agent storage',

  // Log viewer search controls.
  'logViewer.searchPlaceholder': 'Search logs…',
  'logViewer.filterLinesTitle': 'Show matching lines only',
  'logViewer.previousMatch': 'Previous match',
  'logViewer.nextMatch': 'Next match',
} as const;

export type MessageKey = keyof typeof en;

/** Shape every locale dictionary must implement. */
export type Messages = Record<MessageKey, string>;

export type TranslateParams = Record<string, string | number>;

export type Translate = (key: MessageKey, params?: TranslateParams) => string;

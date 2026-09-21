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

import { defineConfig } from "vitepress";

const sdkSidebar = [
  {
    text: "Sandbox SDKs",
    collapsed: false,
    items: [
      { text: "Overview", link: "/sdks/" },
      { text: "Python", link: "/sdks/python" },
      { text: "JavaScript", link: "/sdks/javascript" },
      { text: "Kotlin / Java", link: "/sdks/kotlin" },
      { text: "Go", link: "/sdks/go" },
      { text: "C#", link: "/sdks/csharp" },
    ],
  },
  {
    text: "SDK Features",
    items: [
      { text: "Client Pool", link: "/guides/client-pool" },
      { text: "Observability", link: "/sdks/observability" },
    ],
  },
  {
    text: "CLI",
    items: [{ text: "CLI Reference", link: "/cli/" }],
  },
  {
    text: "MCP",
    collapsed: false,
    items: [{ text: "MCP Server", link: "/sdks/mcp" }],
  },
];

export default defineConfig({
  title: "OpenSandbox",
  description: "Universal Sandbox Infrastructure for AI Applications",
  cleanUrls: true,
  lastUpdated: true,
  base: process.env.DOCS_BASE || "/",
  ignoreDeadLinks: [/^https?:\/\/localhost/],
  // Release notes (docs/releases/*) are GitHub Release bodies referenced
  // verbatim by the umbrella release workflow — not docs-site pages.
  srcExclude: ["README.md", "releases/**"],

  head: [
    ["link", { rel: "icon", type: "image/svg+xml", href: "/favicon.svg" }],
    [
      "meta",
      { property: "og:title", content: "OpenSandbox Documentation" },
    ],
    [
      "meta",
      {
        property: "og:description",
        content: "Universal Sandbox Infrastructure for AI Applications",
      },
    ],
  ],

  themeConfig: {
    logo: "/images/logo.svg",
    siteTitle: "OpenSandbox",

    nav: [
      { text: "Getting Started", link: "/getting-started/" },
      { text: "Guides", link: "/guides/" },
      {
        text: "Reference",
        items: [
          { text: "SDKs", link: "/sdks/" },
          { text: "API Specs", link: "/api/" },
          { text: "Components", link: "/components/" },
          { text: "Kubernetes", link: "/kubernetes/" },
          { text: "Migration Guides", link: "/reference/execd-path-migration" },
        ],
      },
      { text: "Examples", link: "/examples/" },
      { text: "Community", link: "/community/contributing" },
    ],

    sidebar: {
      "/getting-started/": [
        {
          text: "Getting Started",
          items: [
            { text: "Quick Start", link: "/getting-started/" },
            { text: "Installation", link: "/getting-started/installation" },
            {
              text: "Configuration",
              link: "/getting-started/configuration",
            },
          ],
        },
        {
          text: "Next Steps",
          items: [
            { text: "Architecture", link: "/architecture/" },
            { text: "Guides", link: "/guides/" },
            { text: "SDKs", link: "/sdks/" },
          ],
        },
      ],

      "/architecture/": [
        {
          text: "Architecture",
          items: [
            { text: "Overview", link: "/architecture/" },
            {
              text: "Single-Host Network",
              link: "/architecture/single-host-network",
            },
            {
              text: "Network Isolation",
              link: "/architecture/network-isolation",
            },
          ],
        },
      ],

      // Specific guide routes must precede /guides/ for VitePress prefix matching.
      "/guides/client-pool": sdkSidebar,
      "/guides/": [
        {
          text: "Guides",
          items: [
            { text: "Overview", link: "/guides/" },
            { text: "Credential Vault", link: "/guides/credential-vault" },
            { text: "Secure Access", link: "/guides/secure-access" },
            { text: "Secure Container", link: "/guides/secure-container" },
            { text: "Multi-Tenancy", link: "/guides/multi-tenancy" },
            { text: "Isolation Sessions", link: "/guides/isolation-sessions" },
            { text: "Pause & Resume", link: "/guides/pause-resume" },
            { text: "Lifecycle Hooks", link: "/guides/lifecycle-hooks" },
            { text: "Windows Sandbox", link: "/guides/windows-sandbox" },
          ],
        },
      ],

      "/sdks/": sdkSidebar,

      "/components/": [
        {
          text: "Components",
          items: [
            { text: "Overview", link: "/components/" },
            { text: "Server", link: "/components/server" },
            { text: "Execd", link: "/components/execd" },
            { text: "Ingress", link: "/components/ingress" },
            { text: "Egress", link: "/components/egress" },
            { text: "Node Agent", link: "/components/node-agent" },
          ],
        },
      ],

      "/kubernetes/": [
        {
          text: "Kubernetes",
          items: [
            { text: "Overview", link: "/kubernetes/" },
            { text: "Deployment", link: "/kubernetes/deployment" },
            {
              text: "QEMU VMState Snapshots",
              link: "/kubernetes/qemu-vmstate-snapshots",
            },
          ],
        },
      ],

      "/api/": [
        {
          text: "API Reference",
          items: [{ text: "OpenAPI Specs", link: "/api/" }],
        },
      ],

      "/cli/": sdkSidebar,

      "/examples/": [
        {
          text: "Examples",
          items: [{ text: "Overview", link: "/examples/" }],
        },
        {
          text: "Coding Agents",
          collapsed: false,
          items: [
            { text: "Claude Code", link: "/examples/claude-code" },
            { text: "Gemini CLI", link: "/examples/gemini-cli" },
            { text: "Codex CLI", link: "/examples/codex-cli" },
            { text: "OpenCode", link: "/examples/opencode" },
            { text: "Qwen Code", link: "/examples/qwen-code" },
            { text: "Kimi CLI", link: "/examples/kimi-cli" },
            { text: "LangGraph", link: "/examples/langgraph" },
            { text: "Google ADK", link: "/examples/google-adk" },
            { text: "OpenClaw", link: "/examples/openclaw" },
            { text: "NullClaw", link: "/examples/nullclaw" },
          ],
        },
        {
          text: "Browser & Desktop",
          collapsed: false,
          items: [
            { text: "Chrome", link: "/examples/chrome" },
            { text: "Playwright", link: "/examples/playwright" },
            { text: "Desktop", link: "/examples/desktop" },
            { text: "VS Code", link: "/examples/vscode" },
          ],
        },
        {
          text: "Core Usage",
          collapsed: false,
          items: [
            { text: "Code Interpreter", link: "/examples/code-interpreter" },
            { text: "AIO Sandbox", link: "/examples/aio-sandbox" },
            { text: "Agent Sandbox", link: "/examples/agent-sandbox" },
            { text: "Windows", link: "/examples/windows" },
            { text: "AKS Kata", link: "/examples/aks-kata" },
            { text: "Harbor Evaluation", link: "/examples/harbor-evaluation" },
          ],
        },
        {
          text: "Storage",
          collapsed: false,
          items: [
            {
              text: "Host Volume Mount",
              link: "/examples/host-volume-mount",
            },
            {
              text: "Docker PVC Volume",
              link: "/examples/docker-pvc-volume-mount",
            },
            {
              text: "Docker OSSFS Volume",
              link: "/examples/docker-ossfs-volume-mount",
            },
            {
              text: "Kubernetes PVC",
              link: "/examples/kubernetes-pvc-volume-mount",
            },
          ],
        },
      ],

      "/community/": [
        {
          text: "Community",
          items: [
            { text: "Contributing", link: "/community/contributing" },
            { text: "Code of Conduct", link: "/community/code-of-conduct" },
            {
              text: "Enhancement Proposals",
              link: "/community/oseps",
            },
          ],
        },
        {
          text: "Releases",
          items: [
            {
              text: "Versioning",
              link: "/community/versioning",
            },
            {
              text: "Release Automation",
              link: "/community/release-automation",
            },
            {
              text: "Release Verification",
              link: "/community/release-verification",
            },
          ],
        },
      ],

      "/reference/": [
        {
          text: "Reference",
          items: [
            {
              text: "Execd Path Migration",
              link: "/reference/execd-path-migration",
            },
            {
              text: "Snapshot Store Migration",
              link: "/reference/snapshot-store-migration",
            },
            {
              text: "Code Interpreter Image Migration",
              link: "/reference/code-interpreter-image-migration",
            },
          ],
        },
      ],
    },

    editLink: {
      pattern:
        "https://github.com/opensandbox-group/OpenSandbox/edit/main/docs/:path",
      text: "Edit this page on GitHub",
    },

    socialLinks: [
      {
        icon: "github",
        link: "https://github.com/opensandbox-group/OpenSandbox",
      },
    ],

    footer: {
      message: "Released under the Apache 2.0 License.",
      copyright: "Copyright © 2024-present OpenSandbox Contributors",
    },

    search: {
      provider: "local",
    },

    outline: {
      level: [2, 3],
    },
  },
});

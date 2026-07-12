import { defineConfig } from "vitepress";

export default defineConfig({
  title: "OpenSandbox",
  description: "Universal Sandbox Infrastructure for AI Applications",
  cleanUrls: true,
  lastUpdated: true,
  base: process.env.DOCS_BASE || "/",
  ignoreDeadLinks: [/^https?:\/\/localhost/],
  srcExclude: ["README.md"],

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
      { text: "Guides", link: "/guides/credential-vault" },
      {
        text: "Reference",
        items: [
          { text: "SDKs", link: "/sdks/" },
          { text: "API Specs", link: "/api/" },
          { text: "CLI", link: "/cli/" },
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
            { text: "Guides", link: "/guides/credential-vault" },
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

      "/guides/": [
        {
          text: "Guides",
          items: [
            { text: "Credential Vault", link: "/guides/credential-vault" },
            { text: "Secure Container", link: "/guides/secure-container" },
            { text: "Pause & Resume", link: "/guides/pause-resume" },
            { text: "Windows Sandbox", link: "/guides/windows-sandbox" },
          ],
        },
      ],

      "/sdks/": [
        {
          text: "Sandbox SDKs",
          collapsed: false,
          items: [
            { text: "Overview", link: "/sdks/" },
            { text: "Python", link: "/sdks/python" },
            { text: "JavaScript", link: "/sdks/javascript" },
            { text: "Kotlin", link: "/sdks/kotlin" },
            { text: "Go", link: "/sdks/go" },
            { text: "C#", link: "/sdks/csharp" },
          ],
        },
        {
          text: "Code Interpreter SDKs",
          collapsed: false,
          items: [
            { text: "Python", link: "/sdks/code-interpreter/python" },
            {
              text: "JavaScript",
              link: "/sdks/code-interpreter/javascript",
            },
            { text: "Kotlin", link: "/sdks/code-interpreter/kotlin" },
            { text: "C#", link: "/sdks/code-interpreter/csharp" },
          ],
        },
        {
          text: "MCP",
          collapsed: false,
          items: [{ text: "MCP Server", link: "/sdks/mcp" }],
        },
      ],

      "/components/": [
        {
          text: "Components",
          items: [
            { text: "Overview", link: "/components/" },
            { text: "Server", link: "/components/server" },
            { text: "Execd", link: "/components/execd" },
            { text: "Ingress", link: "/components/ingress" },
            { text: "Egress", link: "/components/egress" },
          ],
        },
      ],

      "/kubernetes/": [
        {
          text: "Kubernetes",
          items: [
            { text: "Overview", link: "/kubernetes/" },
            { text: "Deployment", link: "/kubernetes/deployment" },
          ],
        },
      ],

      "/api/": [
        {
          text: "API Reference",
          items: [{ text: "OpenAPI Specs", link: "/api/" }],
        },
      ],

      "/cli/": [
        {
          text: "CLI",
          items: [{ text: "Reference", link: "/cli/" }],
        },
      ],

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
            { text: "RL Training", link: "/examples/rl-training" },
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

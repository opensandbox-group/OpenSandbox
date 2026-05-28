import { defineConfig } from "vitepress";
import { loadManifest } from "./scripts/docs-manifest.mjs";

const manifest = loadManifest();
const docsBase = process.env.DOCS_BASE || "/";

export default defineConfig({
  title: "OpenSandbox",
  description: "OpenSandbox documentation site for users and developers",
  appearance: "dark",
  head: [["link", { rel: "icon", type: "image/svg+xml", href: "/favicon.svg" }]],
  cleanUrls: true,
  lastUpdated: true,
  base: docsBase,
  ignoreDeadLinks: [/^https?:\/\/localhost/, /\/README$/, /\/index$/, "./contributing"],
  srcExclude: ["node_modules/**", "README_zh.md", "RELEASE_NOTE_TEMPLATE.md"],
  rewrites: manifest.rewrites,
  themeConfig: {
    logo: "/assets/logo.svg",
    search: {
      provider: "local",
    },
    socialLinks: [{ icon: "github", link: "https://github.com/alibaba/OpenSandbox" }],
    nav: manifest.nav.en,
    sidebar: {
      ...manifest.sidebar.en,
      ...manifest.sidebar.zh,
    },
    outline: {
      level: [2, 3],
    },
  },
  locales: {
    root: {
      label: "English",
      lang: "en-US",
      themeConfig: {
        nav: manifest.nav.en,
      },
    },
    zh: {
      label: "简体中文",
      lang: "zh-CN",
      themeConfig: {
        nav: manifest.nav.zh,
      },
    },
  },
});

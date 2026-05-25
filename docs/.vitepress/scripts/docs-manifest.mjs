import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);
const repoRoot = path.resolve(__dirname, "../../../");
const docsRoot = path.join(repoRoot, "docs");
const generatedRoot = path.join(docsRoot, "generated");
const manifestPath = path.join(docsRoot, ".vitepress", "generated", "manifest.json");

const blobBaseUrl = "https://github.com/alibaba/OpenSandbox/blob/main";
const treeBaseUrl = "https://github.com/alibaba/OpenSandbox/tree/main";
const rawBaseUrl = "https://raw.githubusercontent.com/alibaba/OpenSandbox/main";

const ignoredDirNames = new Set([
  ".git",
  ".github",
  "node_modules",
  ".vitepress",
  ".pytest_cache",
  "generated",
  ".venv",
  "venv",
  "__pycache__",
  "dist",
  "build",
  "target",
  "bin",
]);
const zhReadmePattern = /^README(?:[-_](?:zh|zh-cn|zh_cn))?\.md$/i;
const standardReadmePattern = /^README\.md$/i;

const sectionDefinitions = [
  {
    id: "modules",
    scanRoots: ["server", "components", "sandboxes", "kubernetes", "specs", "sdks"],
    includeDevelopment: true,
  },
  {
    id: "examples",
    scanRoots: ["examples"],
    includeDevelopment: false,
  },
  {
    id: "community",
    scanRoots: ["oseps"],
    includeDevelopment: false,
  },
];

const manualEntries = [
  {
    key: "guide-home",
    sectionId: "overview",
    slug: "overview/home",
    enPath: "README.md",
    zhPath: "docs/README_zh.md",
    titleEn: "OpenSandbox",
    titleZh: "OpenSandbox",
  },
  {
    key: "guide-architecture",
    sectionId: "overview",
    slug: "overview/architecture",
    enPath: "docs/architecture.md",
    zhPath: null,
    titleEn: "Architecture",
    titleZh: "架构设计",
  },
  {
    key: "guide-release-verification",
    sectionId: "overview",
    slug: "overview/release-verification",
    enPath: "docs/release-verification.md",
    zhPath: null,
    titleEn: "Release Verification",
    titleZh: "发布验签",
  },
  {
    key: "guide-network",
    sectionId: "modules",
    slug: "design/single-host-network",
    enPath: "docs/single_host_network.md",
    zhPath: null,
    titleEn: "Single Host Network",
    titleZh: "单机场景网络设计",
  },
  {
    key: "community-contributing",
    sectionId: "community",
    slug: "community/contributing",
    enPath: "CONTRIBUTING.md",
    zhPath: null,
    titleEn: "Contributing",
    titleZh: "参与贡献",
  },
  {
    key: "community-code-of-conduct",
    sectionId: "community",
    slug: "community/code-of-conduct",
    enPath: "CODE_OF_CONDUCT.md",
    zhPath: null,
    titleEn: "Code of Conduct",
    titleZh: "行为准则",
  },
];

const moduleGroupLabels = {
  en: {
    sdks: "SDKs",
    specs: "Specs & API",
    server: "Server",
    components: "Components",
    sandboxes: "Sandboxes",
    kubernetes: "Kubernetes",
    design: "Design",
  },
  zh: {
    sdks: "SDKs",
    specs: "Specs & API",
    server: "Server",
    components: "Components",
    sandboxes: "Sandboxes",
    kubernetes: "Kubernetes",
    design: "设计",
  },
};

const communityGroupLabels = {
  en: {
    community: "Community",
    oseps: "OSEPs",
  },
  zh: {
    community: "社区",
    oseps: "OSEPs",
  },
};

const shortTitleByPath = {
  "sdks/code-interpreter/javascript/README.md": "Code Interpreter JS SDK",
  "sdks/code-interpreter/kotlin/README.md": "Code Interpreter Kotlin SDK",
  "sdks/code-interpreter/python/README.md": "Code Interpreter Python SDK",
  "sdks/code-interpreter/csharp/README.md": "Code Interpreter C# SDK",
  "sdks/sandbox/javascript/README.md": "Sandbox JS SDK",
  "sdks/sandbox/kotlin/README.md": "Sandbox Kotlin SDK",
  "sdks/sandbox/python/README.md": "Sandbox Python SDK",
  "sdks/sandbox/csharp/README.md": "Sandbox C# SDK",
  "sdks/mcp/sandbox/python/README.md": "MCP Sandbox Python SDK",
  "cli/README.md": "CLI (Python)",
  "sdks/sandbox/kotlin/sandbox-api/build/generated/api/execd/README.md": "Sandbox Execd API (Kotlin)",
  "sdks/sandbox/kotlin/sandbox-api/build/generated/api/lifecycle/README.md": "Sandbox Lifecycle API (Kotlin)",

  "examples/agent-sandbox/README.md": "Agent Sandbox",
  "examples/aio-sandbox/README.md": "AIO Sandbox",
  "examples/chrome/README.md": "Chrome",
  "examples/claude-code/README.md": "Claude Code",
  "examples/code-interpreter/README.md": "Code Interpreter",
  "examples/codex-cli/README.md": "Codex CLI",
  "examples/desktop/README.md": "Desktop (VNC)",
  "examples/gemini-cli/README.md": "Gemini CLI",
  "examples/google-adk/README.md": "Google ADK",
  "examples/host-volume-mount/README.md": "Host Volume Mount",
  "examples/langgraph/README.md": "LangGraph",
  "examples/playwright/README.md": "Playwright",
  "examples/README.md": "Examples Overview",
  "examples/rl-training/README.md": "RL Training",
  "examples/vscode/README.md": "VS Code",

  "server/README.md": "Server",
  "server/DEVELOPMENT.md": "Server Development",
  "components/ingress/README.md": "Ingress",
  "components/ingress/DEVELOPMENT.md": "Ingress Development",
  "components/egress/README.md": "Egress Sidecar",
  "components/execd/README.md": "execd",
  "components/execd/DEVELOPMENT.md": "execd Development",
  "sandboxes/code-interpreter/README.md": "Code Interpreter Runtime",
  "kubernetes/README.md": "Kubernetes Controller",
  "kubernetes/examples/task-executor/README.md": "Task Executor",
  "kubernetes/examples/controller/README.md": "Controller Example",
  "oseps/README.md": "OSEP Overview",
};

const shortTitleByPathZh = {
  "sdks/code-interpreter/javascript/README.md": "代码解释器 JS SDK",
  "sdks/code-interpreter/kotlin/README.md": "代码解释器 Kotlin SDK",
  "sdks/code-interpreter/python/README.md": "代码解释器 Python SDK",
  "sdks/code-interpreter/csharp/README.md": "代码解释器 C# SDK",
  "sdks/sandbox/javascript/README.md": "沙箱 JS SDK",
  "sdks/sandbox/kotlin/README.md": "沙箱 Kotlin SDK",
  "sdks/sandbox/python/README.md": "沙箱 Python SDK",
  "sdks/sandbox/csharp/README.md": "沙箱 C# SDK",
  "sdks/mcp/sandbox/python/README.md": "MCP 沙箱 Python SDK",
  "cli/README.md": "CLI（Python）",
  "sdks/sandbox/kotlin/sandbox-api/build/generated/api/execd/README.md": "沙箱 Execd API（Kotlin）",
  "sdks/sandbox/kotlin/sandbox-api/build/generated/api/lifecycle/README.md": "沙箱生命周期 API（Kotlin）",

  "examples/agent-sandbox/README.md": "Agent Sandbox",
  "examples/aio-sandbox/README.md": "AIO 沙箱",
  "examples/chrome/README.md": "Chrome",
  "examples/claude-code/README.md": "Claude Code",
  "examples/code-interpreter/README.md": "代码解释器",
  "examples/codex-cli/README.md": "Codex CLI",
  "examples/desktop/README.md": "桌面环境（VNC）",
  "examples/gemini-cli/README.md": "Gemini CLI",
  "examples/google-adk/README.md": "Google ADK",
  "examples/host-volume-mount/README.md": "宿主机目录挂载",
  "examples/langgraph/README.md": "LangGraph",
  "examples/playwright/README.md": "Playwright",
  "examples/README.md": "示例总览",
  "examples/rl-training/README.md": "强化学习训练",
  "examples/vscode/README.md": "VS Code",

  "server/README.md": "Server",
  "server/DEVELOPMENT.md": "Server 开发指南",
  "components/ingress/README.md": "Ingress",
  "components/ingress/DEVELOPMENT.md": "Ingress 开发指南",
  "components/egress/README.md": "Egress Sidecar",
  "components/execd/README.md": "execd",
  "components/execd/DEVELOPMENT.md": "execd 开发指南",
  "sandboxes/code-interpreter/README.md": "代码解释器运行时",
  "kubernetes/README.md": "Kubernetes 控制器",
  "kubernetes/examples/task-executor/README.md": "Task Executor",
  "kubernetes/examples/controller/README.md": "Controller 示例",
  "oseps/README.md": "OSEP 总览",
};

function ensureDir(dirPath) {
  fs.mkdirSync(dirPath, { recursive: true });
}

function rmIfExists(targetPath) {
  if (fs.existsSync(targetPath)) {
    fs.rmSync(targetPath, { recursive: true, force: true, maxRetries: 5, retryDelay: 80 });
  }
}

function walkMarkdownFiles(absDirPath, acc = []) {
  const entries = fs.readdirSync(absDirPath, { withFileTypes: true });
  for (const entry of entries) {
    if (ignoredDirNames.has(entry.name)) {
      continue;
    }
    const absPath = path.join(absDirPath, entry.name);
    if (entry.isDirectory()) {
      walkMarkdownFiles(absPath, acc);
      continue;
    }
    if (!entry.isFile()) {
      continue;
    }
    if (entry.name.endsWith(".md")) {
      acc.push(absPath);
    }
  }
  return acc;
}

function shouldIgnoreRepoPath(repoRelPath) {
  const normalized = repoRelPath.replaceAll("\\", "/");
  const denylistFragments = [
    "/.venv/",
    "/venv/",
    "/node_modules/",
    "/docs/.vitepress/",
    "/docs/generated/",
    "/.pytest_cache/",
    "/__pycache__/",
    "/dist/",
    "/build/",
    "/target/",
    "/bin/",
  ];
  return denylistFragments.some((fragment) => normalized.includes(fragment));
}

function toRepoRelative(absPath) {
  return path.relative(repoRoot, absPath).replaceAll(path.sep, "/");
}

function readHeadingTitle(absPath, fallbackTitle) {
  if (!fs.existsSync(absPath)) {
    return fallbackTitle;
  }
  const content = fs.readFileSync(absPath, "utf8");
  const lines = content.split(/\r?\n/);
  let inFence = false;
  for (const line of lines) {
    const trimmed = line.trimStart();
    if (trimmed.startsWith("```")) {
      inFence = !inFence;
      continue;
    }
    if (inFence) {
      continue;
    }
    const matched = trimmed.match(/^#{1,3}\s+(.+)$/);
    if (matched) {
      return matched[1].trim();
    }
  }
  return fallbackTitle;
}

function normalizeTitleWhitespace(title) {
  return title.replace(/\s+/g, " ").trim();
}

function shortenOsepTitle(repoRelPath, title, locale = "en") {
  const match = repoRelPath.match(/^oseps\/(0\d{3})-(.+)\.md$/i);
  if (!match) {
    return title;
  }
  const number = match[1];
  const slug = match[2].toLowerCase();
  if (locale === "zh") {
    if (slug.includes("fqdn") && slug.includes("egress")) {
      return `OSEP-${number}: FQDN 出口访问控制`;
    }
    if (slug.includes("agent-sandbox") || slug.includes("kubernetes-sigs")) {
      return `OSEP-${number}: Kubernetes Agent Sandbox 支持`;
    }
    if (slug.includes("volume")) {
      return `OSEP-${number}: Volume 与 VolumeBinding 支持`;
    }
  }
  if (slug.includes("fqdn") && slug.includes("egress")) {
    return `OSEP-${number}: FQDN Egress Control`;
  }
  if (slug.includes("agent-sandbox") || slug.includes("kubernetes-sigs")) {
    return `OSEP-${number}: Agent Sandbox on Kubernetes`;
  }
  if (slug.includes("volume")) {
    return `OSEP-${number}: Volume & VolumeBinding Support`;
  }
  const readable = slug
    .split("-")
    .map((part) => (part.length <= 3 ? part.toUpperCase() : part.charAt(0).toUpperCase() + part.slice(1)))
    .join(" ");
  return `OSEP-${number}: ${readable}`;
}

function shortenTitleByRule(title) {
  let next = normalizeTitleWhitespace(title);
  next = next.replace(/^Alibaba\s+/i, "");
  next = next.replace(/^OpenSandbox\s+/i, "");
  next = next.replace(/\bJavaScript\/TypeScript\b/g, "JS");
  next = next.replace(/\bJava\/Kotlin\b/g, "Kotlin");
  next = next.replace(/\s+Example$/i, "");
  next = next.replace(/\s+SDK for /i, " ");
  return normalizeTitleWhitespace(next);
}

function shortenTitleByRuleZh(title) {
  let next = normalizeTitleWhitespace(title);
  next = next.replace(/^Alibaba\s+/i, "");
  next = next.replace(/^OpenSandbox\s+/i, "");
  next = next.replace(/\bJavaScript\/TypeScript\b/g, "JS");
  next = next.replace(/\bJava\/Kotlin\b/g, "Kotlin");
  next = next.replace(/\s+Example$/i, " 示例");
  next = next.replace(/\s+SDK for /i, " ");
  return normalizeTitleWhitespace(next);
}

function getShortTitle(repoRelPath, currentTitle, locale = "en") {
  if (locale === "zh" && shortTitleByPathZh[repoRelPath]) {
    return shortTitleByPathZh[repoRelPath];
  }
  if (locale !== "zh" && shortTitleByPath[repoRelPath]) {
    return shortTitleByPath[repoRelPath];
  }
  if (/^oseps\/0\d{3}-.+\.md$/i.test(repoRelPath)) {
    return shortenOsepTitle(repoRelPath, currentTitle, locale);
  }
  if (locale === "zh") {
    return shortenTitleByRuleZh(currentTitle);
  }
  return shortenTitleByRule(currentTitle);
}

function toYamlString(value) {
  return `"${String(value).replace(/\\/g, "\\\\").replace(/"/g, '\\"')}"`;
}

function normalizeSlugFromPath(relPath) {
  const normalized = relPath.replaceAll("\\", "/");
  const dirName = path.posix.dirname(normalized);
  const baseName = path.posix.basename(normalized);
  const lowerBase = baseName.toLowerCase();

  if (lowerBase === "readme.md" || zhReadmePattern.test(baseName)) {
    return dirName === "." ? "overview/home" : `${dirName}/readme`;
  }
  if (lowerBase === "development.md") {
    return `${dirName}/development`;
  }
  return normalized.replace(/\.md$/i, "");
}

function resolveZhCandidate(repoRelPath, readmeCandidatesByDir) {
  const dir = path.posix.dirname(repoRelPath);
  const candidates = readmeCandidatesByDir.get(dir) ?? [];
  for (const candidate of candidates) {
    if (candidate.toLowerCase() !== "readme.md") {
      return `${dir}/${candidate}`;
    }
  }
  return null;
}

function buildGeneratedAssetPath(locale, routeSlug, resolvedRepoPath) {
  const normalized = resolvedRepoPath.replaceAll("\\", "/");
  if (!normalized.startsWith("docs/assets/")) {
    return null;
  }
  const generatedDir = path.posix.dirname(`generated/${locale}/${routeSlug}.md`);
  const assetPath = normalized.replace(/^docs\//, "");
  let relativePath = path.posix.relative(generatedDir, assetPath);
  if (!relativePath || relativePath === "") {
    relativePath = "./";
  }
  if (!relativePath.startsWith(".") && !relativePath.startsWith("/")) {
    relativePath = `./${relativePath}`;
  }
  return relativePath;
}

function normalizeLinkTarget(target, sourceDirRel, isImage, routeSlug, locale) {
  if (
    target.startsWith("http://") ||
    target.startsWith("https://") ||
    target.startsWith("mailto:") ||
    target.startsWith("#") ||
    target.startsWith("data:") ||
    target.startsWith("/")
  ) {
    return target;
  }

  const [rawPath, hashFragment] = target.split("#");
  const resolvedPath = path.posix.normalize(path.posix.join(sourceDirRel, rawPath));
  const localAssetPath = isImage ? buildGeneratedAssetPath(locale, routeSlug, resolvedPath) : null;
  if (localAssetPath) {
    if (hashFragment) {
      return `${localAssetPath}#${hashFragment}`;
    }
    return localAssetPath;
  }

  const urlBase = isImage
    ? `${rawBaseUrl}/${resolvedPath}`
    : fs.existsSync(path.join(repoRoot, resolvedPath)) &&
      fs.statSync(path.join(repoRoot, resolvedPath)).isDirectory()
      ? `${treeBaseUrl}/${resolvedPath}`
      : `${blobBaseUrl}/${resolvedPath}`;

  if (hashFragment) {
    return `${urlBase}#${hashFragment}`;
  }
  return urlBase;
}

function rewriteRelativeLinks(markdown, sourceRelPath, routeSlug, locale) {
  const sourceDirRel = path.posix.dirname(sourceRelPath);

  const withMarkdownLinks = markdown.replace(
    /(!?)\[([^\]]*?)\]\(([^)]+)\)/g,
    (_match, imageMark, text, linkValue) => {
      const trimmed = linkValue.trim();
      if (!trimmed) {
        return _match;
      }
      const firstSpace = trimmed.search(/\s/);
      const target = firstSpace === -1 ? trimmed : trimmed.slice(0, firstSpace);
      const trailing = firstSpace === -1 ? "" : trimmed.slice(firstSpace);
      const rewrittenTarget = normalizeLinkTarget(target, sourceDirRel, imageMark === "!", routeSlug, locale);
      return `${imageMark}[${text}](${rewrittenTarget}${trailing})`;
    },
  );

  return withMarkdownLinks.replace(
    /<img([^>]*?)src=(["'])([^"']+)\2([^>]*)>/gi,
    (matched, before, quote, src, after) => {
      const rewritten = normalizeLinkTarget(src, sourceDirRel, true, routeSlug, locale);
      return `<img${before}src=${quote}${rewritten}${quote}${after}>`;
    },
  );
}

function renderPageSource({ locale, title, sourceRelPath, routeSlug, passthrough = false }) {
  const sourceAbsPath = path.join(repoRoot, sourceRelPath);
  const sourceMarkdown = fs.readFileSync(sourceAbsPath, "utf8");
  const displayTitle = title || readHeadingTitle(sourceAbsPath, path.posix.basename(sourceRelPath, ".md"));

  let body = sourceMarkdown;
  if (!passthrough) {
    body = rewriteRelativeLinks(sourceMarkdown, sourceRelPath, routeSlug, locale);
  }

  const sourceUrl = `${blobBaseUrl}/${sourceRelPath}`;
  const sourceNotice =
    locale === "zh"
      ? `> 此页内容来自仓库源文件：[\`${sourceRelPath}\`](${sourceUrl})`
      : `> This page is sourced from: [\`${sourceRelPath}\`](${sourceUrl})`;


  return `---\ntitle: ${toYamlString(displayTitle)}\n---\n\n${body}\n\n---\n\n${sourceNotice}\n`;
}

function prettifyPathTitle(repoRelPath) {
  const dirPath = path.posix.dirname(repoRelPath);
  if (dirPath === "." || dirPath === "docs") {
    return "Overview";
  }
  return dirPath
    .split("/")
    .map((part) =>
      part
        .replaceAll("-", " ")
        .replaceAll("_", " ")
        .replace(/\b\w/g, (ch) => ch.toUpperCase()),
    )
    .join(" / ");
}

function collectAutoEntries() {
  const readmeCandidatesByDir = new Map();
  const entries = [];

  for (const section of sectionDefinitions) {
    for (const scanRoot of section.scanRoots) {
      const absScanRoot = path.join(repoRoot, scanRoot);
      if (!fs.existsSync(absScanRoot)) {
        continue;
      }
      const files = walkMarkdownFiles(absScanRoot);
      for (const absPath of files) {
        const repoRelPath = toRepoRelative(absPath);
        if (shouldIgnoreRepoPath(repoRelPath)) {
          continue;
        }
        const fileName = path.posix.basename(repoRelPath);
        const dirName = path.posix.dirname(repoRelPath);

        if (zhReadmePattern.test(fileName)) {
          const arr = readmeCandidatesByDir.get(dirName) ?? [];
          arr.push(fileName);
          readmeCandidatesByDir.set(dirName, arr);
        }
      }
    }
  }

  for (const section of sectionDefinitions) {
    for (const scanRoot of section.scanRoots) {
      const absScanRoot = path.join(repoRoot, scanRoot);
      if (!fs.existsSync(absScanRoot)) {
        continue;
      }
      const files = walkMarkdownFiles(absScanRoot);
      for (const absPath of files) {
        const repoRelPath = toRepoRelative(absPath);
        if (shouldIgnoreRepoPath(repoRelPath)) {
          continue;
        }
        const fileName = path.posix.basename(repoRelPath);
        if (zhReadmePattern.test(fileName) && !standardReadmePattern.test(fileName)) {
          continue;
        }

        const isReadme = standardReadmePattern.test(fileName);
        const isDevelopment = fileName === "DEVELOPMENT.md";
        const isOsepDoc = section.id === "community" && /^0\d{3}-.+\.md$/i.test(fileName);
        if (!isReadme && !(section.includeDevelopment && isDevelopment) && !isOsepDoc) {
          continue;
        }

        const zhCandidate = isReadme ? resolveZhCandidate(repoRelPath, readmeCandidatesByDir) : null;
        const entryKey = `auto:${section.id}:${repoRelPath}`;
        const slug = normalizeSlugFromPath(repoRelPath);
        const titleFallback = isDevelopment ? `${prettifyPathTitle(repoRelPath)} Development` : prettifyPathTitle(repoRelPath);
        entries.push({
          key: entryKey,
          sectionId: section.id,
          slug,
          enPath: repoRelPath,
          zhPath: zhCandidate,
          titleEn: getShortTitle(repoRelPath, readHeadingTitle(absPath, titleFallback), "en"),
          titleZh: getShortTitle(
            repoRelPath,
            readHeadingTitle(
            zhCandidate ? path.join(repoRoot, zhCandidate) : absPath,
            readHeadingTitle(absPath, titleFallback),
            ),
            "zh",
          ),
        });
      }
    }
  }

  const unique = new Map();
  for (const item of entries) {
    if (!unique.has(item.key)) {
      unique.set(item.key, item);
    }
  }
  return [...unique.values()].sort((a, b) => a.slug.localeCompare(b.slug));
}

function buildEntries() {
  const autoEntries = collectAutoEntries();
  const all = [...manualEntries, ...autoEntries];
  const uniqueBySlug = new Map();

  for (const item of all) {
    if (uniqueBySlug.has(item.slug)) {
      continue;
    }
    uniqueBySlug.set(item.slug, item);
  }
  return [...uniqueBySlug.values()];
}

function toSidebarItems(entries, locale) {
  return entries
    .map((entry) => ({
      text: locale === "zh" ? entry.titleZh || entry.titleEn : entry.titleEn,
      link: locale === "zh" ? `/zh/${entry.slug}` : `/${entry.slug}`,
    }))
    .sort((a, b) => a.link.localeCompare(b.link));
}

function buildOverviewSidebar(entries, locale) {
  const overviewEntries = entries.filter((entry) => entry.sectionId === "overview");
  const slugOrder = ["overview/home", "overview/architecture"];
  const items = overviewEntries
    .sort((a, b) => {
      const ai = slugOrder.indexOf(a.slug);
      const bi = slugOrder.indexOf(b.slug);
      if (ai === -1 && bi === -1) return a.slug.localeCompare(b.slug);
      if (ai === -1) return 1;
      if (bi === -1) return -1;
      return ai - bi;
    })
    .map((entry) => ({
      text: locale === "zh" ? entry.titleZh || entry.titleEn : entry.titleEn,
      link: locale === "zh" ? `/zh/${entry.slug}` : `/${entry.slug}`,
    }));
  if (items.length === 0) {
    return [];
  }
  return [{ text: locale === "zh" ? "Overview" : "Overview", items }];
}

function buildModulesSidebar(entries, locale) {
  const modules = entries.filter((entry) => entry.sectionId === "modules");
  const byPrefix = new Map();
  for (const entry of modules) {
    const prefix = entry.slug.split("/")[0];
    const arr = byPrefix.get(prefix) ?? [];
    arr.push(entry);
    byPrefix.set(prefix, arr);
  }

  const order = ["sdks", "specs", "design", "server", "components", "sandboxes", "kubernetes"];
  const blocks = [];
  for (const prefix of order) {
    const groupEntries = byPrefix.get(prefix);
    if (!groupEntries || groupEntries.length === 0) {
      continue;
    }
    blocks.push({
      text: moduleGroupLabels[locale][prefix],
      items: toSidebarItems(groupEntries, locale),
    });
  }
  return blocks;
}

function buildExamplesSidebar(entries, locale) {
  const items = toSidebarItems(entries.filter((entry) => entry.sectionId === "examples"), locale);
  if (items.length === 0) {
    return [];
  }
  return [{ text: locale === "zh" ? "示例" : "Examples", items }];
}

function buildCommunitySidebar(entries, locale) {
  const blocks = [];
  const communityEntries = entries.filter(
    (entry) => entry.sectionId === "community" && entry.slug.startsWith("community/"),
  );
  if (communityEntries.length > 0) {
    blocks.push({
      text: communityGroupLabels[locale].community,
      items: toSidebarItems(communityEntries, locale),
    });
  }

  const osepReadmeEntries = entries.filter((entry) => entry.sectionId === "community" && entry.slug === "oseps/readme");
  const osepDocEntries = entries.filter(
    (entry) => entry.sectionId === "community" && entry.slug.startsWith("oseps/") && entry.slug !== "oseps/readme",
  );
  const sortedOsepDocs = osepDocEntries.sort((a, b) => a.slug.localeCompare(b.slug));
  const osepItems = [...toSidebarItems(osepReadmeEntries, locale), ...toSidebarItems(sortedOsepDocs, locale)];
  if (osepItems.length > 0) {
    blocks.push({
      text: communityGroupLabels[locale].oseps,
      items: osepItems,
    });
  }

  return blocks;
}

function buildSidebarByPath(entries, locale) {
  const prefix = locale === "zh" ? "/zh" : "";
  const overviewSidebar = buildOverviewSidebar(entries, locale);
  const modulesSidebar = buildModulesSidebar(entries, locale);
  const examplesSidebar = buildExamplesSidebar(entries, locale);
  const communitySidebar = buildCommunitySidebar(entries, locale);

  const sidebar = {
    [`${prefix}/`]: overviewSidebar,
    [`${prefix}/overview/`]: overviewSidebar,
    [`${prefix}/examples/`]: examplesSidebar,
    [`${prefix}/community/`]: communitySidebar,
    [`${prefix}/oseps/`]: communitySidebar,
  };

  for (const modulesPrefix of ["server", "components", "sandboxes", "kubernetes", "specs", "sdks", "design"]) {
    sidebar[`${prefix}/${modulesPrefix}/`] = modulesSidebar;
  }
  return sidebar;
}

function writeGeneratedPages(entries) {
  rmIfExists(generatedRoot);
  ensureDir(path.join(generatedRoot, "en"));
  ensureDir(path.join(generatedRoot, "zh"));

  const rewrites = {};
  const pages = [];

  for (const entry of entries) {
    const enSourcePath = entry.enPath;
    const zhSourcePath = entry.zhPath || entry.enPath;
    const enGeneratedRel = `generated/en/${entry.slug}.md`;
    const zhGeneratedRel = `generated/zh/${entry.slug}.md`;
    const enGeneratedAbs = path.join(docsRoot, enGeneratedRel);
    const zhGeneratedAbs = path.join(docsRoot, zhGeneratedRel);
    ensureDir(path.dirname(enGeneratedAbs));
    ensureDir(path.dirname(zhGeneratedAbs));

    fs.writeFileSync(
      enGeneratedAbs,
      renderPageSource({
        locale: "en",
        title: entry.titleEn,
        sourceRelPath: enSourcePath,
        routeSlug: entry.slug,
        passthrough: entry.passthrough === true,
      }),
      "utf8",
    );

    fs.writeFileSync(
      zhGeneratedAbs,
      renderPageSource({
        locale: "zh",
        title: entry.titleZh || entry.titleEn,
        sourceRelPath: zhSourcePath,
        routeSlug: entry.slug,
        passthrough: entry.passthrough === true,
      }),
      "utf8",
    );

    rewrites[enGeneratedRel] = `${entry.slug}.md`;
    rewrites[zhGeneratedRel] = `zh/${entry.slug}.md`;

    pages.push({
      key: entry.key,
      slug: entry.slug,
      en: enSourcePath,
      zh: zhSourcePath,
    });
  }

  return { rewrites, pages };
}

export function buildManifest() {
  const entries = buildEntries();
  const { rewrites, pages } = writeGeneratedPages(entries);
  const manifest = {
    generatedAt: new Date().toISOString(),
    pages,
    nav: {
      en: [
        { text: "Overview", link: "/overview/home" },
        { text: "Project", link: "/sdks/sandbox/python/readme" },
        { text: "Examples", link: "/examples/readme" },
        { text: "Community", link: "/community/contributing" },
      ],
      zh: [
        { text: "Overview", link: "/zh/overview/home" },
        { text: "Project", link: "/zh/sdks/sandbox/python/readme" },
        { text: "Examples", link: "/zh/examples/readme" },
        { text: "Community", link: "/zh/community/contributing" },
      ],
    },
    sidebar: {
      en: buildSidebarByPath(entries, "en"),
      zh: buildSidebarByPath(entries, "zh"),
    },
    rewrites,
  };

  ensureDir(path.dirname(manifestPath));
  fs.writeFileSync(manifestPath, `${JSON.stringify(manifest, null, 2)}\n`, "utf8");
  return manifest;
}

export function loadManifest() {
  try {
    if (!fs.existsSync(manifestPath)) {
      return buildManifest();
    }
    const data = JSON.parse(fs.readFileSync(manifestPath, "utf8"));
    if (!data || !data.generatedAt || !data.nav || !data.sidebar || !data.rewrites) {
      return buildManifest();
    }
    return buildManifest();
  } catch (_error) {
    return buildManifest();
  }
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const manifest = buildManifest();
  // Keep logging terse for CI output.
  console.log(`docs manifest generated (${manifest.pages.length} pages)`);
}

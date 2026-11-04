<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from "vue";
import { withBase } from "vitepress";
import "./home.css";

const github = "https://github.com/opensandbox-group/OpenSandbox";
const reducedMotion = ref(false);
const selectedSdk = ref(0);
const copyState = ref("Copy");
let motionQuery: MediaQueryList | undefined;
let copyTimer: ReturnType<typeof setTimeout> | undefined;

// Deterministic positions keep the server render and hydration identical.
const particles = Array.from({ length: 24 }, (_, index) => ({
  left: `${(index * 37 + 13) % 100}%`,
  top: `${12 + ((index * 23) % 82)}%`,
  "--particle-size": `${1.5 + (index % 3) * 0.7}px`,
  "--particle-x": `${45 + (index % 5) * 22}px`,
  "--particle-y": `-${100 + (index % 4) * 35}px`,
  animationDuration: `${9 + (index % 7)}s`,
  animationDelay: `-${(index * 3) % 13}s`,
}));

const sdks = [
  { name: "Python", command: "pip install opensandbox", link: "/sdks/python" },
  {
    name: "TypeScript",
    command: "npm install @alibaba-group/opensandbox",
    link: "/sdks/javascript",
  },
  {
    name: "Java / Kotlin",
    command:
      'implementation("com.alibaba.opensandbox:sandbox:{latest_version}")',
    link: "/sdks/kotlin",
  },
  {
    name: "Go",
    command: "go get github.com/alibaba/OpenSandbox/sdks/sandbox/go",
    link: "/sdks/go",
  },
  {
    name: "C# / .NET",
    command: "dotnet add package Alibaba.OpenSandbox",
    link: "/sdks/csharp",
  },
];
const currentSdk = computed(() => sdks[selectedSdk.value]);
const preview = [
  "from opensandbox import Sandbox",
  "",
  'sandbox = await Sandbox.create("alpine")',
  "try:",
  "    result = await sandbox.commands.run(",
  "        \"echo 'Hello OpenSandbox!'\"",
  "    )",
  "finally:",
  "    await sandbox.destroy()",
];
const workloads = [
  {
    title: "Agent environment",
    description:
      "Give agents isolated environments to run tools, manage files, and execute commands.",
    tools: ["Claude Code", "Codex CLI", "DeerFlow"],
    link: "/examples/claude-code",
    icon: "m8 8-4 4 4 4m8-8 4 4-4 4m-3-11-2 14",
  },
  {
    title: "Browsers & desktops",
    description:
      "Let agents navigate the web, test interfaces, and interact with a full desktop.",
    tools: ["Playwright", "Chrome", "Desktop"],
    link: "/examples/playwright",
    icon: "M3 8h18M6 5h.01M9 5h.01M5 3h14a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2Z",
  },
  {
    title: "Code & data analysis",
    description:
      "Execute model-generated code and work with its results through the Code Interpreter SDK.",
    tools: ["Code Interpreter"],
    link: "/examples/code-interpreter",
    icon: "M4 19h16M6 15v-4m6 4V5m6 10V8",
  },
  {
    title: "Evaluation & training",
    description:
      "Run a separate sandbox for each trial, with resource pools and batch creation for parallel workloads.",
    tools: ["Harbor", "RL workloads"],
    link: "/examples/harbor-evaluation",
    icon: "M5 3v4m14-4v4M3 5h4m10 0h4M5 17v4m14-4v4M3 19h4m10 0h4M9 9h6v6H9z",
  },
];

function tokens(line: string) {
  return line
    .split(/("[^"]*"|'[^']*'|\b(?:from|import|await|try|finally)\b)/g)
    .map((text) => ({
      text,
      tone: /^["']/.test(text)
        ? "string"
        : /^(from|import|await|try|finally)$/.test(text)
          ? "keyword"
          : "plain",
    }));
}

function moveLight(event: PointerEvent) {
  if (reducedMotion.value || event.pointerType === "touch") return;
  const element = event.currentTarget as HTMLElement;
  const bounds = element.getBoundingClientRect();
  element.style.setProperty(
    "--pointer-x",
    `${((event.clientX - bounds.left) / bounds.width - 0.5) * 12}px`,
  );
  element.style.setProperty(
    "--pointer-y",
    `${((event.clientY - bounds.top) / bounds.height - 0.5) * 10}px`,
  );
}

function resetLight(event: PointerEvent) {
  const element = event.currentTarget as HTMLElement;
  element.style.setProperty("--pointer-x", "0px");
  element.style.setProperty("--pointer-y", "0px");
}

function selectSdk(index: number) {
  selectedSdk.value = index;
  copyState.value = "Copy";
  clearTimeout(copyTimer);
}

function navigateSdk(event: KeyboardEvent) {
  if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
  event.preventDefault();
  const index =
    event.key === "Home"
      ? 0
      : event.key === "End"
        ? sdks.length - 1
        : (selectedSdk.value +
            (event.key === "ArrowRight" ? 1 : -1) +
            sdks.length) %
          sdks.length;
  selectSdk(index);
  document.getElementById(`sdk-tab-${index}`)?.focus();
}

async function copyCommand() {
  const index = selectedSdk.value;
  try {
    await navigator.clipboard.writeText(currentSdk.value.command);
    if (index === selectedSdk.value) copyState.value = "Copied";
  } catch {
    if (index === selectedSdk.value) copyState.value = "Select text to copy";
  }
  clearTimeout(copyTimer);
  copyTimer = setTimeout(() => {
    copyState.value = "Copy";
  }, 2500);
}

function updateMotion() {
  reducedMotion.value = motionQuery?.matches ?? false;
}

onMounted(() => {
  motionQuery = window.matchMedia("(prefers-reduced-motion: reduce)");
  updateMotion();
  motionQuery.addEventListener("change", updateMotion);
});
onUnmounted(() => {
  motionQuery?.removeEventListener("change", updateMotion);
  clearTimeout(copyTimer);
});
</script>

<template>
  <div class="os-home">
    <section
      class="os-hero"
      aria-labelledby="hero-title"
      @pointermove="moveLight"
      @pointerleave="resetLight"
    >
      <div class="os-atmosphere" aria-hidden="true">
        <div class="os-nebula os-nebula-blue"></div>
        <div class="os-nebula os-nebula-violet"></div>
        <div class="os-aurora-ribbon"></div>
        <div class="os-starfield"></div>
        <div class="os-particles">
          <span
            v-for="(particle, index) in particles"
            :key="index"
            class="os-particle"
            :style="particle"
          ></span>
        </div>
        <div class="os-horizon"></div>
      </div>
      <div class="os-shell os-hero-grid">
        <div class="os-hero-copy">
          <a
            href="https://aaif.io/projects/opensandbox"
            target="_blank"
            rel="noreferrer"
            class="os-affiliation"
          >
            <span>An Agentic AI Foundation (AAIF) Project</span>
            <span aria-hidden="true">↗</span>
          </a>
          <h1 id="hero-title">Your agents<br />Your sandbox</h1>
          <p class="os-hero-description">
            Run AI agents on your own infrastructure. Give them isolated
            environments to execute code, use tools, and explore the web.
          </p>
          <div class="os-actions">
            <a
              class="os-button os-button-primary"
              :href="withBase('/getting-started/')"
              >Start building <span aria-hidden="true">↗</span></a
            >
            <a
              class="os-button os-button-secondary"
              :href="github"
              target="_blank"
              rel="noreferrer"
              ><svg viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
                <path
                  d="M12 .75a11.25 11.25 0 0 0-3.56 21.92c.56.1.77-.24.77-.54v-2.1c-3.13.68-3.79-1.33-3.79-1.33-.51-1.3-1.25-1.64-1.25-1.64-1.03-.7.08-.69.08-.69 1.13.08 1.73 1.16 1.73 1.16 1.01 1.72 2.64 1.22 3.29.93.1-.73.4-1.22.72-1.5-2.5-.28-5.13-1.25-5.13-5.56 0-1.23.44-2.23 1.16-3.02-.12-.29-.5-1.43.11-2.98 0 0 .95-.3 3.1 1.15a10.8 10.8 0 0 1 5.64 0c2.15-1.46 3.1-1.15 3.1-1.15.61 1.55.23 2.69.11 2.98.72.79 1.16 1.79 1.16 3.02 0 4.32-2.63 5.27-5.14 5.55.4.35.76 1.03.76 2.08v3.1c0 .3.2.65.77.54A11.25 11.25 0 0 0 12 .75Z"
                /></svg
              >View on GitHub</a
            >
          </div>
          <p class="os-hero-note">Start with Docker. Scale on Kubernetes.</p>
          <div class="os-open-source">
            <span class="os-open-source-license">
              <span class="os-mini-cube" aria-hidden="true">◇</span> Open source
              <span class="os-dot-separator">·</span> Apache 2.0
            </span>
          </div>
        </div>
        <div class="os-stage">
          <div class="os-stage-aura" aria-hidden="true"></div>
          <svg
            class="os-sandbox-art"
            viewBox="0 0 640 570"
            fill="none"
            role="img"
            aria-labelledby="sandbox-art-title"
          >
            <title id="sandbox-art-title">
              An isolated sandbox connects your agent to code, browsers, and
              tools.
            </title>
            <defs>
              <linearGradient
                id="os-edge"
                x1="150"
                y1="90"
                x2="510"
                y2="430"
                gradientUnits="userSpaceOnUse"
              >
                <stop stop-color="#e1d4ff" />
                <stop offset=".5" stop-color="#93e2ff" />
                <stop offset="1" stop-color="#548bff" />
              </linearGradient>
              <linearGradient
                id="os-top"
                x1="340"
                y1="90"
                x2="340"
                y2="300"
                gradientUnits="userSpaceOnUse"
              >
                <stop stop-color="#8d7eff" stop-opacity=".24" />
                <stop offset="1" stop-color="#5d8bff" stop-opacity=".06" />
              </linearGradient>
              <linearGradient
                id="os-left"
                x1="160"
                y1="180"
                x2="340"
                y2="470"
                gradientUnits="userSpaceOnUse"
              >
                <stop stop-color="#739dff" stop-opacity=".27" />
                <stop offset="1" stop-color="#152765" stop-opacity=".1" />
              </linearGradient>
              <linearGradient
                id="os-right"
                x1="520"
                y1="180"
                x2="340"
                y2="470"
                gradientUnits="userSpaceOnUse"
              >
                <stop stop-color="#448dff" stop-opacity=".24" />
                <stop offset="1" stop-color="#4d5ab5" stop-opacity=".06" />
              </linearGradient>
              <radialGradient id="os-core">
                <stop stop-color="#c8f9ff" />
                <stop offset=".35" stop-color="#63caff" stop-opacity=".5" />
                <stop offset="1" stop-color="#548bff" stop-opacity="0" />
              </radialGradient>
              <linearGradient id="os-chip" x1="0" y1="0" x2="1" y2="1">
                <stop stop-color="#e0fcff" />
                <stop offset=".5" stop-color="#87dfff" />
                <stop offset="1" stop-color="#798bff" />
              </linearGradient>
              <filter id="os-glow" x="-80%" y="-80%" width="260%" height="260%">
                <feGaussianBlur stdDeviation="5" />
              </filter>
            </defs>
            <g class="os-circuit-lines" stroke="#91b4ef" stroke-opacity=".5">
              <path
                d="M12 216h78l70 40M520 256l62-36h54M340 82V28M520 360l62 36h54"
              />
              <circle cx="12" cy="216" r="3" />
              <circle cx="636" cy="220" r="3" />
              <circle cx="636" cy="396" r="3" />
              <path d="m80 418 260 150 260-150-260-150Z" opacity=".35" />
            </g>
            <g
              class="os-data-stream"
              stroke="#d3f6ff"
              stroke-width="2.5"
              stroke-linecap="round"
              stroke-dasharray="22 320"
            >
              <path d="M12 216h78l70 40" />
              <path d="M636 220h-54l-62 36" />
              <path d="M340 0v82" />
            </g>
            <g class="os-floating-cube">
              <path
                d="m340 80 180 104-180 104-180-104Z"
                fill="url(#os-top)"
                stroke="url(#os-edge)"
                stroke-width="1.6"
              />
              <path
                d="m160 184 180 104v190L160 374Z"
                fill="url(#os-left)"
                stroke="url(#os-edge)"
                stroke-opacity=".85"
              />
              <path
                d="m520 184-180 104v190l180-104Z"
                fill="url(#os-right)"
                stroke="url(#os-edge)"
                stroke-opacity=".85"
              />
              <path
                d="M340 80v190M160 374l180-104 180 104"
                stroke="#8ea7ff"
                stroke-opacity=".3"
                stroke-dasharray="4 6"
              />
              <path
                d="m340 164 122 70-122 71-122-71Z"
                stroke="#a7caff"
                stroke-opacity=".6"
              />
              <path
                d="m340 245 122 70-122 71-122-71Z"
                stroke="#a7caff"
                stroke-opacity=".35"
              />
              <ellipse
                class="os-core-aura"
                cx="340"
                cy="260"
                rx="124"
                ry="88"
                fill="url(#os-core)"
              />
              <path
                d="m340 184 88 51-88 51-88-51Z"
                fill="#080e22"
                stroke="#a9e6ff"
                stroke-width="1.5"
              />
              <path
                d="m252 235 88 51 88-51v21l-88 51-88-51Z"
                fill="#426de0"
                fill-opacity=".65"
                stroke="#96ccff"
                stroke-opacity=".8"
              />
              <path
                d="m340 208 46 27-46 27-46-27Z"
                fill="url(#os-chip)"
                fill-opacity=".75"
                stroke="#e3fbff"
                stroke-width="1.5"
              />
              <path
                d="m340 208 46 27-46 27-46-27Z"
                stroke="#9feaff"
                stroke-width="5"
                filter="url(#os-glow)"
                opacity=".75"
              />
              <path
                d="m340 80 180 104-180 104-180-104Z"
                stroke="#86b8ff"
                stroke-width="4"
                filter="url(#os-glow)"
                opacity=".7"
              />
              <path
                class="os-energy-trace"
                d="m340 80 180 104-180 104-180-104Z"
                stroke="#e1faff"
                stroke-width="2.5"
                stroke-linecap="round"
                stroke-dasharray="72 760"
              />
              <circle cx="340" cy="80" r="3.5" fill="#f2edff" />
              <circle cx="160" cy="184" r="3.5" fill="#c5f3ff" />
              <circle cx="520" cy="184" r="3.5" fill="#c5f3ff" />
              <text
                x="340"
                y="143"
                text-anchor="middle"
                fill="#d5e5ff"
                font-size="11"
                font-family="monospace"
                letter-spacing="2"
              >
                sandbox / isolated
              </text>
            </g>
          </svg>
          <div class="os-art-label os-art-label-agent">
            <span class="os-status-dot"></span> AI agent
          </div>
          <div class="os-art-label os-art-label-tools">
            Code · Browser · Tools
          </div>
          <div class="os-execution-window">
            <div class="os-window-bar">
              <div class="os-window-dots" aria-hidden="true">
                <i></i><i></i><i></i>
              </div>
              <span>sandbox.py</span
              ><span class="os-window-caption">Execution preview</span>
            </div>
            <pre
              class="os-code"
            ><code><span v-for="(line, index) in preview" :key="index" class="os-code-line"><span class="os-line-number" aria-hidden="true">{{ index + 1 }}</span><span><span v-for="(token, n) in tokens(line)" :key="n" :class="`os-token-${token.tone}`">{{ token.text }}</span></span></span></code></pre>
            <div class="os-code-output">
              <span aria-hidden="true">›</span> Hello OpenSandbox!
              <span class="os-output-cursor" aria-hidden="true"></span>
            </div>
          </div>
        </div>
      </div>
    </section>
    <div class="os-ecosystem">
      <div class="os-shell os-ecosystem-inner">
        <span>Fits your stack.</span>
        <div>
          <a v-for="sdk in sdks" :key="sdk.name" :href="withBase(sdk.link)">{{
            sdk.name
          }}</a
          ><span class="os-stack-divider"></span
          ><a :href="withBase('/cli/')">CLI</a
          ><a :href="withBase('/sdks/mcp')">MCP</a>
        </div>
      </div>
    </div>
    <section class="os-shell os-section" aria-labelledby="workloads-title">
      <div class="os-section-heading">
        <div>
          <p class="os-section-kicker">A workspace for every workload</p>
          <h2 id="workloads-title">Let your agents get to work.</h2>
        </div>
        <a class="os-text-link" :href="withBase('/examples/')"
          >Explore examples <span aria-hidden="true">↗</span></a
        >
      </div>
      <div class="os-workload-grid">
        <a
          v-for="workload in workloads"
          :key="workload.title"
          class="os-workload"
          :href="withBase(workload.link)"
          ><div class="os-workload-top">
            <svg
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              stroke-width="1.4"
              stroke-linecap="round"
              stroke-linejoin="round"
              aria-hidden="true"
            >
              <path :d="workload.icon" /></svg
            ><span aria-hidden="true">↗</span>
          </div>
          <h3>{{ workload.title }}</h3>
          <p>{{ workload.description }}</p>
          <div class="os-tool-list">
            <span v-for="tool in workload.tools" :key="tool">{{ tool }}</span>
          </div></a
        >
      </div>
    </section>
    <section class="os-shell os-fast-section" aria-labelledby="fast-title">
      <div class="os-fast-copy">
        <a
          class="os-section-kicker"
          :href="withBase('/architecture/fast-sandbox/')"
          >Fast Sandbox runtime <span aria-hidden="true">↗</span></a
        >
        <h2 id="fast-title">Less waiting.<br />More running.</h2>
        <p>
          Fast, high-density sandboxes on Kubernetes. Restore Firecracker
          microVMs from pre-warmed capacity, and pause with memory and disk
          state preserved.
        </p>
        <a class="os-text-link" :href="withBase('/architecture/fast-sandbox/')"
          >Explore Fast Sandbox <span aria-hidden="true">↗</span></a
        >
      </div>
      <div class="os-benchmark">
        <div class="os-benchmark-heading">
          <span class="os-status-dot"></span> Firecracker warm creation
        </div>
        <div class="os-benchmark-stats">
          <div>
            <strong>64<span>ms</span></strong>
            <p>P50 · Serial creation</p>
          </div>
          <div>
            <strong>125.4<span>ms</span></strong>
            <p>P99 · 10 concurrent</p>
          </div>
        </div>
        <div class="os-runtime-flow">
          <span>Cached template</span><i aria-hidden="true"></i
          ><span>Restore microVM</span><i aria-hidden="true"></i
          ><span class="os-flow-ready">Ready</span>
        </div>
        <a
          :href="withBase('/architecture/fast-sandbox/performance')"
          class="os-text-link"
          >Read the benchmark & conditions <span aria-hidden="true">↗</span></a
        >
      </div>
    </section>
    <section
      class="os-shell os-section os-control-section"
      aria-labelledby="control-title"
    >
      <div class="os-section-heading">
        <div>
          <p class="os-section-kicker">Your infrastructure. Your boundaries.</p>
          <h2 id="control-title">Give agents access.<br />Keep control.</h2>
        </div>
        <p class="os-heading-description">
          One sandbox API, from your laptop to your cluster. Define what agents
          can reach and how they use credentials.
        </p>
      </div>
      <div class="os-control-grid">
        <a :href="withBase('/guides/network-isolation')" class="os-control-item"
          ><svg
            viewBox="0 0 40 40"
            fill="none"
            stroke="currentColor"
            stroke-width="1.3"
            aria-hidden="true"
          >
            <rect x="13" y="13" width="14" height="14" rx="3" />
            <path
              d="M20 4v9m0 14v9M4 20h9m14 0h9M8 8l6 6m12 12 6 6M8 32l6-6m12-12 6-6"
            />
            <circle cx="20" cy="20" r="3" />
          </svg>
          <h3>Network access control <span aria-hidden="true">↗</span></h3>
          <p>
            Route inbound traffic through a unified ingress gateway. Set
            outbound access policies for each sandbox.
          </p></a
        >
        <a :href="withBase('/guides/credential-vault')" class="os-control-item"
          ><svg
            viewBox="0 0 40 40"
            fill="none"
            stroke="currentColor"
            stroke-width="1.3"
            aria-hidden="true"
          >
            <path d="m20 4 13 5v10c0 8-13 16-13 16S7 27 7 19V9Z" />
            <circle cx="20" cy="18" r="4" />
            <path d="M20 22v5" />
          </svg>
          <h3>Credential Vault <span aria-hidden="true">↗</span></h3>
          <p>
            Let agents call external services while keeping real credentials out
            of sandbox workloads.
          </p></a
        >
        <a :href="withBase('/architecture/')" class="os-control-item"
          ><svg
            viewBox="0 0 40 40"
            fill="none"
            stroke="currentColor"
            stroke-width="1.3"
            aria-hidden="true"
          >
            <path
              d="m20 4 14 8-14 8-14-8Zm-14 8v16l14 8 14-8V12M20 20v16M6 20l14 8 14-8"
            />
          </svg>
          <h3>Local to cluster <span aria-hidden="true">↗</span></h3>
          <p>
            Start locally with Docker. Deploy on Kubernetes through a unified,
            extensible lifecycle and execution API.
          </p></a
        >
      </div>
    </section>
    <section
      id="get-started"
      class="os-shell os-start-section"
      aria-labelledby="start-title"
    >
      <div class="os-start-copy">
        <p class="os-section-kicker">
          From your first sandbox to your next idea
        </p>
        <h2 id="start-title">Make room<br />for your next agent.</h2>
        <p>
          Start the server, pick your SDK, and run your first command. Your
          infrastructure takes it from there.
        </p>
        <a
          class="os-button os-button-primary"
          :href="withBase('/getting-started/')"
          >Follow the quick start <span aria-hidden="true">↗</span></a
        >
        <div class="os-start-links">
          <a :href="withBase('/cli/')">Use the CLI</a
          ><a :href="withBase('/sdks/mcp')">Connect with MCP</a>
        </div>
      </div>
      <div class="os-setup-window">
        <div class="os-setup-step">
          <span class="os-step-number">1</span>
          <h3>Start a local server</h3>
          <span>Docker + Python 3.10+</span>
        </div>
        <pre
          class="os-server-code"
        ><code><span class="os-comment"># Initialize your Docker configuration</span>
uvx opensandbox-server init-config \
  ~/.sandbox.toml --example docker

<span class="os-comment"># Start the server</span>
uvx opensandbox-server</code></pre>
        <div class="os-setup-step">
          <span class="os-step-number">2</span>
          <h3>Install your SDK</h3>
        </div>
        <div
          class="os-sdk-tabs"
          role="tablist"
          aria-label="SDK language"
          @keydown="navigateSdk"
        >
          <button
            v-for="(sdk, index) in sdks"
            :id="`sdk-tab-${index}`"
            :key="sdk.name"
            type="button"
            role="tab"
            :aria-selected="selectedSdk === index"
            aria-controls="sdk-install-panel"
            :tabindex="selectedSdk === index ? 0 : -1"
            @click="selectSdk(index)"
          >
            {{ sdk.name }}
          </button>
        </div>
        <div
          id="sdk-install-panel"
          class="os-install-panel"
          role="tabpanel"
          :aria-labelledby="`sdk-tab-${selectedSdk}`"
          tabindex="0"
        >
          <div class="os-install-command">
            <code>{{ currentSdk.command }}</code
            ><button
              type="button"
              :aria-label="`${copyState} install command`"
              @click="copyCommand"
            >
              {{ copyState }}
            </button>
          </div>
          <p v-if="selectedSdk === 2" class="os-install-hint">
            Add to build.gradle.kts and replace {latest_version} with a
            published version.
          </p>
          <a :href="withBase(currentSdk.link)"
            >Read the {{ currentSdk.name }} guide
            <span aria-hidden="true">↗</span></a
          ><span class="os-sr-only" role="status">{{
            copyState === "Copied" ? "Install command copied to clipboard." : ""
          }}</span>
        </div>
      </div>
    </section>
    <div class="os-shell os-community">
      <a :href="github" target="_blank" rel="noreferrer"
        >Built in the open. Build with us. <span aria-hidden="true">↗</span></a
      >
      <div>
        <a :href="withBase('/community/contributing')">Contribute</a
        ><a
          href="https://discord.gg/g7FuPs8YeD"
          target="_blank"
          rel="noreferrer"
          >Join Discord</a
        ><a :href="withBase('/api/')">API reference</a>
      </div>
    </div>
  </div>
</template>

/// <reference types="vite/client" />

interface ImportMetaEnv {
  readonly VITE_API_PREFIX: string;
  readonly VITE_SANDBOX_HELP_RUNTIME_PAUSE: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}

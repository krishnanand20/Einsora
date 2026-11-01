/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** Tenant sent as X-Dev-Tenant when the API runs with EINSORA_AUTH_MODE=dev. */
  readonly VITE_DEV_TENANT?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}

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

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useSyncExternalStore,
  type ReactNode,
} from 'react';

import enUS from 'antd/locale/en_US';
import zhCNLocale from 'antd/locale/zh_CN';

import { en, type Messages, type Translate, type TranslateParams } from './en';
import { zhCN } from './zh-CN';

export type { MessageKey, Translate, TranslateParams } from './en';

export type Locale = 'en' | 'zh-CN';

/** localStorage key holding the user's explicit locale selection. */
export const LOCALE_STORAGE_KEY = 'opensandbox.console.locale';

export const DEFAULT_LOCALE: Locale = 'en';

export type LocaleOption = {
  value: Locale;
  label: string;
};

/** Locale metadata used by the language selector. */
export const SUPPORTED_LOCALES: readonly LocaleOption[] = [
  { value: 'en', label: 'English' },
  { value: 'zh-CN', label: '简体中文' },
];

const messagesByLocale: Record<Locale, Messages> = {
  en,
  'zh-CN': zhCN,
};

/**
 * Ant Design locale objects live next to the message dictionaries, so adding a
 * language is one edit in this module instead of one here and one in `App`.
 */
export const antdLocales: Record<Locale, typeof enUS> = {
  en: enUS,
  'zh-CN': zhCNLocale,
};

export function isLocale(value: unknown): value is Locale {
  return value === 'en' || value === 'zh-CN';
}

function readStoredLocale(): Locale | null {
  if (typeof window === 'undefined') return null;
  try {
    const stored = window.localStorage.getItem(LOCALE_STORAGE_KEY);
    return isLocale(stored) ? stored : null;
  } catch {
    // localStorage can be unavailable (disabled storage, privacy mode).
    return null;
  }
}

function detectBrowserLocale(): Locale {
  if (typeof navigator === 'undefined') return DEFAULT_LOCALE;
  const language = navigator.language;
  return typeof language === 'string' && language.toLowerCase().startsWith('zh')
    ? 'zh-CN'
    : DEFAULT_LOCALE;
}

/**
 * Explicit selection wins; otherwise follow a Chinese browser locale and fall
 * back to English.
 */
export function resolveInitialLocale(): Locale {
  return readStoredLocale() ?? detectBrowserLocale();
}

type LocaleListener = () => void;

let activeLocale: Locale | null = null;
const localeListeners = new Set<LocaleListener>();

/**
 * The committed locale lives outside React state so the translator can keep one
 * identity while still translating with the locale React has rendered. It is
 * read through `useSyncExternalStore`, so no ref is written during render: an
 * interrupted or abandoned render cannot leave translators on an uncommitted
 * locale.
 */
function getLocaleSnapshot(): Locale {
  if (activeLocale === null) activeLocale = resolveInitialLocale();
  return activeLocale;
}

function subscribeLocale(listener: LocaleListener): () => void {
  localeListeners.add(listener);
  return () => {
    localeListeners.delete(listener);
  };
}

function persistLocale(locale: Locale): void {
  if (typeof window === 'undefined') return;
  try {
    window.localStorage.setItem(LOCALE_STORAGE_KEY, locale);
  } catch {
    // Persisting the preference is best-effort.
  }
}

function setActiveLocale(nextLocale: Locale): void {
  if (getLocaleSnapshot() === nextLocale) return;
  activeLocale = nextLocale;
  persistLocale(nextLocale);
  localeListeners.forEach((listener) => listener());
}

function interpolate(template: string, params?: TranslateParams): string {
  if (!params) return template;
  return template.replace(/\{(\w+)\}/g, (placeholder, name: string) => {
    const value = params[name];
    return value === undefined ? placeholder : String(value);
  });
}

interface I18nContextValue {
  locale: Locale;
  setLocale: (locale: Locale) => void;
  t: Translate;
}

const I18nContext = createContext<I18nContextValue | null>(null);

export function I18nProvider({ children }: { children: ReactNode }) {
  const locale = useSyncExternalStore(
    subscribeLocale,
    getLocaleSnapshot,
    getLocaleSnapshot,
  );

  const setLocale = useCallback((nextLocale: Locale) => {
    setActiveLocale(nextLocale);
  }, []);

  useEffect(() => {
    if (typeof document !== 'undefined') {
      document.documentElement.lang = locale;
    }
  }, [locale]);

  // Stable t prevents locale changes from refetching data; memoized translations must depend on locale, not t.
  const t = useCallback<Translate>(
    (key, params) => interpolate(messagesByLocale[getLocaleSnapshot()][key], params),
    [],
  );

  const value = useMemo<I18nContextValue>(
    () => ({ locale, setLocale, t }),
    [locale, setLocale, t],
  );

  return <I18nContext.Provider value={value}>{children}</I18nContext.Provider>;
}

export function useI18n(): I18nContextValue {
  const context = useContext(I18nContext);
  if (!context) throw new Error('useI18n must be used within I18nProvider');
  return context;
}

/** Translate a Lifecycle sandbox state for display; unknown values pass through. */
export function statusLabel(t: Translate, state?: string | null): string {
  switch (state?.trim().toLowerCase()) {
    case 'pending':
      return t('sandbox.status.pending');
    case 'running':
      return t('sandbox.status.running');
    case 'pausing':
      return t('sandbox.status.pausing');
    case 'paused':
      return t('sandbox.status.paused');
    case 'resuming':
      return t('sandbox.status.resuming');
    case 'stopping':
      return t('sandbox.status.stopping');
    case 'terminated':
      return t('sandbox.status.terminated');
    case 'failed':
      return t('sandbox.status.failed');
    default:
      return state ?? '';
  }
}

/** Translate a diagnostic scope for display; the value sent to the API is unchanged. */
export function diagnosticScopeLabel(t: Translate, scope: string): string {
  switch (scope) {
    case 'container':
      return t('scope.container');
    case 'lifecycle':
      return t('scope.lifecycle');
    case 'runtime':
      return t('scope.runtime');
    case 'network':
      return t('scope.network');
    case 'process':
      return t('scope.process');
    case 'all':
      return t('scope.all');
    default:
      return scope;
  }
}

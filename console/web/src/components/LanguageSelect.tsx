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

import { Select } from 'antd';

import { SUPPORTED_LOCALES, useI18n } from '../i18n';

/** Compact locale switcher shared by the login page and the app header. */
export function LanguageSelect() {
  const { locale, setLocale, t } = useI18n();

  return (
    <Select
      size="small"
      aria-label={t('common.language')}
      value={locale}
      onChange={setLocale}
      options={SUPPORTED_LOCALES.map(({ value, label }) => ({ value, label }))}
      style={{ width: 112 }}
      popupMatchSelectWidth={false}
    />
  );
}

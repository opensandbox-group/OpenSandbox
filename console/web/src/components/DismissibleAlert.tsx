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

import { Alert, type AlertProps } from 'antd';
import { useCallback, useState } from 'react';

const STORAGE_PREFIX = 'opensandbox-console.dismiss-alert.';

type Props = AlertProps & {
  /** Stable id; dismissal is remembered in localStorage for this browser. */
  dismissKey: string;
};

export function DismissibleAlert({ dismissKey, onClose, ...rest }: Props) {
  const storageKey = `${STORAGE_PREFIX}${dismissKey}`;
  const [hidden, setHidden] = useState(() => {
    try {
      return localStorage.getItem(storageKey) === '1';
    } catch {
      return false;
    }
  });

  const handleClose: NonNullable<AlertProps['onClose']> = useCallback(
    (e) => {
      try {
        localStorage.setItem(storageKey, '1');
      } catch {
        /* ignore quota / private mode */
      }
      setHidden(true);
      onClose?.(e);
    },
    [onClose, storageKey],
  );

  if (hidden) return null;

  return <Alert closable {...rest} onClose={handleClose} />;
}

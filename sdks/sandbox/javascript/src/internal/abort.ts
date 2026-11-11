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

export type AbortSubscription = () => void;

interface AbortSubscriptionGroup {
  readonly listeners: Set<() => void>;
  readonly dispatch: () => void;
}

const abortSubscriptionGroups = new WeakMap<AbortSignal, AbortSubscriptionGroup>();

/**
 * Subscribes to an AbortSignal while keeping only one native listener per signal.
 *
 * Pool runs can fan out cancellation to thousands of warmup operations. Registering
 * every operation directly on the shared signal triggers EventTarget listener limits
 * and retains completed operations until shutdown. The returned function must be
 * called when the logical subscription is no longer needed.
 */
export function subscribeAbort(
  signal: AbortSignal | undefined,
  listener: () => void,
): AbortSubscription {
  if (!signal) return () => undefined;
  if (signal.aborted) {
    listener();
    return () => undefined;
  }

  let group = abortSubscriptionGroups.get(signal);
  if (!group) {
    const listeners = new Set<() => void>();
    const dispatch = () => {
      abortSubscriptionGroups.delete(signal);
      for (const current of [...listeners]) current();
      listeners.clear();
    };
    group = { listeners, dispatch };
    abortSubscriptionGroups.set(signal, group);
    signal.addEventListener("abort", dispatch, { once: true });
  }

  group.listeners.add(listener);
  return () => {
    const current = abortSubscriptionGroups.get(signal);
    if (!current) return;
    current.listeners.delete(listener);
    if (current.listeners.size === 0) {
      signal.removeEventListener("abort", current.dispatch);
      abortSubscriptionGroups.delete(signal);
    }
  };
}

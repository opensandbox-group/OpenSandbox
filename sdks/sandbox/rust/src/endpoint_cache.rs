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

//! Small TTL/LRU cache for resolved sandbox endpoints.

use std::collections::HashMap;
use std::sync::Mutex;
use std::time::{Duration, Instant};

use crate::models::Endpoint;

type Key = (String, u16, bool);

struct Entry {
    endpoint: Endpoint,
    expires_at: Instant,
    inserted_at: Instant,
}

/// Caches endpoint lookups so callers that issue many execd requests do not
/// pay a lifecycle round-trip per call.
pub(crate) struct EndpointCache {
    entries: Mutex<HashMap<Key, Entry>>,
    ttl: Duration,
    capacity: usize,
}

impl EndpointCache {
    pub(crate) fn new(ttl: Duration, capacity: usize) -> Self {
        Self {
            entries: Mutex::new(HashMap::new()),
            ttl,
            capacity: capacity.max(1),
        }
    }

    pub(crate) fn get(
        &self,
        sandbox_id: &str,
        port: u16,
        use_server_proxy: bool,
    ) -> Option<Endpoint> {
        let key = (sandbox_id.to_string(), port, use_server_proxy);
        let mut entries = self.entries.lock().unwrap_or_else(|e| e.into_inner());
        let entry = entries.get(&key)?;
        if Instant::now() >= entry.expires_at {
            entries.remove(&key);
            return None;
        }
        Some(entry.endpoint.clone())
    }

    pub(crate) fn put(
        &self,
        sandbox_id: &str,
        port: u16,
        use_server_proxy: bool,
        endpoint: Endpoint,
    ) {
        let key = (sandbox_id.to_string(), port, use_server_proxy);
        let now = Instant::now();
        let mut entries = self.entries.lock().unwrap_or_else(|e| e.into_inner());

        if entries.len() >= self.capacity && !entries.contains_key(&key) {
            // Evict expired entries first, then the oldest insertion.
            entries.retain(|_, entry| now < entry.expires_at);
            if entries.len() >= self.capacity {
                if let Some(oldest) = entries
                    .iter()
                    .min_by_key(|(_, entry)| entry.inserted_at)
                    .map(|(key, _)| key.clone())
                {
                    entries.remove(&oldest);
                }
            }
        }

        entries.insert(
            key,
            Entry {
                endpoint,
                expires_at: now + self.ttl,
                inserted_at: now,
            },
        );
    }

    pub(crate) fn invalidate_sandbox(&self, sandbox_id: &str) {
        let mut entries = self.entries.lock().unwrap_or_else(|e| e.into_inner());
        entries.retain(|(id, _, _), _| id != sandbox_id);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::BTreeMap;

    fn endpoint(host: &str) -> Endpoint {
        Endpoint {
            endpoint: host.to_string(),
            headers: BTreeMap::new(),
            origin: None,
        }
    }

    #[test]
    fn stores_and_evicts() {
        let cache = EndpointCache::new(Duration::from_secs(60), 2);
        cache.put("a", 44772, false, endpoint("a:1"));
        cache.put("b", 44772, false, endpoint("b:1"));
        assert!(cache.get("a", 44772, false).is_some());
        cache.put("c", 44772, false, endpoint("c:1"));
        assert_eq!(cache.entries.lock().unwrap().len(), 2);
        assert!(cache.get("c", 44772, false).is_some());
    }

    #[test]
    fn expires_and_invalidates() {
        let cache = EndpointCache::new(Duration::from_millis(1), 4);
        cache.put("a", 44772, true, endpoint("a:1"));
        std::thread::sleep(Duration::from_millis(5));
        assert!(cache.get("a", 44772, true).is_none());

        cache.put("a", 44772, true, endpoint("a:2"));
        cache.invalidate_sandbox("a");
        assert!(cache.get("a", 44772, true).is_none());
    }

    #[test]
    fn proxy_mode_is_part_of_the_key() {
        let cache = EndpointCache::new(Duration::from_secs(60), 4);
        cache.put("a", 44772, false, endpoint("direct"));
        assert!(cache.get("a", 44772, true).is_none());
        assert_eq!(cache.get("a", 44772, false).unwrap().endpoint, "direct");
    }
}

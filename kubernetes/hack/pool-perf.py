# Copyright 2025 The OpenSandbox Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http:#www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import asyncio
import time
import uuid
import sys
import argparse
from kubernetes import client, config
from kubernetes.client.rest import ApiException

# CRD configurations
GROUP = "sandbox.opensandbox.io"
VERSION = "v1alpha1"
POOL_PLURAL = "pools"
BSB_PLURAL = "batchsandboxes"
NAMESPACE = "default"

class PoolPerformanceTester:
    def __init__(self, pool_name, pool_size, replicas_per_bsb, total_bsb_count, timeout, poll_interval=0.00001):
        try:
            config.load_kube_config()
        except Exception:
            # Fall back to in-cluster config if kube config is not available
            config.load_incluster_config()
        self.custom_api = client.CustomObjectsApi()
        self.pool_name = pool_name
        self.pool_size = pool_size
        self.replicas_per_bsb = replicas_per_bsb
        self.total_bsb_count = total_bsb_count
        self.timeout = timeout
        self.poll_interval = poll_interval
        self.bsb_names = []
        self.results = {}

    def create_pool_manifest(self, size):
        return {
            "apiVersion": f"{GROUP}/{VERSION}",
            "kind": "Pool",
            "metadata": {"name": self.pool_name},
            "spec": {
                "template": {
                    "spec": {
                        "containers": [{"name": "nginx", "image": "nginx:alpine"}]
                    }
                },
                "capacitySpec": {
                    "bufferMin": 5,
                    "bufferMax": 10,
                    "poolMin": size,
                    "poolMax": size + 20
                }
            }
        }

    def create_bsb_manifest(self, name):
        return {
            "apiVersion": f"{GROUP}/{VERSION}",
            "kind": "BatchSandbox",
            "metadata": {"name": name},
            "spec": {
                "replicas": self.replicas_per_bsb,
                "poolRef": self.pool_name
            }
        }

    async def setup_pool(self):
        """Create and wait for the resource pool to be ready"""
        print(f"🚀 Setting up Pool: {self.pool_name} with size {self.pool_size}...")
        try:
            self.custom_api.delete_namespaced_custom_object(GROUP, VERSION, NAMESPACE, POOL_PLURAL, self.pool_name)
            await asyncio.sleep(5)
        except ApiException as e:
            if e.status != 404:
                print(f"⚠️  Failed to delete existing Pool: {e}")
        except Exception as e:
            print(f"⚠️  Error during Pool deletion: {e}")

        body = self.create_pool_manifest(self.pool_size)
        self.custom_api.create_namespaced_custom_object(GROUP, VERSION, NAMESPACE, POOL_PLURAL, body)
        
        # Wait for Available count to reach target
        while True:
            try:
                pool = self.custom_api.get_namespaced_custom_object(GROUP, VERSION, NAMESPACE, POOL_PLURAL, self.pool_name)
                available = pool.get("status", {}).get("available", 0)
                if available >= self.pool_size:
                    print(f"✅ Pool is Ready. Available: {available}")
                    break
                print(f"Waiting for Pool Ready... Available: {available}")
            except Exception as e:
                print(f"Waiting for Pool to be created... {e}")
            await asyncio.sleep(2)

    async def create_bsb(self, index):
        """Create BatchSandboxes concurrently"""
        name = f"perf-test-{uuid.uuid4().hex[:8]}"
        self.bsb_names.append(name)
        body = self.create_bsb_manifest(name)
        
        start_time = time.time()
        try:
            self.custom_api.create_namespaced_custom_object(GROUP, VERSION, NAMESPACE, BSB_PLURAL, body)
            self.results[name] = {"create_time": time.time() - start_time, "allocated_time": None}
        except ApiException as e:
            print(f"❌ Failed to create {name}: {e}")

    async def wait_for_allocation(self, name):
        """Poll for allocation completion"""
        start_polling = time.time()
        while True:
            try:
                bsb = self.custom_api.get_namespaced_custom_object(GROUP, VERSION, NAMESPACE, BSB_PLURAL, name)
                status = bsb.get("status", {})
                allocated = status.get("allocated", 0)
                
                if allocated >= self.replicas_per_bsb:
                    print("{0}, endpoint {1}".format(name, bsb.get("metadata", {}).get("annotations", {}).get("sandbox.opensandbox.io/endpoints", "")))
                    self.results[name]["allocated_time"] = time.time() - start_polling
                    break
            except Exception as e:
                pass
            
            await asyncio.sleep(self.poll_interval)
            if time.time() - start_polling > self.timeout:
                print(f"⏰ Timeout waiting for {name}")
                break

    async def run(self):
        await self.setup_pool()
        
        print(f"🔥 Starting concurrent allocation test: {self.total_bsb_count} BatchSandboxes...")
        start_all = time.time()
        
        # Concurrent creation
        await asyncio.gather(*(self.create_bsb(i) for i in range(self.total_bsb_count)))
        
        # Concurrent wait for allocation
        await asyncio.gather(*(self.wait_for_allocation(name) for name in self.bsb_names))
        
        total_duration = time.time() - start_all
        self.print_report(total_duration)

    def print_report(self, total_duration):
        print("\n" + "="*40)
        print("📊 PERFORMANCE REPORT")
        print("="*40)
        durations = [r["allocated_time"] for r in self.results.values() if r.get("allocated_time") is not None]
        
        if durations:
            avg_lat = sum(durations) / len(durations)
            max_lat = max(durations)
            p95 = sorted(durations)[int(len(durations) * 0.95)]
            
            print(f"Total BSB:      {self.total_bsb_count}")
            print(f"Total Duration: {total_duration:.2f}s")
            print(f"Throughput:     {len(durations)/total_duration:.2f} sandbox/s")
            print(f"Avg Latency:    {avg_lat:.2f}s")
            print(f"Max Latency:    {max_lat:.2f}s")
            print(f"P95 Latency:    {p95:.2f}s")
            print(f"Success Rate:   {len(durations)/self.total_bsb_count*100:.1f}%")
        else:
            print("No successful allocations recorded.")
        print("="*40)

    def cleanup(self):
        print("🧹 Cleaning up...")
        for name in self.bsb_names:
            try:
                self.custom_api.delete_namespaced_custom_object(GROUP, VERSION, NAMESPACE, BSB_PLURAL, name)
            except Exception as e:
                # Silently ignore deletion errors during cleanup
                pass
        try:
            self.custom_api.delete_namespaced_custom_object(GROUP, VERSION, NAMESPACE, POOL_PLURAL, self.pool_name)
        except Exception as e:
            # Silently ignore deletion errors during cleanup
            pass

if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="Pool Performance Tester")
    parser.add_argument("--pool-name", type=str, default="perf-pool", help="Pool name (default: perf-pool)")
    parser.add_argument("--pool-size", type=int, default=50, help="Pool size (default: 50)")
    parser.add_argument("--replicas", type=int, default=1, help="Replicas per BatchSandbox (default: 1)")
    parser.add_argument("--bsb-count", type=int, default=50, help="Number of BatchSandboxes to create concurrently (default: 50)")
    parser.add_argument("--namespace", type=str, default="default", help="Kubernetes namespace (default: default)")
    parser.add_argument("--timeout", type=int, default=120, help="Timeout in seconds for each BatchSandbox allocation (default: 120)")
    parser.add_argument("--poll-interval", type=float, default=0.00001, help="Poll interval in seconds for checking BatchSandbox status (default: 0.00001)")
    
    args = parser.parse_args()
    
    # Update global namespace
    NAMESPACE = args.namespace
    
    print(f"🔧 Test Configuration:")
    print(f"   Pool Name:    {args.pool_name}")
    print(f"   Pool Size:    {args.pool_size}")
    print(f"   Replicas:     {args.replicas}")
    print(f"   BSB Count:    {args.bsb_count}")
    print(f"   Namespace:    {args.namespace}")
    print(f"   Timeout:      {args.timeout}s")
    print(f"   Poll Interval: {args.poll_interval}s")
    print()
    
    tester = PoolPerformanceTester(
        pool_name=args.pool_name,
        pool_size=args.pool_size,
        replicas_per_bsb=args.replicas,
        total_bsb_count=args.bsb_count,
        timeout=args.timeout,
        poll_interval=args.poll_interval
    )
    try:
        asyncio.run(tester.run())
    except KeyboardInterrupt:
        print("\nInterrupted by user")
    finally:
        tester.cleanup()
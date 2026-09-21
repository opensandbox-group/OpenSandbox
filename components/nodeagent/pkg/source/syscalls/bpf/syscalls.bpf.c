// Copyright 2026 Alibaba Group Holding Ltd.
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

#include <linux/bpf.h>
#include <bpf/bpf_helpers.h>

char LICENSE[] SEC("license") = "GPL";

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 65536);
    __type(key, __u64);
    __type(value, __u64);
} tracked_cgroups SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 1 << 20);
} events SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 65536);
    __type(key, __u64);
    __type(value, __u64);
} lost_events SEC(".maps");

struct syscall_event {
    __u64 ktime_ns;
    __u64 cgroup_id;
    __u64 handle;
    __u32 host_pid;
    __u32 host_tid;
    __s64 syscall_nr;
    char comm[16];
} __attribute__((packed));

struct syscall_enter_context {
    __u64 _tracepoint_header;
    long id;
    unsigned long args[6];
};

SEC("tracepoint/raw_syscalls/sys_enter")
int collect_syscall(struct syscall_enter_context *ctx)
{
    __u64 cgroup_id = bpf_get_current_cgroup_id();
    __u64 *handle = bpf_map_lookup_elem(&tracked_cgroups, &cgroup_id);
    __u64 pid_tgid;

    if (!handle)
        return 0;

    struct syscall_event event = {};
    event.ktime_ns = bpf_ktime_get_ns();
    event.cgroup_id = cgroup_id;
    event.handle = *handle;
    pid_tgid = bpf_get_current_pid_tgid();
    event.host_pid = pid_tgid >> 32;
    event.host_tid = (__u32)pid_tgid;
    event.syscall_nr = ctx->id;
    bpf_get_current_comm(&event.comm, sizeof(event.comm));

    if (bpf_ringbuf_output(&events, &event, sizeof(event), 0)) {
        __u64 zero = 0;
        __u64 *lost = bpf_map_lookup_elem(&lost_events, handle);

        if (!lost) {
            bpf_map_update_elem(&lost_events, handle, &zero, BPF_NOEXIST);
            lost = bpf_map_lookup_elem(&lost_events, handle);
        }
        if (lost)
            __sync_fetch_and_add(lost, 1);
    }
    return 0;
}

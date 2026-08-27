/* SPDX-License-Identifier: GPL-2.0 */
/*
 * Minimal kernel type definitions for the audit BPF programs (CO-RE).
 *
 * This REPLACES vmlinux.h: CO-RE resolves every accessor by FIELD NAME
 * against the target kernel's BTF at load time, so the compile-time type
 * layout here does not need to match any real kernel. We only need the
 * handful of structures and members the programs touch, declared with the
 * same member NAMES (types/offsets are irrelevant). Keeping this header
 * small (vs. the ~150k-line per-arch vmlinux.h) makes the build hermetic
 * and architecture-independent.
 *
 * If a member below is renamed/removed in a future kernel, the CO-RE
 * relocation fails open (the hook degrades) — extend this header to match.
 *
 * Members used:
 *   - task_struct: real_parent, pid, real_cred
 *   - cred: uid.val, gid.val, cap_effective
 *   - trace_event_raw_sched_process_exec: __data_loc_filename
 *   - trace_event_raw_inet_sock_set_state: newstate, skaddr
 *   - sock: __sk_common
 *   - sock_common: skc_family, skc_daddr, skc_v6_daddr.in6_u.u6_addr32,
 *     skc_dport
 */

#ifndef __AUDIT_TYPES_H
#define __AUDIT_TYPES_H

typedef unsigned char __u8;
typedef unsigned int __u32;
typedef unsigned long long __u64;
typedef unsigned short __u16;
typedef int __s32;
typedef long long __s64;
typedef __u32 __be32;
typedef __u16 __be16;
typedef __u32 __wsum;

typedef __u32 uint32_t;
typedef __u64 uint64_t;
typedef __u16 uint16_t;

/* Forward declarations used by bpf_helper_defs.h helper prototypes. */
struct __sk_buff;

/* Map type used by the events ring buffer (bpf_helpers.h). */
#define BPF_MAP_TYPE_RINGBUF 27

struct task_struct {
	void *real_parent;
	unsigned int pid;
	void *real_cred;
};

struct kuid_t {
	__u32 val;
};
struct kgid_t {
	__u32 val;
};
struct kernel_cap_t {
	__u64 val;
};

struct cred {
	struct kuid_t uid;
	struct kgid_t gid;
	struct kernel_cap_t cap_effective;
};

/* trace_event_raw_sched_process_exec (5.16+ shape; field_exists probes
 * older kernels via the inline-filename fallback in audit.bpf.c). */
struct trace_event_raw_sched_process_exec {
	void *ent;
	__u32 pid;
	__u32 old_pid;
	__u32 __data_loc_filename;
};

struct trace_event_raw_inet_sock_set_state {
	void *ent;
	void *skaddr;
	int oldstate;
	int newstate;
};

struct in6_addr {
	union {
		__u8 u6_addr8[16];
		__be32 u6_addr32[4];
	} in6_u;
};

struct sock_common {
	__u16 skc_dport;
	__u32 skc_daddr;
	struct in6_addr skc_v6_daddr;
	unsigned short skc_family;
};

struct sock {
	struct sock_common __sk_common;
};

/* pt_regs: only the member BPF_KPROBE's PT_REGS_PARM1 reads is needed;
 * the arch macro (__TARGET_ARCH_x86/arm64) is set by bpf2go/clang. */
struct pt_regs;

#if defined(__TARGET_ARCH_arm64)
/* arm64: BPF_KPROBE uses PT_REGS_ARM64 = struct user_pt_regs, PARM1 = regs[0] */
struct user_pt_regs {
	__u64 regs[31];
	__u64 sp;
	__u64 pc;
	__u64 pstate;
};
struct pt_regs {
	struct user_pt_regs user_regs;
};
#else
/* x86-64: PT_REGS_PARM1(x) = (x)->rdi (bpf_tracing.h, non-__VMLINUX_H__ path) */
struct pt_regs {
	__u64 r15, r14, r13, r12, bp, bx, r11, r10;
	__u64 r9, r8, rax, rcx, rdx, rsi, rdi, orig_rax, rip;
	__u64 cs, eflags, rsp, ss;
};
#endif

#endif /* __AUDIT_TYPES_H */

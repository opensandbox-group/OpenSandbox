/*
 * Copyright 2026 Alibaba Group Holding Ltd.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

/*
 * opensandbox-launcher is the pre-exec hardening prelude (OSEP-0018 §4).
 *
 * execd execs it as the child's argv[0]; it applies the privilege floor in
 * the child between fork and exec, then execve(2)s the real user command:
 *
 *   1. unset execd's credential env vars
 *   2. prctl(PR_SET_KEEPCAPS)                 (keep caps across the uid change)
 *   3. drop every bounding-set cap not kept   (needs CAP_SETPCAP)
 *   4. prctl(PR_SET_NO_NEW_PRIVS)
 *   5. setgroups + setgid + setuid            (identity drop)
 *   6. capset permitted/effective to the kept caps; PR_CAP_AMBIENT_RAISE each
 *   7. seccomp BPF filter (SECCOMP_MODE_FILTER) — LAST, so it never blocks
 *      the launcher's own setup syscalls above
 *   8. execve(user argv)
 *
 * The order is pinned by Linux semantics. Every step is best-effort and
 * fail-open (logged, never fatal); a malformed policy exits without
 * executing the workload.
 */

#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <grp.h>
#include <linux/capability.h>
#include <linux/filter.h>
#include <linux/seccomp.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/prctl.h>
#include <sys/syscall.h>
#include <sys/types.h>
#include <unistd.h>

#define POLICY_MAGIC 0x4f534258u /* "OSBX" */
#define POLICY_VERSION 1u

#define FLAG_UID_DROP 0x1u
#define FLAG_CAP_DROP 0x2u

#define LAUNCH_FAILURE 125
#define EXEC_FAILURE 126

/* Keep in sync with policyHeader in hardening_linux.go. */
struct policy_header {
    uint32_t magic;
    uint32_t version;
    uint32_t flags;
    uint32_t uid;
    uint32_t gid;
    uint32_t n_groups;
    uint32_t n_keepcaps;
    uint32_t n_env;
    uint32_t seccomp_len;
    uint32_t landlock_len;
};

#define MAX_CAPS 64

/*
 * Landlock ABI (stable since Linux 5.13): syscalls 444-446 and the fs access
 * bits below are part of the kernel UAPI and are defined here so the helper
 * does not depend on a specific linux/landlock.h.
 */
#define LL_EXECUTE (1ULL << 0)
#define LL_WRITE_FILE (1ULL << 1)
#define LL_READ_FILE (1ULL << 2)
#define LL_READ_DIR (1ULL << 3)
#define LL_REMOVE_DIR (1ULL << 4)
#define LL_REMOVE_FILE (1ULL << 5)
#define LL_MAKE_CHAR (1ULL << 6)
#define LL_MAKE_DIR (1ULL << 7)
#define LL_MAKE_REG (1ULL << 8)
#define LL_MAKE_SOCK (1ULL << 9)
#define LL_MAKE_FIFO (1ULL << 10)
#define LL_MAKE_BLOCK (1ULL << 11)
#define LL_MAKE_SYM (1ULL << 12)
#define LL_REFER (1ULL << 13)   /* ABI >= 2 */
#define LL_TRUNCATE (1ULL << 14) /* ABI >= 3 */

#define LANDLOCK_CREATE_RULESET_VERSION 1
#define LANDLOCK_CREATE_RULESET 0
#define LANDLOCK_RULE_PATH_BENEATH 1

struct ll_ruleset_attr {
    uint64_t handled_access_fs;
};

struct ll_path_beneath_attr {
    uint64_t allowed_access;
    int32_t parent_fd;
};

/* Landlock rule from the policy: a path plus the access bits to grant.
 * required rules must all install, or confinement is skipped entirely. */
struct ll_rule {
    uint64_t access;
    const char *path;
    int required;
};

static void log_err(const char *msg, int err)
{
    fprintf(stderr, "opensandbox-launcher: %s: %s\n", msg, strerror(err));
}

static void fail(int fd, const char *msg)
{
    if (fd >= 0)
        (void)close(fd);
    fprintf(stderr, "opensandbox-launcher: %s\n", msg);
    _exit(LAUNCH_FAILURE);
}

/* Read exactly n bytes from fd, retrying on EINTR and short reads. */
static int read_exact(int fd, void *buf, size_t n)
{
    uint8_t *p = buf;
    size_t left = n;

    while (left > 0) {
        ssize_t got = read(fd, p, left);
        if (got < 0) {
            if (errno == EINTR)
                continue;
            return -1;
        }
        if (got == 0)
            return -1; /* EOF before the expected length */
        p += got;
        left -= (size_t)got;
    }
    return 0;
}

/* Capability ABI v3: two data blocks covering caps 0..63. */
struct cap_header {
    uint32_t version;
    int pid;
};

struct cap_data {
    uint32_t effective;
    uint32_t permitted;
    uint32_t inheritable;
};

static int capset_all(uint64_t kept)
{
    struct cap_header header;
    struct cap_data data[2];

    memset(&header, 0, sizeof(header));
    memset(data, 0, sizeof(data));
    header.version = _LINUX_CAPABILITY_VERSION_3;
    header.pid = 0;
    /* Capability ABI v3 uses two 32-bit words; caps above 31 (e.g.
     * CAP_PERFMON, CAP_BPF) live in the second one. */
    data[0].effective = (uint32_t)kept;
    data[0].permitted = (uint32_t)kept;
    data[0].inheritable = (uint32_t)kept;
    data[1].effective = (uint32_t)(kept >> 32);
    data[1].permitted = (uint32_t)(kept >> 32);
    data[1].inheritable = (uint32_t)(kept >> 32);
    return syscall(SYS_capset, &header, data);
}

static int raise_ambient(uint32_t cap)
{
    return prctl(PR_CAP_AMBIENT, PR_CAP_AMBIENT_RAISE, (unsigned long)cap, 0, 0);
}

/* Trim an access mask to what the detected Landlock ABI supports. */
static uint64_t ll_trim_access(uint64_t access, long abi)
{
    if (abi < 2)
        access &= ~(uint64_t)LL_REFER;
    if (abi < 3)
        access &= ~(uint64_t)LL_TRUNCATE;
    return access;
}

/* Apply the Landlock filesystem confinement from the policy (fail-open). */
static void apply_landlock(const struct ll_rule *rules, size_t n_rules)
{
    long abi;
    int ruleset = -1;
    uint64_t handled;

    if (n_rules == 0)
        return;

    abi = syscall(444, NULL, 0,
                  LANDLOCK_CREATE_RULESET_VERSION);
    if (abi < 1) {
        log_err("landlock unavailable (kernel ABI < 1)", ENOSYS);
        return;
    }

    handled = LL_EXECUTE | LL_WRITE_FILE | LL_READ_FILE | LL_READ_DIR |
              LL_REMOVE_DIR | LL_REMOVE_FILE | LL_MAKE_CHAR | LL_MAKE_DIR |
              LL_MAKE_REG | LL_MAKE_SOCK | LL_MAKE_FIFO | LL_MAKE_BLOCK |
              LL_MAKE_SYM | LL_REFER | LL_TRUNCATE;
    handled = ll_trim_access(handled, abi);

    {
        struct ll_ruleset_attr attr = { .handled_access_fs = handled };

        ruleset = (int)syscall(444, &attr,
                               sizeof(attr), LANDLOCK_CREATE_RULESET);
        if (ruleset < 0) {
            log_err("landlock_create_ruleset", errno);
            return;
        }
    }

    {
        int rule_failed = 0;

        for (size_t i = 0; i < n_rules; i++) {
            int fd;
            uint64_t access = ll_trim_access(rules[i].access, abi);

            if (access == 0)
                continue;
            /* O_PATH: no read/write rights needed to build the rule. */
            fd = open(rules[i].path, O_PATH | O_CLOEXEC);
            if (fd < 0) {
                fprintf(stderr,
                        "opensandbox-launcher: landlock: skip %s: %s\n",
                        rules[i].path, strerror(errno));
                rule_failed |= rules[i].required;
                continue;
            }
            {
                struct ll_path_beneath_attr path_attr = {
                    .allowed_access = access,
                    .parent_fd = fd,
                };

                if (syscall(445, ruleset,
                            LANDLOCK_RULE_PATH_BENEATH, &path_attr, 0) != 0) {
                    fprintf(stderr,
                            "opensandbox-launcher: landlock: add_rule %s: %s\n",
                            rules[i].path, strerror(errno));
                    rule_failed |= rules[i].required;
                }
            }
            close(fd);
        }

        if (rule_failed) {
            /* Fail closed: a missing required rule would silently deny
             * operator-granted access, so skip confinement entirely.
             * Best-effort (mount-expansion) failures only log. */
            fprintf(stderr,
                    "opensandbox-launcher: landlock: rule installation failed; "
                    "skipping filesystem confinement for this launch\n");
            close(ruleset);
            return;
        }
    }

    if (syscall(446, ruleset, 0) != 0) {
        log_err("landlock_restrict_self", errno);
        return;
    }
    close(ruleset);
    /* Irrevocable: from here on the process is confined to the granted set. */
}

int main(int argc, char **argv)
{
    int policy_fd;
    struct policy_header hdr;
    uint32_t keepcaps[MAX_CAPS];
    uint32_t groups[MAX_CAPS];
    char *env_names[MAX_CAPS];
    int n_env;
    size_t env_budget;
    struct sock_filter *filter = NULL;
    struct sock_fprog prog;
    struct ll_rule *ll_rules = NULL;
    size_t n_ll_rules = 0;

    if (argc < 4 || strcmp(argv[2], "--") != 0)
        fail(-1, "usage: opensandbox-launcher <policy-fd> -- <argv...>");

    {
        char *end = NULL;
        long parsed;

        errno = 0;
        parsed = strtol(argv[1], &end, 10);
        if (errno != 0 || end == argv[1] || *end != '\0' ||
            parsed < 3 || parsed > INT32_MAX)
            fail(-1, "invalid policy descriptor");
        policy_fd = (int)parsed;
    }

    if (fcntl(policy_fd, F_GETFD) < 0)
        fail(policy_fd, "policy descriptor is not open");

    if (read_exact(policy_fd, &hdr, sizeof(hdr)) != 0)
        fail(policy_fd, "truncated policy header");
    if (hdr.magic != POLICY_MAGIC || hdr.version != POLICY_VERSION)
        fail(policy_fd, "invalid policy header");

    if (hdr.n_groups > MAX_CAPS)
        fail(policy_fd, "too many supplementary groups");
    if (hdr.n_keepcaps > MAX_CAPS)
        fail(policy_fd, "too many kept capabilities");
    if (hdr.n_env > MAX_CAPS)
        fail(policy_fd, "too many environment names");
    if (hdr.seccomp_len % sizeof(struct sock_filter) != 0)
        fail(policy_fd, "seccomp filter length is not a multiple of sock_filter");

    if (hdr.n_groups > 0 &&
        read_exact(policy_fd, groups, hdr.n_groups * sizeof(uint32_t)) != 0)
        fail(policy_fd, "truncated group list");
    if (hdr.n_keepcaps > 0 &&
        read_exact(policy_fd, keepcaps, hdr.n_keepcaps * sizeof(uint32_t)) != 0)
        fail(policy_fd, "truncated capability list");

    /* Env names are NUL-terminated strings packed back to back. Bound the
     * total budget so a malicious policy cannot exhaust the stack. */
    env_budget = hdr.n_env * 64u;
    if (env_budget > 4096u)
        fail(policy_fd, "environment names exceed the policy budget");
    n_env = 0;
    while (n_env < (int)hdr.n_env) {
        static char env_buf[4096];
        size_t off = 0;

        while (off + 1 < sizeof(env_buf)) {
            if (read(policy_fd, &env_buf[off], 1) != 1)
                fail(policy_fd, "truncated environment name");
            if (env_buf[off] == '\0')
                break;
            off++;
        }
        if (off + 1 >= sizeof(env_buf) && env_buf[off] != '\0')
            fail(policy_fd, "environment name too long");
        env_buf[off] = '\0';
        env_names[n_env++] = strdup(env_buf);
        if (env_names[n_env - 1] == NULL)
            fail(policy_fd, "out of memory for environment name");
    }

    if (hdr.seccomp_len > 0) {
        filter = (struct sock_filter *)malloc(hdr.seccomp_len);
        if (filter == NULL)
            fail(policy_fd, "out of memory for seccomp filter");
        if (read_exact(policy_fd, filter, hdr.seccomp_len) != 0)
            fail(policy_fd, "truncated seccomp filter");
    }

    /* Landlock rules: repeated { u8 required; u64 access; u16 pathlen;
     * path bytes }. Allocated dynamically: mount-heavy pods can exceed any
     * fixed cap after the mount-expansion in the policy. */
    if (hdr.landlock_len > 0) {
        size_t left = hdr.landlock_len;
        size_t max_rules = hdr.landlock_len / (sizeof(uint8_t) + sizeof(uint64_t) + sizeof(uint16_t));

        ll_rules = (struct ll_rule *)calloc(max_rules + 1, sizeof(struct ll_rule));
        if (ll_rules == NULL)
            fail(policy_fd, "out of memory for landlock rules");

        while (left > 0) {
            uint8_t required;
            uint64_t access;
            uint16_t pathlen;
            char *path;

            if (left < sizeof(required) + sizeof(access) + sizeof(pathlen))
                fail(policy_fd, "truncated landlock rule header");
            if (read_exact(policy_fd, &required, sizeof(required)) != 0 ||
                read_exact(policy_fd, &access, sizeof(access)) != 0 ||
                read_exact(policy_fd, &pathlen, sizeof(pathlen)) != 0)
                fail(policy_fd, "truncated landlock rule header");
            left -= sizeof(required) + sizeof(access) + sizeof(pathlen);
            if (pathlen == 0 || pathlen > 4096)
                fail(policy_fd, "invalid landlock path length");
            if (left < pathlen)
                fail(policy_fd, "truncated landlock path");
            path = malloc((size_t)pathlen + 1);
            if (path == NULL)
                fail(policy_fd, "out of memory for landlock path");
            if (read_exact(policy_fd, path, pathlen) != 0)
                fail(policy_fd, "truncated landlock path");
            path[pathlen] = '\0';
            left -= pathlen;
            ll_rules[n_ll_rules].access = access;
            ll_rules[n_ll_rules].path = path;
            ll_rules[n_ll_rules].required = required != 0;
            n_ll_rules++;
        }
        if (left > 0)
            fail(policy_fd, "truncated landlock rules");
    }

    if (close(policy_fd) != 0)
        _exit(LAUNCH_FAILURE);

    /* 1. Strip execd's credential/config env from the workload. */
    for (int i = 0; i < n_env; i++) {
        unsetenv(env_names[i]);
        free(env_names[i]);
    }

    if (hdr.flags & FLAG_CAP_DROP) {
        /* 2. Keep caps across the identity change (step 5 clears them). */
        if (prctl(PR_SET_KEEPCAPS, 1, 0, 0, 0) != 0)
            log_err("PR_SET_KEEPCAPS", errno);

        /* 3. Trim the bounding set while CAP_SETPCAP is still held. */
        for (int cap = 0; cap <= CAP_LAST_CAP; cap++) {
            int keep = 0;

            for (uint32_t k = 0; k < hdr.n_keepcaps; k++) {
                if ((uint32_t)cap == keepcaps[k]) {
                    keep = 1;
                    break;
                }
            }
            if (!keep && prctl(PR_CAPBSET_DROP, (unsigned long)cap, 0, 0, 0) != 0 &&
                errno != EPERM && errno != EINVAL)
                log_err("PR_CAPBSET_DROP", errno);
        }
    }

    /* 4. No new privileges: nothing below can regain what the launcher drops. */
    if (prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != 0)
        log_err("PR_SET_NO_NEW_PRIVS", errno);

    if (hdr.flags & FLAG_UID_DROP) {
        /* 5. Identity change: a foreign target that cannot be applied must
         * abort the launch rather than silently run as the wrong user. */
        if (hdr.n_groups > 0) {
            gid_t *gids = (gid_t *)malloc(hdr.n_groups * sizeof(gid_t));

            if (gids == NULL)
                fail(-1, "out of memory for supplementary groups");
            for (uint32_t g = 0; g < hdr.n_groups; g++)
                gids[g] = (gid_t)groups[g];
            if (setgroups(hdr.n_groups, gids) != 0)
                fail(-1, "setgroups: cannot apply requested supplementary groups");
            free(gids);
        } else if (setgroups(0, NULL) != 0) {
            fail(-1, "setgroups: cannot clear supplementary groups");
        }
        if (setgid((gid_t)hdr.gid) != 0)
            fail(-1, "setgid: cannot apply requested gid");
        if (setuid((uid_t)hdr.uid) != 0)
            fail(-1, "setuid: cannot apply requested uid");
    }

    if (hdr.flags & FLAG_CAP_DROP) {
        /* 6. Final cap sets + ambient raise so kept caps survive execve. */
        uint64_t kept = 0;

        for (uint32_t k = 0; k < hdr.n_keepcaps; k++)
            kept |= (UINT64_C(1) << keepcaps[k]);
        if (capset_all(kept) != 0)
            log_err("capset", errno);
        for (uint32_t k = 0; k < hdr.n_keepcaps; k++) {
            if (raise_ambient(keepcaps[k]) != 0)
                log_err("PR_CAP_AMBIENT_RAISE", errno);
        }
    }

    /* 6. Landlock filesystem confinement, before seccomp. */
    apply_landlock(ll_rules, n_ll_rules);
    free(ll_rules);

    /* 7. Seccomp LAST: it must never block the setup above, and execve
     * (which the Go side reserves from the deny list) is still allowed. */
    if (filter != NULL) {
        prog.len = (unsigned short)(hdr.seccomp_len / sizeof(struct sock_filter));
        prog.filter = filter;
        if (prctl(PR_SET_SECCOMP, SECCOMP_MODE_FILTER, &prog) != 0)
            log_err("PR_SET_SECCOMP", errno);
        free(filter);
    }

    /* 8. Exec the real workload. */
    execvp(argv[3], &argv[3]);
    fprintf(stderr, "opensandbox-launcher: exec %s: %s\n", argv[3], strerror(errno));
    _exit(EXEC_FAILURE);
}

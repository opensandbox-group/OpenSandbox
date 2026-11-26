// Copyright 2026 The OpenSandbox Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <linux/filter.h>
#include <linux/seccomp.h>
#include <sched.h>
#include <stddef.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/prctl.h>
#include <sys/resource.h>
#include <sys/syscall.h>
#include <unistd.h>

static int read_oom(void)
{
    int value;
    FILE *f = fopen("/proc/self/oom_score_adj", "r");
    if (!f || fscanf(f, "%d", &value) != 1)
        exit(2);
    fclose(f);
    return value;
}

static int write_oom(const char *value)
{
    int fd = open("/proc/self/oom_score_adj", O_WRONLY);
    int result;
    if (fd < 0)
        return errno;
    errno = 0;
    result = write(fd, value, strlen(value)) == (ssize_t)strlen(value) ? 0 : (errno != 0 ? errno : EIO);
    close(fd);
    return result;
}

int main(int argc, char **argv)
{
    if (argc == 2 && strcmp(argv[1], "argv0") == 0) {
        puts(argv[0]);
        return 0;
    }
    if (argc == 2 && strcmp(argv[1], "groups") == 0) {
        int count = getgroups(0, NULL);
        if (count < 0)
            return 2;
        gid_t *groups = calloc((size_t)count + 1, sizeof(gid_t));
        if (!groups || getgroups(count, groups) != count)
            return 2;
        printf("[");
        for (int i = 0; i < count; i++)
            printf("%s%u", i == 0 ? "" : ",", (unsigned int)groups[i]);
        puts("]");
        free(groups);
        return 0;
    }
    if (argc > 3 && strcmp(argv[1], "inherit") == 0) {
        if (write_oom(argv[2]) != 0)
            return 2;
        execvp(argv[3], &argv[3]);
        return 3;
    }
    if (argc > 2 && strcmp(argv[1], "deny-qos") == 0) {
        struct sock_filter filters[] = {
            BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, nr)),
            BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, SYS_sched_setscheduler, 3, 0),
            BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, SYS_setpriority, 2, 0),
            BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, SYS_pwrite64, 1, 0),
            BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ALLOW),
            BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ERRNO | EPERM),
        };
        struct sock_fprog prog = {sizeof(filters) / sizeof(filters[0]), filters};
        if (prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != 0 ||
            prctl(PR_SET_SECCOMP, SECCOMP_MODE_FILTER, &prog) != 0)
            return 2;
        execvp(argv[2], &argv[2]);
        return 3;
    }
    struct rlimit limit;
    int oom = read_oom();
    int lower_errno = -1;
    int nice_errno = -1;
    errno = 0;
    int nice_value = getpriority(PRIO_PROCESS, 0);
    if (nice_value == -1 && errno != 0)
        return 2;
    unsigned long long caps = 0;
    int seccomp = 0;
    char line[256];
    FILE *status = fopen("/proc/self/status", "r");
    if (!status)
        return 2;
    while (fgets(line, sizeof(line), status)) {
        if (strncmp(line, "CapEff:", 7) == 0 && sscanf(line + 7, "%llx", &caps) != 1)
            return 2;
        if (strncmp(line, "Seccomp:", 8) == 0 && sscanf(line + 8, "%d", &seccomp) != 1)
            return 2;
    }
    fclose(status);
    if (getrlimit(RLIMIT_NICE, &limit) != 0)
        return 2;
    if (argc == 3 && strcmp(argv[1], "lower") == 0) {
        lower_errno = write_oom(argv[2]);
        errno = 0;
        nice_errno = setpriority(PRIO_PROCESS, 0, nice_value - 1) == 0 ? 0 : errno;
    }
    printf("{\"nice\":%d,\"scheduler\":%d,\"oom\":%d,\"oom_after\":%d,"
           "\"soft\":%llu,\"hard\":%llu,\"nnp\":%d,\"uid\":%d,\"gid\":%d,"
           "\"lower_errno\":%d,\"nice_errno\":%d,\"caps\":%llu,\"seccomp\":%d}\n",
           nice_value, sched_getscheduler(0), oom, read_oom(),
           (unsigned long long)limit.rlim_cur, (unsigned long long)limit.rlim_max,
           prctl(PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0), getuid(), getgid(), lower_errno, nice_errno, caps, seccomp);
    return 0;
}

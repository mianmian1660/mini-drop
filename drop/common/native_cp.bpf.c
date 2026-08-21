// Minimal CO-RE event collector shared by strict continuous Sessions.
// The userspace loader updates target_tgids in place and drains histograms
// every base slice. Keys retain TGID so process Sessions can be isolated.
#include <linux/types.h>
#include <linux/bpf.h>
#include <asm/unistd.h>
#include <bpf/bpf_helpers.h>

char LICENSE[] SEC("license") = "GPL";

enum cp_signal {
    CP_BLOCK = 1,
    CP_SYSCALL = 2,
    CP_SCHED = 3,
};

struct cp_hist_key {
    __u32 signal;
    __u32 tgid;
    __u32 slot;
};

struct cp_target {
    __u8 enabled;
};

struct cp_request_key {
    __u32 dev;
    __u64 sector;
};

struct cp_request_value {
    __u64 started_ns;
    __u32 tgid;
};

struct cp_pending_value {
    __u64 started_ns;
    __u32 tgid;
};

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 1024);
    __type(key, __u32);
    __type(value, struct cp_target);
} target_tgids SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 16384);
    __type(key, struct cp_hist_key);
    __type(value, __u64);
} histogram_a SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 16384);
    __type(key, struct cp_hist_key);
    __type(value, __u64);
} histogram_b SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, __u32);
} active_histogram SEC(".maps");

// sched_wakeup exposes the awakened TID, not its TGID. Userspace refreshes
// this map from /proc/<tgid>/task while updating the authoritative TGID set.
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 32768);
    __type(key, __u32);
    __type(value, __u32);
} target_tids SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, __u64);
} lost_events SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 8192);
    __type(key, struct cp_request_key);
    __type(value, struct cp_request_value);
} block_requests SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 16384);
    __type(key, __u32);
    __type(value, struct cp_pending_value);
} syscall_pending SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 16384);
    __type(key, __u32);
    __type(value, struct cp_pending_value);
} wake_pending SEC(".maps");

struct trace_block_issue {
    __u8 common[8];
    __u32 dev;
    __u32 pad0;
    __u64 sector;
    __u32 bytes;
    __u32 nr_sector;
    char rwbs[8];
    char comm[16];
};

struct trace_block_complete {
    __u8 common[8];
    __u32 dev;
    __u32 pad0;
    __u64 sector;
    __u32 error;
    char rwbs[8];
};

struct trace_sched_wakeup {
    __u8 common[8];
    char comm[16];
    __s32 pid;
};

struct trace_sched_switch {
    __u8 common[8];
    char prev_comm[16];
    __s32 prev_pid;
    __s32 prev_prio;
    __s64 prev_state;
    char next_comm[16];
    __s32 next_pid;
};

static __always_inline __u32 current_tgid(void)
{
    return (__u32)(bpf_get_current_pid_tgid() >> 32);
}

static __always_inline int target_enabled(__u32 tgid)
{
    struct cp_target *target = bpf_map_lookup_elem(&target_tgids, &tgid);
    if (target)
        return 1;
    __u32 host_key = 0;
    return bpf_map_lookup_elem(&target_tgids, &host_key) != 0;
}

static __always_inline void count_lost(void)
{
    __u32 zero = 0;
    __u64 *lost = bpf_map_lookup_elem(&lost_events, &zero);
    if (lost)
        __sync_fetch_and_add(lost, 1);
}

static __always_inline __u32 target_tgid_for_tid(__u32 tid)
{
    __u32 *tgid = bpf_map_lookup_elem(&target_tids, &tid);
    if (tgid)
        return *tgid;
    // Host scope is represented by the wildcard TGID key. Preserve a useful
    // identity even when the tracepoint exposes only a TID.
    __u32 host_key = 0;
    return bpf_map_lookup_elem(&target_tgids, &host_key) ? tid : 0;
}

static __always_inline int io_syscall(__s32 id)
{
    return id == __NR_read || id == __NR_write ||
           id == __NR_pread64 || id == __NR_pwrite64 ||
           id == __NR_readv || id == __NR_writev ||
           id == __NR_preadv || id == __NR_pwritev ||
           id == __NR_preadv2 || id == __NR_pwritev2;
}

static __always_inline __u32 latency_slot(__u64 latency_us)
{
    __u32 slot = 0;
    if (latency_us > 1) slot = 1;
    if (latency_us > 2) slot = 2;
    if (latency_us > 4) slot = 3;
    if (latency_us > 8) slot = 4;
    if (latency_us > 16) slot = 5;
    if (latency_us > 32) slot = 6;
    if (latency_us > 64) slot = 7;
    if (latency_us > 128) slot = 8;
    if (latency_us > 256) slot = 9;
    if (latency_us > 512) slot = 10;
    if (latency_us > 1024) slot = 11;
    if (latency_us > 2048) slot = 12;
    if (latency_us > 4096) slot = 13;
    if (latency_us > 8192) slot = 14;
    if (latency_us > 16384) slot = 15;
    return slot;
}

static __always_inline void record_latency(__u32 signal, __u32 tgid, __u64 delta_ns)
{
    struct cp_hist_key key = {
        .signal = signal,
        .tgid = tgid,
        .slot = latency_slot(delta_ns / 1000),
    };
    __u32 zero = 0;
    __u32 *active = bpf_map_lookup_elem(&active_histogram, &zero);
    __u64 *count;
    __u64 initial = 1;
    if (active && (*active & 1)) {
        count = bpf_map_lookup_elem(&histogram_b, &key);
        if (count)
            __sync_fetch_and_add(count, 1);
        else if (bpf_map_update_elem(&histogram_b, &key, &initial, BPF_NOEXIST) != 0) {
            count = bpf_map_lookup_elem(&histogram_b, &key);
            if (count)
                __sync_fetch_and_add(count, 1);
            else
                count_lost();
        }
    } else {
        count = bpf_map_lookup_elem(&histogram_a, &key);
        if (count)
            __sync_fetch_and_add(count, 1);
        else if (bpf_map_update_elem(&histogram_a, &key, &initial, BPF_NOEXIST) != 0) {
            count = bpf_map_lookup_elem(&histogram_a, &key);
            if (count)
                __sync_fetch_and_add(count, 1);
            else
                count_lost();
        }
    }
}

SEC("tracepoint/block/block_rq_issue")
int cp_block_issue(struct trace_block_issue *ctx)
{
    __u32 tgid = current_tgid();
    if (!target_enabled(tgid))
        return 0;
    struct cp_request_key key = {.dev = ctx->dev, .sector = ctx->sector};
    struct cp_request_value value = {.started_ns = bpf_ktime_get_ns(), .tgid = tgid};
    if (bpf_map_update_elem(&block_requests, &key, &value, BPF_ANY) != 0)
        count_lost();
    return 0;
}

SEC("tracepoint/block/block_rq_complete")
int cp_block_complete(struct trace_block_complete *ctx)
{
    struct cp_request_key key = {.dev = ctx->dev, .sector = ctx->sector};
    struct cp_request_value *value = bpf_map_lookup_elem(&block_requests, &key);
    if (!value)
        return 0;
    record_latency(CP_BLOCK, value->tgid, bpf_ktime_get_ns() - value->started_ns);
    bpf_map_delete_elem(&block_requests, &key);
    return 0;
}

SEC("tracepoint/raw_syscalls/sys_enter")
int cp_sys_enter(struct { __u8 common[8]; __s32 id; __u64 args[6]; } *ctx)
{
    __u32 tgid = current_tgid();
    if (!target_enabled(tgid) || !io_syscall(ctx->id))
        return 0;
    __u32 tid = (__u32)bpf_get_current_pid_tgid();
    struct cp_pending_value value = {.started_ns = bpf_ktime_get_ns(), .tgid = tgid};
    if (bpf_map_update_elem(&syscall_pending, &tid, &value, BPF_ANY) != 0)
        count_lost();
    return 0;
}

SEC("tracepoint/raw_syscalls/sys_exit")
int cp_sys_exit(struct { __u8 common[8]; __s32 id; long ret; } *ctx)
{
    __u32 tid = (__u32)bpf_get_current_pid_tgid();
    struct cp_pending_value *value = bpf_map_lookup_elem(&syscall_pending, &tid);
    if (!value)
        return 0;
    record_latency(CP_SYSCALL, value->tgid, bpf_ktime_get_ns() - value->started_ns);
    bpf_map_delete_elem(&syscall_pending, &tid);
    return 0;
}

static __always_inline int handle_sched_wakeup(struct trace_sched_wakeup *ctx)
{
    __u32 pid = (__u32)ctx->pid;
    __u32 tgid = target_tgid_for_tid(pid);
    if (!tgid)
        return 0;
    struct cp_pending_value value = {.started_ns = bpf_ktime_get_ns(), .tgid = tgid};
    if (bpf_map_update_elem(&wake_pending, &pid, &value, BPF_ANY) != 0)
        count_lost();
    return 0;
}

SEC("tracepoint/sched/sched_wakeup")
int cp_sched_wakeup(struct trace_sched_wakeup *ctx)
{
    return handle_sched_wakeup(ctx);
}

SEC("tracepoint/sched/sched_wakeup_new")
int cp_sched_wakeup_new(struct trace_sched_wakeup *ctx)
{
    return handle_sched_wakeup(ctx);
}

SEC("tracepoint/sched/sched_switch")
int cp_sched_switch(struct trace_sched_switch *ctx)
{
    __u32 pid = (__u32)ctx->next_pid;
    struct cp_pending_value *value = bpf_map_lookup_elem(&wake_pending, &pid);
    if (!value)
        return 0;
    record_latency(CP_SCHED, value->tgid, bpf_ktime_get_ns() - value->started_ns);
    bpf_map_delete_elem(&wake_pending, &pid);
    return 0;
}

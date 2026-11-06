//go:build ignore

// SPDX-License-Identifier: GPL-2.0 OR MIT
//
// Run queue latency per cgroup: the time from a task being ready to run until it
// gets a CPU, as a log2 histogram in microseconds.
//
// vmlinux.h comes from: bpftool btf dump file /sys/kernel/btf/vmlinux format c

#include "vmlinux.h"
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

#define SLOTS 32 // must match Slots in hist.go
#define TASK_RUNNING 0

struct hist {
	__u64 slots[SLOTS];
};

// When each ready task became ready, by thread id.
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 65536);
	__type(key, __u32);
	__type(value, __u64);
} ready_at SEC(".maps");

// Cumulative histograms by cgroup id.
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 4096);
	__type(key, __u64);
	__type(value, struct hist);
} hists SEC(".maps");

// Forces struct hist into the object file, so bpf2go generates a Go type for it.
const struct hist *unused_hist __attribute__((unused));

static __always_inline void mark_ready(__u32 pid)
{
	__u64 now;

	if (pid == 0) // the idle task has no wait worth measuring
		return;
	now = bpf_ktime_get_ns();
	bpf_map_update_elem(&ready_at, &pid, &now, BPF_ANY);
}

static __always_inline __u32 log2_slot(__u64 v)
{
	__u32 slot = 0;

	if (v >= 1ULL << 16) {
		slot += 16;
		v >>= 16;
	}
	if (v >= 1ULL << 8) {
		slot += 8;
		v >>= 8;
	}
	if (v >= 1ULL << 4) {
		slot += 4;
		v >>= 4;
	}
	if (v >= 1ULL << 2) {
		slot += 2;
		v >>= 2;
	}
	if (v >= 1ULL << 1)
		slot += 1;
	return slot < SLOTS ? slot : SLOTS - 1;
}

SEC("tp_btf/sched_wakeup")
int BPF_PROG(on_wakeup, struct task_struct *p)
{
	mark_ready(BPF_CORE_READ(p, pid));
	return 0;
}

SEC("tp_btf/sched_wakeup_new")
int BPF_PROG(on_wakeup_new, struct task_struct *p)
{
	mark_ready(BPF_CORE_READ(p, pid));
	return 0;
}

SEC("tp_btf/sched_switch")
int BPF_PROG(on_switch, bool preempt, struct task_struct *prev, struct task_struct *next)
{
	static const struct hist empty;
	__u32 pid, slot;
	__u64 *start, delta_us, cgroup_id;
	struct hist *h;

	// A task preempted while still runnable is ready again straight away.
	if (BPF_CORE_READ(prev, __state) == TASK_RUNNING)
		mark_ready(BPF_CORE_READ(prev, pid));

	pid = BPF_CORE_READ(next, pid);
	start = bpf_map_lookup_elem(&ready_at, &pid);
	if (!start)
		return 0;
	delta_us = (bpf_ktime_get_ns() - *start) / 1000;
	bpf_map_delete_elem(&ready_at, &pid);

	// next's cgroup, not bpf_get_current_cgroup_id(): "current" is still prev here.
	cgroup_id = BPF_CORE_READ(next, cgroups, dfl_cgrp, kn, id);
	h = bpf_map_lookup_elem(&hists, &cgroup_id);
	if (!h) {
		bpf_map_update_elem(&hists, &cgroup_id, &empty, BPF_NOEXIST);
		h = bpf_map_lookup_elem(&hists, &cgroup_id);
		if (!h)
			return 0;
	}
	slot = log2_slot(delta_us);
	__sync_fetch_and_add(&h->slots[slot], 1);
	return 0;
}

char LICENSE[] SEC("license") = "Dual MIT/GPL";

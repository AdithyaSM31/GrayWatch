#!/usr/bin/env bash
# Preflight for the Graywatch VM. Reports every problem, then exits non-zero if
# there were any. It changes nothing on the machine.
set -u

failures=0

fail() {
    echo "FAIL: $1" >&2
    failures=$((failures + 1))
}

if [ "$(stat -fc %T /sys/fs/cgroup 2>/dev/null)" != "cgroup2fs" ]; then
    fail "/sys/fs/cgroup is not cgroup v2 (want cgroup2fs)"
fi

if ! cat /proc/pressure/cpu >/dev/null 2>&1; then
    fail "cannot read /proc/pressure/cpu: this kernel has PSI disabled"
fi

for tool in tc nsenter socat docker; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        fail "$tool is not installed"
    fi
done

if [ "$(id -u)" != "0" ]; then
    fail "must run as root (use sudo)"
fi

# io.max and io.pressure only exist in container cgroups when the io controller
# is enabled for system.slice, which is where Docker puts them.
subtree=/sys/fs/cgroup/system.slice/cgroup.subtree_control
if ! grep -qw io "$subtree" 2>/dev/null; then
    fail "io controller is not enabled in $subtree: set DefaultIOAccounting=yes in /etc/systemd/system.conf, then run systemctl daemon-reexec"
fi

if [ "$failures" -gt 0 ]; then
    echo "$failures preflight check(s) failed." >&2
    exit 1
fi
echo "Preflight OK."

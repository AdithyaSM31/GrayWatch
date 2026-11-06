// Package netstat reads TCP counters from /proc/<pid>/net/snmp. That file shows
// the counters of the process's network namespace, which gives per-container
// numbers from the host without entering the namespace.
package netstat

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type TCPCounters struct {
	OutSegs, RetransSegs uint64
}

// ReadTCPCounters pairs the two "Tcp:" lines by header name, since column
// positions differ between kernels.
func ReadTCPCounters(snmpPath string) (TCPCounters, error) {
	data, err := os.ReadFile(snmpPath)
	if err != nil {
		return TCPCounters{}, err
	}
	var tcp [][]string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "Tcp:") {
			tcp = append(tcp, strings.Fields(line))
		}
	}
	if len(tcp) != 2 {
		return TCPCounters{}, fmt.Errorf("%s: got %d Tcp lines, want 2", snmpPath, len(tcp))
	}
	header, values := tcp[0], tcp[1]
	if len(header) != len(values) {
		return TCPCounters{}, fmt.Errorf("%s: %d Tcp headers but %d values", snmpPath, len(header), len(values))
	}

	var c TCPCounters
	found := 0
	for i, name := range header {
		var dst *uint64
		switch name {
		case "OutSegs":
			dst = &c.OutSegs
		case "RetransSegs":
			dst = &c.RetransSegs
		default:
			continue
		}
		if *dst, err = strconv.ParseUint(values[i], 10, 64); err != nil {
			return TCPCounters{}, fmt.Errorf("%s: bad %s %q", snmpPath, name, values[i])
		}
		found++
	}
	if found != 2 {
		return TCPCounters{}, fmt.Errorf("%s: OutSegs or RetransSegs column missing", snmpPath)
	}
	return c, nil
}

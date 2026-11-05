// Package haproxy talks to HAProxy's runtime API. Each command uses a new TCP
// connection, because HAProxy closes the connection after answering one command.
package haproxy

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"
)

type ServerStat struct {
	Name, Status string
	Scur         int
}

// Down reports whether HAProxy's health checks have failed this server. A
// server taken out on purpose reads MAINT or DRAIN, which is not down.
func (s ServerStat) Down() bool {
	return strings.HasPrefix(s.Status, "DOWN")
}

type State string

const (
	Ready State = "ready"
	Drain State = "drain"
	Maint State = "maint"
)

type Client struct {
	Addr    string
	Timeout time.Duration
}

func (c *Client) command(ctx context.Context, cmd string) (string, error) {
	dialer := net.Dialer{Timeout: c.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp", c.Addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if c.Timeout > 0 {
		if err := conn.SetDeadline(time.Now().Add(c.Timeout)); err != nil {
			return "", err
		}
	}
	if _, err := io.WriteString(conn, cmd+"\n"); err != nil {
		return "", err
	}
	reply, err := io.ReadAll(conn)
	if err != nil {
		return "", err
	}
	return string(reply), nil
}

// ShowStat returns the servers of one backend, without its aggregate rows.
func (c *Client) ShowStat(ctx context.Context, backend string) ([]ServerStat, error) {
	reply, err := c.command(ctx, "show stat")
	if err != nil {
		return nil, err
	}
	body, ok := strings.CutPrefix(reply, "# ")
	if !ok {
		return nil, fmt.Errorf("show stat: unexpected reply %q", strings.TrimSpace(reply))
	}
	r := csv.NewReader(strings.NewReader(body))
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("show stat: %w", err)
	}

	columns := map[string]int{}
	for i, name := range rows[0] {
		columns[name] = i
	}
	need := 0
	for _, name := range []string{"pxname", "svname", "scur", "status"} {
		i, ok := columns[name]
		if !ok {
			return nil, fmt.Errorf("show stat: no %s column", name)
		}
		need = max(need, i+1)
	}

	var servers []ServerStat
	for _, row := range rows[1:] {
		if len(row) < need {
			return nil, fmt.Errorf("show stat: short row %q", strings.Join(row, ","))
		}
		name := row[columns["svname"]]
		if row[columns["pxname"]] != backend || name == "FRONTEND" || name == "BACKEND" {
			continue
		}
		scur, err := strconv.Atoi(row[columns["scur"]])
		if err != nil {
			return nil, fmt.Errorf("show stat: %s: bad scur: %w", name, err)
		}
		servers = append(servers, ServerStat{Name: name, Status: row[columns["status"]], Scur: scur})
	}
	return servers, nil
}

// ServerAddrs returns the address HAProxy is really sending each server's traffic
// to. It resolves names only when it starts, so this can differ from what the name
// resolves to now, and health checks will not show it.
func (c *Client) ServerAddrs(ctx context.Context, backend string) (map[string]string, error) {
	reply, err := c.command(ctx, "show servers state "+backend)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(reply), "\n")
	if len(lines) < 2 || !strings.HasPrefix(lines[1], "# ") {
		return nil, fmt.Errorf("show servers state: unexpected reply %q", strings.TrimSpace(reply))
	}
	columns := strings.Fields(strings.TrimPrefix(lines[1], "# "))
	nameCol, addrCol := slices.Index(columns, "srv_name"), slices.Index(columns, "srv_addr")
	if nameCol < 0 || addrCol < 0 {
		return nil, fmt.Errorf("show servers state: no srv_name or srv_addr column in %q", lines[1])
	}
	addrs := map[string]string{}
	for _, line := range lines[2:] {
		fields := strings.Fields(line)
		if len(fields) <= max(nameCol, addrCol) {
			return nil, fmt.Errorf("show servers state: short row %q", line)
		}
		addrs[fields[nameCol]] = fields[addrCol]
	}
	return addrs, nil
}

// SetState changes a server's admin state. HAProxy answers success with a blank
// line, so any text in the reply is an error.
func (c *Client) SetState(ctx context.Context, backend, server string, s State) error {
	reply, err := c.command(ctx, fmt.Sprintf("set server %s/%s state %s", backend, server, s))
	if err != nil {
		return err
	}
	if text := strings.TrimSpace(reply); text != "" {
		return fmt.Errorf("set server %s/%s state %s: %s", backend, server, s, text)
	}
	return nil
}

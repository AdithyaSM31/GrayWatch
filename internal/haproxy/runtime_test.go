package haproxy

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// fakeRuntime answers each connection the way HAProxy's runtime API does: read
// one command line, write the reply, close. It reports the commands it received.
func fakeRuntime(t *testing.T, reply string) (addr string, commands <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got := make(chan string, 8)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadString('\n')
			got <- line
			conn.Write([]byte(reply))
			conn.Close()
		}
	}()
	return ln.Addr().String(), got
}

func TestServerAddrs(t *testing.T) {
	reply := "1\n# be_id be_name srv_id srv_name srv_addr srv_op_state srv_admin_state\n" +
		"3 be_app 1 app-01 172.18.0.14 2 0\n3 be_app 2 app-02 172.18.0.18 2 0\n\n"
	addr, commands := fakeRuntime(t, reply)
	got, err := newClient(addr).ServerAddrs(context.Background(), "be_app")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["app-01"] != "172.18.0.14" || got["app-02"] != "172.18.0.18" {
		t.Fatalf("got %v", got)
	}
	if cmd := <-commands; cmd != "show servers state be_app\n" {
		t.Fatalf("command = %q", cmd)
	}
}

func TestServerAddrsRejectsUnexpectedReply(t *testing.T) {
	for _, reply := range []string{
		"Can't find backend.\n",
		"1\n# be_id be_name srv_id srv_name srv_op_state\n3 be_app 1 app-01 2\n",
		"1\n# be_id be_name srv_id srv_name srv_addr\n3 be_app 1\n",
	} {
		addr, _ := fakeRuntime(t, reply)
		if got, err := newClient(addr).ServerAddrs(context.Background(), "be_app"); err == nil {
			t.Errorf("reply %q: got %v, want error", reply, got)
		}
	}
}

func newClient(addr string) *Client {
	return &Client{Addr: addr, Timeout: time.Second}
}

func fixture(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("testdata/show_stat.csv")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestShowStatParsesServers(t *testing.T) {
	addr, commands := fakeRuntime(t, fixture(t))
	got, err := newClient(addr).ShowStat(context.Background(), "be_app")
	if err != nil {
		t.Fatal(err)
	}
	want := []ServerStat{
		{Name: "app-01", Status: "UP", Scur: 7},
		{Name: "app-02", Status: "DOWN 1/2", Scur: 0},
		{Name: "app-03", Status: "MAINT", Scur: 2},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("server %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if !got[1].Down() {
		t.Error("app-02 Down() = false, want true")
	}
	if cmd := <-commands; cmd != "show stat\n" {
		t.Errorf("command = %q", cmd)
	}
}

// real_show_stat.csv is a reply captured from HAProxy 2.8 running the generated
// config, with the whole fleet up.
func TestShowStatParsesRealReply(t *testing.T) {
	data, err := os.ReadFile("testdata/real_show_stat.csv")
	if err != nil {
		t.Fatal(err)
	}
	addr, _ := fakeRuntime(t, string(data))
	got, err := newClient(addr).ShowStat(context.Background(), "be_app")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 20 {
		t.Fatalf("got %d servers, want 20", len(got))
	}
	for i, s := range got {
		if want := fmt.Sprintf("app-%02d", i+1); s.Name != want || s.Status != "UP" || s.Down() {
			t.Errorf("server %d = %+v, want %s and UP", i, s, want)
		}
	}
}

func TestShowStatSkipsAggregateRows(t *testing.T) {
	addr, _ := fakeRuntime(t, fixture(t))
	got, err := newClient(addr).ShowStat(context.Background(), "be_app")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range got {
		if s.Name == "FRONTEND" || s.Name == "BACKEND" {
			t.Errorf("aggregate row %q returned", s.Name)
		}
		if s.Name == "app-99" {
			t.Error("server from another backend returned")
		}
	}
}

func TestServerStatDown(t *testing.T) {
	for status, want := range map[string]bool{
		"UP": false, "UP 1/3": false, "MAINT": false, "DRAIN": false, "no check": false,
		"DOWN": true, "DOWN 1/2": true,
	} {
		if got := (ServerStat{Status: status}).Down(); got != want {
			t.Errorf("%q: Down() = %v, want %v", status, got, want)
		}
	}
}

func TestShowStatRejectsBadReplies(t *testing.T) {
	for name, reply := range map[string]string{
		"error text":     "Permission denied.\n",
		"empty":          "",
		"missing status": "# pxname,svname,scur,\nbe_app,app-01,1,\n",
		"bad scur":       "# pxname,svname,scur,status,\nbe_app,app-01,x,UP,\n",
		"short row":      "# pxname,svname,scur,status,\nbe_app,app-01,\n",
	} {
		addr, _ := fakeRuntime(t, reply)
		if _, err := newClient(addr).ShowStat(context.Background(), "be_app"); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestShowStatErrorCarriesReplyText(t *testing.T) {
	addr, _ := fakeRuntime(t, "Permission denied.\n")
	_, err := newClient(addr).ShowStat(context.Background(), "be_app")
	if err == nil || !strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("err = %v, want it to carry the reply", err)
	}
}

func TestSetStateSendsExactCommand(t *testing.T) {
	addr, commands := fakeRuntime(t, "\n")
	if err := newClient(addr).SetState(context.Background(), "be_app", "app-03", Drain); err != nil {
		t.Fatal(err)
	}
	if cmd := <-commands; cmd != "set server be_app/app-03 state drain\n" {
		t.Fatalf("command = %q", cmd)
	}
}

func TestSetStateValues(t *testing.T) {
	for state, want := range map[State]string{Ready: "ready", Drain: "drain", Maint: "maint"} {
		if string(state) != want {
			t.Errorf("state = %q, want %q", state, want)
		}
	}
}

func TestSetStateErrorOnReply(t *testing.T) {
	addr, _ := fakeRuntime(t, "No such server.\n")
	err := newClient(addr).SetState(context.Background(), "be_app", "app-03", Drain)
	if err == nil || !strings.Contains(err.Error(), "No such server") {
		t.Fatalf("err = %v, want it to carry the reply", err)
	}
}

func TestSetStateAcceptsEmptyReply(t *testing.T) {
	addr, _ := fakeRuntime(t, "")
	if err := newClient(addr).SetState(context.Background(), "be_app", "app-03", Ready); err != nil {
		t.Fatal(err)
	}
}

func TestConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	if _, err := newClient(addr).ShowStat(context.Background(), "be_app"); err == nil {
		t.Error("ShowStat: want error")
	}
	if err := newClient(addr).SetState(context.Background(), "be_app", "app-03", Drain); err == nil {
		t.Error("SetState: want error")
	}
}

// hangingServer accepts connections and never replies.
func hangingServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { conn.Close() })
		}
	}()
	return ln.Addr().String()
}

// A wedged HAProxy must not stall the daemon's one-second loop.
func TestTimeoutWhenServerHangs(t *testing.T) {
	c := &Client{Addr: hangingServer(t), Timeout: 100 * time.Millisecond}
	start := time.Now()
	if _, err := c.ShowStat(context.Background(), "be_app"); err == nil {
		t.Fatal("want error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("took %v, want about 100ms", elapsed)
	}
}

func TestContextCancelsHungRequest(t *testing.T) {
	c := &Client{Addr: hangingServer(t), Timeout: time.Minute}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := c.SetState(ctx, "be_app", "app-03", Drain); err == nil {
		t.Fatal("want error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("took %v, want about 100ms", elapsed)
	}
}

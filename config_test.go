package main

import (
	"bufio"
	"bytes"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCheckConfig(t *testing.T) {
	cases := []struct {
		name    string
		timeout int
		check   int
		event   bool
		conns   int
		want    string
	}{
		{"defaults", 2000, 250, false, 10000, ""},
		{"timeout zero", 0, 250, false, 10000, "timeoutms must be greater than 0"},
		{"timeout negative", -1, 250, false, 10000, "timeoutms must be greater than 0"},
		{"check negative", 2000, -1, false, 10000, "checkms must not be negative"},
		{"check negative with events", 2000, -1, true, 10000, "checkms must not be negative"},
		{"check zero", 2000, 0, false, 10000, "needs eventlistener"},
		{"check zero with events", 2000, 0, true, 10000, ""},
		{"check with events", 2000, 250, true, 10000, ""},
		{"conns negative", 2000, 250, false, -1, "maxconns must not be negative"},
		{"conns unlimited", 2000, 250, false, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reset(t)
			*timeout, *check = c.timeout, c.check
			*eventListener, *maxConns = c.event, c.conns
			err := checkConfig()
			if c.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(),
				c.want) {
				t.Fatalf("error %v, want %q", err, c.want)
			}
		})
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func captureLog(t *testing.T) *syncBuf {
	t.Helper()
	b := &syncBuf{}
	log.SetOutput(b)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return b
}

func startServe(t *testing.T) *net.TCPListener {
	t.Helper()
	l, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		serve(l)
		close(done)
	}()
	t.Cleanup(func() {
		l.Close()
		<-done
		deadline := time.Now().Add(3 * time.Second)
		for activeConns.Load() != 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	})
	return l
}

func dialProxy(t *testing.T, l *net.TCPListener) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(3 * time.Second))
	return c
}

func ping(c net.Conn) (string, error) {
	if _, err := c.Write([]byte("PING\r\n")); err != nil {
		return "", err
	}
	return bufio.NewReader(c).ReadString('\n')
}

func setTestMaster(t *testing.T, addr string) {
	t.Helper()
	setMaster(resolve(t, addr))
}

func waitConns(t *testing.T, n int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for activeConns.Load() != n {
		if time.Now().After(deadline) {
			t.Fatalf("%d active connections, want %d", activeConns.Load(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestConnectionLimit(t *testing.T) {
	r1 := newRedis(t)
	reset(t)
	*maxConns = 2
	logs := captureLog(t)
	setTestMaster(t, r1.addr())
	l := startServe(t)
	c1, c2 := dialProxy(t, l), dialProxy(t, l)
	for _, c := range []net.Conn{c1, c2} {
		if got, err := ping(c); got != "+PONG\r\n" {
			t.Fatalf("ping %q, %v", got, err)
		}
	}
	c3 := dialProxy(t, l)
	if b, err := io.ReadAll(c3); err != nil || len(b) != 0 {
		t.Fatalf("over the limit: got %q, %v, want close", b, err)
	}
	c4 := dialProxy(t, l)
	io.ReadAll(c4)
	if n := strings.Count(logs.String(), "Connection limit 2 reached"); n != 1 {
		t.Fatalf("limit logged %d times, want 1", n)
	}
	c1.Close()
	waitConns(t, 1)
	c5 := dialProxy(t, l)
	if got, err := ping(c5); got != "+PONG\r\n" {
		t.Fatalf("after release: ping %q, %v", got, err)
	}
}

func TestConnectionLimitOff(t *testing.T) {
	r1 := newRedis(t)
	reset(t)
	*maxConns = 0
	setTestMaster(t, r1.addr())
	l := startServe(t)
	var cs []net.Conn
	for i := 0; i < 50; i++ {
		cs = append(cs, dialProxy(t, l))
	}
	for i, c := range cs {
		if got, err := ping(c); got != "+PONG\r\n" {
			t.Fatalf("conn %d: ping %q, %v", i, got, err)
		}
	}
}

func TestAcquireConnLogEvery(t *testing.T) {
	reset(t)
	*maxConns = 1
	logs := captureLog(t)
	activeConns.Store(1)
	for i := 0; i < 5; i++ {
		if acquireConn() {
			t.Fatal("acquired over the limit")
		}
	}
	if activeConns.Load() != 1 {
		t.Fatalf("active %d after refusals, want 1", activeConns.Load())
	}
	if n := strings.Count(logs.String(), "reached"); n != 1 {
		t.Fatalf("logged %d times in a burst, want 1", n)
	}
	limitLogged.Store(time.Now().Unix() - limitLogEvery)
	acquireConn()
	if n := strings.Count(logs.String(), "reached"); n != 2 {
		t.Fatalf("logged %d times after %ds, want 2", n, limitLogEvery)
	}
	activeConns.Store(0)
	if !acquireConn() || activeConns.Load() != 1 {
		t.Fatal("could not acquire under the limit")
	}
}

func TestServeReturnsWhenClosed(t *testing.T) {
	reset(t)
	l, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		serve(l)
		close(done)
	}()
	l.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serve kept running after Close")
	}
}

func TestDebugCmd(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"AUTH", "pw"}, "AUTH <redacted>"},
		{[]string{"AUTH", "user", "pw"}, "AUTH <redacted>"},
		{[]string{"auth", "pw"}, "auth <redacted>"},
		{[]string{"AUTH"}, "AUTH"},
		{[]string{"SENTINEL", "get-master-addr-by-name", "m"}, "SENTINEL get-master-addr-by-name m"},
		{[]string{"PING"}, "PING"},
	}
	for _, c := range cases {
		if got := debugCmd(c.args); got != c.want {
			t.Fatalf("debugCmd(%q) = %q, want %q", c.args, got, c.want)
		}
	}
}

func TestDebugOutputHidesPassword(t *testing.T) {
	r1 := newRedis(t)
	s1 := newSentinel(t, r1.addr())
	reset(t, s1)
	*debug = true
	*username, *password = "user1", "s3cretpass"
	logs := captureLog(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		out <- string(b)
	}()
	old := os.Stdout
	os.Stdout = w
	err = getMasterAddr()
	os.Stdout = old
	w.Close()
	stdout := <-out
	if err != nil {
		t.Fatal(err)
	}
	all := stdout + logs.String()
	if !strings.Contains(stdout, "AUTH <redacted>") {
		t.Fatalf("no redacted AUTH in debug output:\n%s", stdout)
	}
	if strings.Contains(all, "s3cretpass") {
		t.Fatalf("password in debug output:\n%s", all)
	}
}

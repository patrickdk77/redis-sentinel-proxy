package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("RSP_TEST_MAIN") == "1" {
		os.Args = os.Args[:1]
		main()
		return
	}
	os.Exit(m.Run())
}

type child struct {
	cmd    *exec.Cmd
	stdout bytes.Buffer
	stderr bytes.Buffer
	listen string
}

func startChild(t *testing.T, env ...string) *child {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	listen, release := reserveAddr(t)
	c := &child{listen: listen}
	c.cmd = exec.CommandContext(ctx, os.Args[0])
	c.cmd.Env = append([]string{"RSP_TEST_MAIN=1", "LISTEN=" + c.listen}, env...)
	c.cmd.Stdout = &c.stdout
	c.cmd.Stderr = &c.stderr
	release()
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.cmd.Process.Kill()
		c.cmd.Wait()
		cancel()
	})
	return c
}

func runChild(t *testing.T, env ...string) (int, string) {
	t.Helper()
	c := startChild(t, env...)
	c.cmd.Wait()
	return c.cmd.ProcessState.ExitCode(), c.stdout.String() + c.stderr.String()
}

func (c *child) reply(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	got := ""
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", c.listen, time.Second)
		if err == nil {
			conn.SetDeadline(time.Now().Add(time.Second))
			got, _ = ping(conn)
			conn.Close()
			if got == want {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("proxy replied %q, want %q\n%s%s", got, want, c.stdout.String(), c.stderr.String())
}

func (c *child) stop(t *testing.T) {
	t.Helper()
	if err := c.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := c.cmd.Wait(); err != nil {
		t.Fatalf("exit after SIGTERM: %v", err)
	}
	if !strings.Contains(c.stdout.String(), "SigTerm issued") {
		t.Fatalf("stdout %q", c.stdout.String())
	}
}

func TestProcessRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		want string
	}{
		{"checkms negative", []string{"CHECKMS=-1"}, "checkms must not be negative"},
		{"checkms zero", []string{"CHECKMS=0"}, "needs eventlistener"},
		{"timeoutms zero", []string{"TIMEOUTMS=0"}, "timeoutms must be greater than 0"},
		{"timeoutms negative", []string{"TIMEOUTMS=-5"}, "timeoutms must be greater than 0"},
		{"maxconns negative", []string{"MAXCONNS=-1"}, "maxconns must not be negative"},
		{"tls file without tls", []string{"SENTINELTLSCA=/nonexistent"}, "given without sentineltls"},
		{"tls ca missing", []string{"SENTINELTLS=true", "SENTINELTLSCA=/nonexistent"}, "no such file"},
		{"majority on", []string{"MAJORITY=on"}, "invalid boolean value"},
		{"bad listen", []string{"LISTEN=nonsense"}, "Failed to resolve local address"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, out := runChild(t, c.env...)
			if code == 0 || !strings.Contains(out, c.want) {
				t.Fatalf("exit %d, output %q, want failure with %q", code, out, c.want)
			}
		})
	}
}

func TestProcessPolling(t *testing.T) {
	r1 := newRedisReply(t, "+R1\r\n")
	r2 := newRedisReply(t, "+R2\r\n")
	s := newSentinel(t, r1.addr())
	c := startChild(t, "SENTINEL="+s.addr(), "CHECKMS=50")
	c.reply(t, "+R1\r\n")
	s.set(func(f *sstate) { f.master = r2.addr() })
	c.reply(t, "+R2\r\n")
	c.stop(t)
}

func TestProcessEventsOnly(t *testing.T) {
	r1 := newRedisReply(t, "+R1\r\n")
	r2 := newRedisReply(t, "+R2\r\n")
	s := newSentinel(t, r1.addr())
	c := startChild(t, "SENTINEL="+s.addr(), "EVENTLISTENER=true", "CHECKMS=0")
	c.reply(t, "+R1\r\n")
	waitSubscribed(t, s)
	s.set(func(f *sstate) { f.master = r2.addr() })
	s.publish(switchPayload("mymaster", r1.addr(), r2.addr()))
	c.reply(t, "+R2\r\n")
	c.stop(t)
}

func TestProcessMajority(t *testing.T) {
	r1 := newRedisReply(t, "+R1\r\n")
	r2 := newRedisReply(t, "+R2\r\n")
	s1 := withID(newSentinel(t, r2.addr()), "id1")
	s2 := withID(newSentinel(t, r1.addr()), "id2")
	s3 := withID(newSentinel(t, r1.addr()), "id3")
	c := startChild(t, "MAJORITY=true", "CHECKMS=50", "SENTINEL="+s1.addr()+","+s2.addr()+","+s3.addr())
	c.reply(t, "+R1\r\n")
	c.stop(t)
}

func TestProcessSentinelTLS(t *testing.T) {
	p := newPKI(t)
	_, _, srv := p.issue(t, "srv", nil, localIP, false)
	r1 := newRedisReply(t, "+R1\r\n")
	s := newTLSSentinel(t, r1.addr(), &tls.Config{Certificates: []tls.Certificate{srv}})
	c := startChild(t, "SENTINEL="+s.addr(), "CHECKMS=50", "SENTINELTLS=true", "SENTINELTLSCA="+p.caFile)
	c.reply(t, "+R1\r\n")
	c.stop(t)
}

func TestProcessConnectionLimit(t *testing.T) {
	r1 := newRedisReply(t, "+R1\r\n")
	s := newSentinel(t, r1.addr())
	c := startChild(t, "SENTINEL="+s.addr(), "CHECKMS=50", "MAXCONNS=1")
	c.reply(t, "+R1\r\n")
	var held net.Conn
	deadline := time.Now().Add(5 * time.Second)
	for held == nil && time.Now().Before(deadline) {
		conn, err := net.Dial("tcp", c.listen)
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		if got, _ := ping(conn); got == "+R1\r\n" {
			held = conn
			break
		}
		conn.Close()
		time.Sleep(20 * time.Millisecond)
	}
	if held == nil {
		t.Fatal("never got the one allowed connection")
	}
	defer held.Close()
	extra, err := net.Dial("tcp", c.listen)
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Close()
	extra.SetDeadline(time.Now().Add(3 * time.Second))
	got, err := ping(extra)
	var ne net.Error
	if got != "" || err == nil || (errors.As(err, &ne) && ne.Timeout()) {
		t.Fatalf("second connection got %q, %v, want it closed", got, err)
	}
	held.Close()
	c.reply(t, "+R1\r\n")
	c.stop(t)
}

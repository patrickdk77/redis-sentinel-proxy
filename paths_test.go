package main

import (
	"net"
	"strings"
	"testing"
	"time"
)

func waitLog(t *testing.T, logs *syncBuf, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(logs.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("log lacks %q:\n%s", want, logs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestUnresolvableMaster(t *testing.T) {
	s1 := newSentinel(t, "")
	s1.set(func(f *sstate) {
		f.raw = "*2\r\n" + bulk("nonexistent.invalid") + bulk("6379")
	})
	reset(t, s1)
	logs := captureLog(t)
	if err := getMasterAddr(); err == nil {
		t.Fatal("unresolvable master accepted")
	}
	waitLog(t, logs, "Unable to resolve new master")
	if cur() != "<nil>" {
		t.Fatalf("master %s", cur())
	}
}

func TestAuthConnectionDropped(t *testing.T) {
	r1 := newRedis(t)
	s1 := newSentinel(t, r1.addr())
	s1.set(func(f *sstate) { f.dropAuth = true })
	reset(t, s1)
	*password = "pw"
	if _, err := dialSentinel(s1.addr()); err == nil {
		t.Fatal("dial succeeded when AUTH got no reply")
	}
	pollFails(t, "No Sentinels returned")
}

func TestDoOnClosedConnection(t *testing.T) {
	r1 := newRedis(t)
	s1 := newSentinel(t, r1.addr())
	reset(t, s1)
	s, err := dialSentinel(s1.addr())
	if err != nil {
		t.Fatal(err)
	}
	s.conn.Close()
	if _, err := s.do("PING"); err == nil {
		t.Fatal("do on a closed connection succeeded")
	}
}

func TestPeersTimeoutKeepsVote(t *testing.T) {
	r1 := newRedis(t)
	s1 := withID(newSentinel(t, r1.addr()), "id1")
	s1.set(func(f *sstate) { f.hangPeer = true })
	reset(t, s1)
	*majority = true
	*debug = true
	timeoutms = 200
	logs := captureLog(t)
	a := askSentinel(s1.addr(), true, true)
	if a.err != nil || a.peersOK || a.master != r1.addr() {
		t.Fatalf("answer %+v", a)
	}
	waitLog(t, logs, "Error reading from Sentinel")
	pollOK(t)
	if cur() != r1.addr() {
		t.Fatalf("master %s, want %s", cur(), r1.addr())
	}
}

func TestPeerSameAddressTwoIDs(t *testing.T) {
	got := peerAddrs([]answer{{addr: "s:1", id: "1", peersOK: true, peers: []peerInfo{
		{addr: "a:1", id: "y"},
		{addr: "a:1", id: "x"},
	}}})
	if len(got) != 1 || got[0] != "a:1" {
		t.Fatalf("got %v, want [a:1]", got)
	}
}

func TestDebugLogging(t *testing.T) {
	r1 := newRedis(t)
	s1 := newSentinel(t, r1.addr())
	reset(t, s1)
	*majority = true
	*debug = true
	logs := captureLog(t)
	*sentinelAddr = s1.addr() + "," + deadAddr(t)
	pollOK(t)
	waitLog(t, logs, "Connecting to Sentinel at")
	waitLog(t, logs, "Unable to connect to Sentinel")
	c := startProxy(t, resolve(t, r1.addr()), make(chan struct{}))
	if got, err := ping(c); got != "+PONG\r\n" {
		t.Fatalf("ping %q, %v", got, err)
	}
	c.Close()
	waitLog(t, logs, "New connection")
	waitLog(t, logs, "Shutting down stream")
	waitLog(t, logs, "Closing connection")
}

func TestProxyClientReset(t *testing.T) {
	r1 := newRedis(t)
	reset(t)
	l, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := l.AcceptTCP()
		if err == nil {
			proxy(c, resolve(t, r1.addr()), make(chan struct{}))
		}
	}()
	c, err := net.DialTCP("tcp", nil, l.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if got, err := ping(c); got != "+PONG\r\n" {
		t.Fatalf("ping %q, %v", got, err)
	}
	c.SetLinger(0)
	c.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("proxy still running after client reset")
	}
}

func TestWatchSentinelUnreachable(t *testing.T) {
	reset(t)
	if err := watchSentinel(deadAddr(t)); err == nil {
		t.Fatal("watchSentinel on a dead address returned nil")
	}
}

func TestWatchSyncErrorAndBadEvent(t *testing.T) {
	r1 := newRedis(t)
	s1 := newSentinel(t, "")
	reset(t, s1)
	*debug = true
	logs := captureLog(t)
	startWatch(t, s1)
	waitSubscribed(t, s1)
	waitLog(t, logs, "Error polling for new master")
	s1.publish("mymaster 127.0.0.1 1 nonexistent.invalid 6379")
	waitLog(t, logs, "Unable to resolve new master")
	s1.set(func(f *sstate) { f.master = r1.addr() })
	s1.publish(switchPayload("mymaster", r1.addr(), r1.addr()))
	waitMaster(t, r1.addr(), 2*time.Second)
}

func TestEventWindowExpires(t *testing.T) {
	r1, r2 := newRedis(t), newRedis(t)
	s1 := newSentinel(t, r1.addr())
	reset(t, s1)
	*majority = true
	old := eventRetryWindow
	eventRetryWindow = 300 * time.Millisecond
	t.Cleanup(func() { eventRetryWindow = old })
	logs := captureLog(t)
	startWatch(t, s1)
	waitSubscribed(t, s1)
	waitMaster(t, r1.addr(), 2*time.Second)
	s1.publish(switchPayload("mymaster", r1.addr(), r2.addr()))
	waitLog(t, logs, "Sentinels did not agree on "+r2.addr())
	if cur() != r1.addr() {
		t.Fatalf("master %s, want %s", cur(), r1.addr())
	}
}

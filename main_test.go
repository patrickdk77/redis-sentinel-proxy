package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeRedis struct {
	l net.Listener
}

func newRedis(t *testing.T) *fakeRedis {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRedis{l: l}
	t.Cleanup(func() { l.Close() })
	go f.serve()
	return f
}

func (f *fakeRedis) serve() {
	for {
		c, err := f.l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			r := bufio.NewReader(c)
			for {
				_, err := r.ReadString('\n')
				if err != nil {
					return
				}
				c.Write([]byte("+PONG\r\n"))
			}
		}()
	}
}

func (f *fakeRedis) addr() string { return f.l.Addr().String() }

type peer struct{ addr, flags string }

type fakeSentinel struct {
	l      net.Listener
	closed chan struct{}

	mu sync.Mutex
	st sstate
}

type sstate struct {
	master  string
	raw     string
	peers   []peer
	hang    bool
	split   bool
	authErr bool
	silent  bool
	dropSub bool
	auths   [][]string
	conns   []net.Conn
	subs    []net.Conn
}

func newSentinel(t *testing.T, master string) *fakeSentinel {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSentinel{l: l, st: sstate{master: master},
		closed: make(chan struct{})}
	t.Cleanup(f.close)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.st.conns = append(f.st.conns, c)
			f.mu.Unlock()
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeSentinel) addr() string { return f.l.Addr().String() }

func (f *fakeSentinel) set(fn func(f *sstate)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.st)
}

func (f *fakeSentinel) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case <-f.closed:
		return
	default:
	}
	close(f.closed)
	f.l.Close()
	for _, c := range f.st.conns {
		c.Close()
	}
}

func (f *fakeSentinel) write(c net.Conn, s string) {
	f.mu.Lock()
	split := f.st.split
	f.mu.Unlock()
	if !split {
		c.Write([]byte(s))
		return
	}
	for i := 0; i < len(s); i++ {
		c.Write([]byte{s[i]})
		time.Sleep(time.Millisecond)
	}
}

func bulk(s string) string {
	return fmt.Sprintf("$%d\r\n%s\r\n", len(s), s)
}

func (f *fakeSentinel) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	for {
		cmd, err := readReply(r)
		if err != nil {
			return
		}
		arr, _ := cmd.([]interface{})
		var args []string
		for _, a := range arr {
			s, _ := a.(string)
			args = append(args, strings.ToLower(s))
		}
		if len(args) == 0 {
			return
		}
		f.mu.Lock()
		st := f.st
		f.mu.Unlock()
		switch {
		case args[0] == "auth":
			f.set(func(f *sstate) {
				f.auths = append(f.auths, args)
			})
			if st.authErr {
				f.write(c, "-WRONGPASS bad\r\n")
			} else {
				f.write(c, "+OK\r\n")
			}
		case len(args) == 3 &&
			args[1] == "get-master-addr-by-name":
			if st.hang {
				<-f.closed
				return
			}
			switch {
			case st.raw != "":
				f.write(c, st.raw)
			case st.master == "":
				f.write(c, "*-1\r\n")
			default:
				h, p, _ := net.SplitHostPort(
					st.master)
				f.write(c, "*2\r\n"+bulk(h)+bulk(p))
			}
		case len(args) == 3 && args[1] == "sentinels":
			out := fmt.Sprintf("*%d\r\n", len(st.peers))
			for _, p := range st.peers {
				h, port, _ := net.SplitHostPort(
					p.addr)
				out += "*8\r\n" + bulk("name") +
					bulk(p.addr) + bulk("ip") +
					bulk(h) + bulk("port") +
					bulk(port) + bulk("flags") +
					bulk("sentinel"+p.flags)
			}
			f.write(c, out)
		case args[0] == "subscribe":
			f.write(c, "*3\r\n"+bulk("subscribe")+
				bulk("+switch-master")+":1\r\n")
			if st.dropSub {
				return
			}
			f.set(func(f *sstate) {
				f.subs = append(f.subs, c)
			})
		case args[0] == "ping":
			if !st.silent {
				f.write(c, "*2\r\n"+bulk("pong")+
					bulk(""))
			}
		default:
			f.write(c, "-ERR unknown command\r\n")
		}
	}
}

func (f *fakeSentinel) publish(payload string) {
	f.mu.Lock()
	subs := append([]net.Conn(nil), f.st.subs...)
	f.mu.Unlock()
	for _, c := range subs {
		c.Write([]byte("*3\r\n" + bulk("message") +
			bulk("+switch-master") + bulk(payload)))
	}
}

func switchPayload(name, from, to string) string {
	fh, fp, _ := net.SplitHostPort(from)
	th, tp, _ := net.SplitHostPort(to)
	return strings.Join([]string{name, fh, fp, th, tp}, " ")
}

var testMajority bool

func modes(t *testing.T, fn func(t *testing.T)) {
	for _, m := range []bool{false, true} {
		name := fmt.Sprintf("majority=%v", m)
		t.Run(name, func(t *testing.T) {
			testMajority = m
			defer func() { testMajority = false }()
			fn(t)
		})
	}
}

func reset(t *testing.T, sentinels ...*fakeSentinel) {
	*majority = testMajority
	masterMu.Lock()
	masterAddr = nil
	masterStop = make(chan struct{})
	masterMu.Unlock()
	timeoutms = 500
	*password = ""
	*username = ""
	*masterName = "mymaster"
	var addrs []string
	for _, s := range sentinels {
		addrs = append(addrs, s.addr())
	}
	*sentinelAddr = strings.Join(addrs, ",")
}

func cur() string {
	addr, _ := currentMaster()
	return addr.String()
}

func waitMaster(t *testing.T, want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cur() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("master is %s, want %s after %v", cur(), want,
		within)
}

func deadAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func pollOK(t *testing.T) {
	t.Helper()
	if err := getMasterAddr(); err != nil {
		t.Fatalf("getMasterAddr: %v", err)
	}
}

func pollFails(t *testing.T, want string) {
	t.Helper()
	err := getMasterAddr()
	if err == nil {
		t.Fatalf("getMasterAddr succeeded, want error %q",
			want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("getMasterAddr error %q, want %q", err, want)
	}
}

func TestMajorityWins(t *testing.T) {
	r1, r2 := newRedis(t), newRedis(t)
	s1 := newSentinel(t, r1.addr())
	s2 := newSentinel(t, r1.addr())
	s3 := newSentinel(t, r2.addr())
	orders := [][]*fakeSentinel{
		{s1, s2, s3}, {s3, s1, s2}, {s2, s3, s1},
	}
	reset(t)
	*majority = true
	for i := 0; i < 30; i++ {
		reset := orders[i%3]
		var addrs []string
		for _, s := range reset {
			addrs = append(addrs, s.addr())
		}
		*sentinelAddr = strings.Join(addrs, ",")
		pollOK(t)
		if cur() != r1.addr() {
			t.Fatalf("poll %d: master %s, want %s",
				i, cur(), r1.addr())
		}
	}
}

func TestNoFlipFlopBehindOneAddress(t *testing.T) {
	r1, r2 := newRedis(t), newRedis(t)
	s1 := newSentinel(t, r1.addr())
	s2 := newSentinel(t, r1.addr())
	s3 := newSentinel(t, r2.addr())
	all := []*fakeSentinel{s1, s2, s3}
	for _, s := range all {
		var ps []peer
		for _, o := range all {
			if o != s {
				ps = append(ps, peer{addr: o.addr()})
			}
		}
		s.set(func(f *sstate) { f.peers = ps })
	}
	reset(t, s1)
	*majority = true
	pollOK(t)
	_, stop := currentMaster()
	for i := 0; i < 30; i++ {
		*sentinelAddr = all[i%3].addr()
		pollOK(t)
		if cur() != r1.addr() {
			t.Fatalf("poll %d via %s: master %s, want %s",
				i, *sentinelAddr, cur(), r1.addr())
		}
	}
	select {
	case <-stop:
		t.Fatal("client connections were dropped")
	default:
	}
}

func TestSwitchesWhenMajorityMoves(t *testing.T) {
	r1, r2 := newRedis(t), newRedis(t)
	s1 := newSentinel(t, r1.addr())
	s2 := newSentinel(t, r1.addr())
	s3 := newSentinel(t, r1.addr())
	reset(t, s1, s2, s3)
	*majority = true
	pollOK(t)
	_, stop := currentMaster()
	for _, s := range []*fakeSentinel{s1, s2} {
		s.set(func(f *sstate) { f.master = r2.addr() })
	}
	pollOK(t)
	if cur() != r2.addr() {
		t.Fatalf("master %s, want %s", cur(), r2.addr())
	}
	select {
	case <-stop:
	default:
		t.Fatal("stop channel not closed on master change")
	}
}

func TestTieKeepsCurrent(t *testing.T) {
	r1, r2 := newRedis(t), newRedis(t)
	s1 := newSentinel(t, r1.addr())
	s2 := newSentinel(t, r2.addr())
	reset(t, s1, s2)
	*majority = true
	pollFails(t, "No master has a majority of 2 sentinels")
	if cur() != "<nil>" {
		t.Fatalf("master %s chosen without a majority", cur())
	}
	s2.set(func(f *sstate) { f.master = r1.addr() })
	pollOK(t)
	s2.set(func(f *sstate) { f.master = r2.addr() })
	pollFails(t, "No master has a majority")
	if cur() != r1.addr() {
		t.Fatalf("master %s, want %s kept on a tie",
			cur(), r1.addr())
	}
}

func TestDiscoveredPeersOutvoteStaleSeed(t *testing.T) {
	r1, r2 := newRedis(t), newRedis(t)
	s1 := newSentinel(t, r1.addr())
	s2 := newSentinel(t, r1.addr())
	seed := newSentinel(t, r2.addr())
	seed.set(func(f *sstate) {
		f.peers = []peer{{addr: s1.addr()}, {addr: s2.addr()}}
	})
	reset(t, seed)
	*majority = true
	pollOK(t)
	if cur() != r1.addr() {
		t.Fatalf("master %s, want %s", cur(), r1.addr())
	}
}

func TestPeerAlsoListedIsAskedOnce(t *testing.T) {
	r1, r2 := newRedis(t), newRedis(t)
	s1 := newSentinel(t, r1.addr())
	s2 := newSentinel(t, r2.addr())
	s3 := newSentinel(t, r2.addr())
	s1.set(func(f *sstate) {
		f.peers = []peer{{addr: s1.addr()}, {addr: s1.addr()}}
	})
	reset(t, s1, s2, s3)
	*majority = true
	pollOK(t)
	if cur() != r2.addr() {
		t.Fatalf("master %s, want %s", cur(), r2.addr())
	}
}

func TestDownPeersSkipped(t *testing.T) {
	r1 := newRedis(t)
	s1 := newSentinel(t, r1.addr())
	sdown := newSentinel(t, r1.addr())
	disc := newSentinel(t, r1.addr())
	for _, s := range []*fakeSentinel{sdown, disc} {
		s.set(func(f *sstate) { f.hang = true })
	}
	s1.set(func(f *sstate) {
		f.peers = []peer{
			{addr: sdown.addr(), flags: ",s_down"},
			{addr: disc.addr(), flags: ",disconnected"},
		}
	})
	reset(t, s1)
	*majority = true
	timeoutms = 2000
	start := time.Now()
	pollOK(t)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("poll took %v, down peers were queried", d)
	}
}

func TestUnreachablePeerHasNoVote(t *testing.T) {
	r1 := newRedis(t)
	s1 := newSentinel(t, r1.addr())
	s1.set(func(f *sstate) {
		f.peers = []peer{{addr: deadAddr(t)}}
	})
	reset(t, s1)
	*majority = true
	pollOK(t)
	if cur() != r1.addr() {
		t.Fatalf("master %s, want %s", cur(), r1.addr())
	}
}

func TestHungSentinelTimesOut(t *testing.T) {
	modes(t, testHungSentinelTimesOut)
}

func testHungSentinelTimesOut(t *testing.T) {
	r1 := newRedis(t)
	hung := newSentinel(t, r1.addr())
	hung.set(func(f *sstate) { f.hang = true })
	reset(t, hung)
	start := time.Now()
	pollFails(t, "No Sentinels returned a valid master")
	if d := time.Since(start); d > 2*timeoutms*time.Millisecond {
		t.Fatalf("poll took %v with timeoutms=%d", d,
			timeoutms)
	}
	s2 := newSentinel(t, r1.addr())
	*sentinelAddr = hung.addr() + "," + s2.addr()
	pollOK(t)
	if cur() != r1.addr() {
		t.Fatalf("master %s, want %s", cur(), r1.addr())
	}
}

func TestFragmentedReply(t *testing.T) {
	modes(t, testFragmentedReply)
}

func testFragmentedReply(t *testing.T) {
	r1 := newRedis(t)
	s1 := newSentinel(t, r1.addr())
	s1.set(func(f *sstate) { f.split = true })
	reset(t, s1)
	pollOK(t)
	if cur() != r1.addr() {
		t.Fatalf("master %s, want %s", cur(), r1.addr())
	}
}

func TestBadReplies(t *testing.T) {
	modes(t, testBadReplies)
}

func testBadReplies(t *testing.T) {
	r1 := newRedis(t)
	const none = "No Sentinels returned"
	cases := []struct {
		name, raw, want string
	}{
		{"nil master", "", "No Sentinels returned"},
		{"error reply", "-ERR no such master\r\n",
			none},
		{"garbage", "hello\r\n", "No Sentinels returned"},
		{"huge bulk", "$99999999999\r\n", none},
		{"huge array", "*99999999\r\n", none},
		{"short array", "*1\r\n$3\r\nabc\r\n",
			none},
		{"non-string items", "*2\r\n*0\r\n*0\r\n",
			none},
		{"truncated", "*2\r\n$9\r\n127.0", none},
		{"bad port", "*2\r\n" + bulk("127.0.0.1") + bulk("x"),
			none},
		{"port range", "*2\r\n" + bulk("127.0.0.1") +
			bulk("70000"), "No Sentinels returned"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newSentinel(t, "")
			s.set(func(f *sstate) { f.raw = c.raw })
			good := newSentinel(t, r1.addr())
			reset(t, s)
			timeoutms = 200
			err := getMasterAddr()
			if err == nil ||
				!strings.Contains(err.Error(),
					c.want) {
				t.Fatalf("error %v, want %q", err,
					c.want)
			}
			if cur() != "<nil>" {
				t.Fatalf("master %s set from bad "+
					"reply", cur())
			}
			*sentinelAddr = s.addr() + "," + good.addr()
			pollOK(t)
			if cur() != r1.addr() {
				t.Fatalf("master %s, want %s", cur(),
					r1.addr())
			}
		})
	}
}

func TestAuth(t *testing.T) {
	modes(t, testAuth)
}

func testAuth(t *testing.T) {
	r1 := newRedis(t)
	s1 := newSentinel(t, r1.addr())
	reset(t, s1)
	pollOK(t)
	s1.set(func(f *sstate) {
		if len(f.auths) != 0 {
			t.Errorf("AUTH sent without a password: %v",
				f.auths)
		}
	})
	*password = "p w"
	pollOK(t)
	*username = "u"
	pollOK(t)
	s1.set(func(f *sstate) {
		want := "[[auth p w] [auth u p w]]"
		if got := fmt.Sprint(f.auths); got != want {
			t.Errorf("auths %s, want %s", got, want)
		}
	})
}

func TestAuthErrorIgnored(t *testing.T) {
	modes(t, testAuthErrorIgnored)
}

func testAuthErrorIgnored(t *testing.T) {
	r1 := newRedis(t)
	s1 := newSentinel(t, r1.addr())
	s1.set(func(f *sstate) { f.authErr = true })
	reset(t, s1)
	*password = "secret"
	pollOK(t)
	if cur() != r1.addr() {
		t.Fatalf("master %s, want %s", cur(), r1.addr())
	}
}

func TestUnreachableMasterRejected(t *testing.T) {
	modes(t, testUnreachableMasterRejected)
}

func testUnreachableMasterRejected(t *testing.T) {
	r1 := newRedis(t)
	dead := deadAddr(t)
	s1 := newSentinel(t, dead)
	s2 := newSentinel(t, dead)
	reset(t, s1, s2)
	if err := getMasterAddr(); err == nil {
		t.Fatal("unreachable master accepted")
	}
	if cur() != "<nil>" {
		t.Fatalf("master %s, want none", cur())
	}
	for _, s := range []*fakeSentinel{s1, s2} {
		s.set(func(f *sstate) { f.master = r1.addr() })
	}
	pollOK(t)
	for _, s := range []*fakeSentinel{s1, s2} {
		s.set(func(f *sstate) { f.master = dead })
	}
	if err := getMasterAddr(); err == nil {
		t.Fatal("unreachable master accepted")
	}
	if cur() != r1.addr() {
		t.Fatalf("master %s, want %s kept", cur(), r1.addr())
	}
}

func TestSentinelLookupFailure(t *testing.T) {
	modes(t, testSentinelLookupFailure)
}

func testSentinelLookupFailure(t *testing.T) {
	r1 := newRedis(t)
	s1 := newSentinel(t, r1.addr())
	reset(t)
	*sentinelAddr = "does-not-exist.invalid:26379"
	pollFails(t, "No Sentinels returned")
	*sentinelAddr = "no-port-here"
	pollFails(t, "No Sentinels returned")
	*sentinelAddr = "does-not-exist.invalid:26379," + s1.addr()
	pollOK(t)
	if cur() != r1.addr() {
		t.Fatalf("master %s, want %s", cur(), r1.addr())
	}
}

func TestConcurrentMasterChanges(t *testing.T) {
	modes(t, testConcurrentMasterChanges)
}

func testConcurrentMasterChanges(t *testing.T) {
	r1, r2 := newRedis(t), newRedis(t)
	reset(t)
	h1, p1, _ := net.SplitHostPort(r1.addr())
	h2, p2, _ := net.SplitHostPort(r2.addr())
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				currentMaster()
			}
		}
	}()
	for i := 0; i < 500; i++ {
		h, p := h1, p1
		if i%2 == 1 {
			h, p = h2, p2
		}
		var wg sync.WaitGroup
		for j := 0; j < 2; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				setNewMaster(h, p, "test")
			}()
		}
		wg.Wait()
	}
	close(done)
}

func startWatch(t *testing.T, s *fakeSentinel) chan error {
	t.Helper()
	ch := make(chan error, 1)
	go func() { ch <- watchSentinel(s.addr()) }()
	t.Cleanup(func() {
		s.close()
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Error("watchSentinel did not return")
		}
	})
	return ch
}

func waitSubscribed(t *testing.T, s *fakeSentinel) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		n := 0
		s.set(func(f *sstate) { n = len(f.subs) })
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("never subscribed")
}

func TestEventTriggersMajorityVote(t *testing.T) {
	r1, r2 := newRedis(t), newRedis(t)
	s1 := newSentinel(t, r1.addr())
	s2 := newSentinel(t, r1.addr())
	s3 := newSentinel(t, r1.addr())
	reset(t, s1, s2, s3)
	*majority = true
	startWatch(t, s1)
	waitSubscribed(t, s1)
	waitMaster(t, r1.addr(), 2*time.Second)

	s1.set(func(f *sstate) { f.master = r2.addr() })
	s1.publish(switchPayload("mymaster", r1.addr(), r2.addr()))
	time.Sleep(time.Second)
	if cur() != r1.addr() {
		t.Fatalf("switched to %s on one sentinel's event",
			cur())
	}
	s2.set(func(f *sstate) { f.master = r2.addr() })
	waitMaster(t, r2.addr(), 2*time.Second)
}

func TestEventOnSubscribeUsesVote(t *testing.T) {
	r1, r2 := newRedis(t), newRedis(t)
	stale := newSentinel(t, r2.addr())
	s2 := newSentinel(t, r1.addr())
	s3 := newSentinel(t, r1.addr())
	stale.set(func(f *sstate) {
		f.peers = []peer{{addr: s2.addr()}, {addr: s3.addr()}}
	})
	reset(t, stale)
	*majority = true
	startWatch(t, stale)
	waitSubscribed(t, stale)
	waitMaster(t, r1.addr(), 2*time.Second)
}

func TestEventRetriesUnreachableMaster(t *testing.T) {
	modes(t, testEventRetriesUnreachableMaster)
}

func testEventRetriesUnreachableMaster(t *testing.T) {
	r1 := newRedis(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	later := l.Addr().String()
	l.Close()
	s1 := newSentinel(t, r1.addr())
	reset(t, s1)
	startWatch(t, s1)
	waitSubscribed(t, s1)
	waitMaster(t, r1.addr(), 2*time.Second)

	s1.set(func(f *sstate) { f.master = later })
	s1.publish(switchPayload("mymaster", r1.addr(), later))
	time.Sleep(time.Second)
	if cur() != r1.addr() {
		t.Fatalf("switched to unreachable %s", cur())
	}
	l, err = net.Listen("tcp", later)
	if err != nil {
		t.Skipf("cannot reopen %s: %v", later, err)
	}
	t.Cleanup(func() { l.Close() })
	go (&fakeRedis{l: l}).serve()
	waitMaster(t, later, 2*time.Second)
}

func TestEventIgnoredForOtherMasterOrGarbage(t *testing.T) {
	modes(t, testEventIgnoredForOtherMasterOrGarbage)
}

func testEventIgnoredForOtherMasterOrGarbage(t *testing.T) {
	r1, r2 := newRedis(t), newRedis(t)
	s1 := newSentinel(t, r1.addr())
	reset(t, s1)
	startWatch(t, s1)
	waitSubscribed(t, s1)
	waitMaster(t, r1.addr(), 2*time.Second)

	s1.set(func(f *sstate) { f.master = r2.addr() })
	s1.publish(switchPayload("other", r1.addr(), r2.addr()))
	s1.publish("garbage")
	s1.publish("")
	time.Sleep(500 * time.Millisecond)
	if cur() != r1.addr() {
		t.Fatalf("switched to %s on an ignored event", cur())
	}
	s1.publish(switchPayload("mymaster", r1.addr(), r2.addr()))
	waitMaster(t, r2.addr(), 2*time.Second)
}

func TestSilentSentinelDetected(t *testing.T) {
	modes(t, testSilentSentinelDetected)
}

func testSilentSentinelDetected(t *testing.T) {
	r1 := newRedis(t)
	s1 := newSentinel(t, r1.addr())
	s1.set(func(f *sstate) { f.silent = true })
	reset(t, s1)
	old := pingInterval
	pingInterval = 100 * time.Millisecond
	t.Cleanup(func() { pingInterval = old })
	ch := startWatch(t, s1)
	select {
	case err := <-ch:
		if !strings.Contains(err.Error(), "timeout") {
			t.Fatalf("unexpected error: %v", err)
		}
		ch <- err
	case <-time.After(3 * time.Second):
		t.Fatal("silent sentinel not detected")
	}
}

func TestLiveSentinelKeptByPing(t *testing.T) {
	modes(t, testLiveSentinelKeptByPing)
}

func testLiveSentinelKeptByPing(t *testing.T) {
	r1 := newRedis(t)
	s1 := newSentinel(t, r1.addr())
	reset(t, s1)
	old := pingInterval
	pingInterval = 100 * time.Millisecond
	t.Cleanup(func() { pingInterval = old })
	ch := startWatch(t, s1)
	select {
	case err := <-ch:
		t.Fatalf("watch ended on a live sentinel: %v", err)
	case <-time.After(time.Second):
	}
}

func countFDs(t *testing.T) int {
	e, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("no /proc/self/fd")
	}
	return len(e)
}

func TestWatchSentinelClosesConnections(t *testing.T) {
	modes(t, testWatchSentinelClosesConnections)
}

func testWatchSentinelClosesConnections(t *testing.T) {
	r1 := newRedis(t)
	s1 := newSentinel(t, r1.addr())
	s1.set(func(f *sstate) { f.dropSub = true })
	reset(t, s1)
	watchSentinel(s1.addr())
	time.Sleep(100 * time.Millisecond)
	before := countFDs(t)
	for i := 0; i < 200; i++ {
		if err := watchSentinel(s1.addr()); err == nil {
			t.Fatal("watchSentinel returned nil")
		}
	}
	time.Sleep(100 * time.Millisecond)
	if n := countFDs(t) - before; n > 20 {
		t.Fatalf("%d fds leaked over 200 reconnects", n)
	}
}

func startProxy(t *testing.T, addr *net.TCPAddr,
	stop <-chan struct{}) *net.TCPConn {
	t.Helper()
	l, err := net.ListenTCP("tcp",
		&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		l.Close()
		<-done
	})
	go func() {
		defer close(done)
		c, err := l.AcceptTCP()
		if err == nil {
			proxy(c, addr, stop)
		}
	}()
	c, err := net.DialTCP("tcp", nil, l.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(3 * time.Second))
	return c
}

func resolve(t *testing.T, s string) *net.TCPAddr {
	a, err := net.ResolveTCPAddr("tcp", s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestProxyRoundTrip(t *testing.T) {
	r1 := newRedis(t)
	c := startProxy(t, resolve(t, r1.addr()), make(chan struct{}))
	br := bufio.NewReader(c)
	for i := 0; i < 3; i++ {
		c.Write([]byte("PING\r\n"))
		line, err := br.ReadString('\n')
		if err != nil || line != "+PONG\r\n" {
			t.Fatalf("reply %q, %v", line, err)
		}
	}
}

func TestProxyHalfClose(t *testing.T) {
	r1 := newRedis(t)
	c := startProxy(t, resolve(t, r1.addr()), make(chan struct{}))
	c.Write([]byte("PING\r\nPING\r\n"))
	c.CloseWrite()
	b, err := io.ReadAll(c)
	if err != nil || string(b) != "+PONG\r\n+PONG\r\n" {
		t.Fatalf("reply after half-close %q, %v", b, err)
	}
}

func TestProxyClosedOnMasterChange(t *testing.T) {
	r1 := newRedis(t)
	stop := make(chan struct{})
	c := startProxy(t, resolve(t, r1.addr()), stop)
	c.Write([]byte("PING\r\n"))
	br := bufio.NewReader(c)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	close(stop)
	if _, err := br.ReadString('\n'); err != io.EOF {
		t.Fatalf("read after stop: %v, want EOF", err)
	}
}

func TestProxyRedisGoesAway(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err == nil {
			c.Write([]byte("-ERR bye\r\n"))
			c.Close()
		}
	}()
	c := startProxy(t, resolve(t, l.Addr().String()),
		make(chan struct{}))
	b, err := io.ReadAll(c)
	if err != nil || string(b) != "-ERR bye\r\n" {
		t.Fatalf("got %q, %v", b, err)
	}
}

func TestProxyNoMaster(t *testing.T) {
	c := startProxy(t, nil, make(chan struct{}))
	b, err := io.ReadAll(c)
	if err != nil || len(b) != 0 {
		t.Fatalf("got %q, %v, want immediate close", b, err)
	}
}

func TestProxyUnreachableMaster(t *testing.T) {
	c := startProxy(t, resolve(t, deadAddr(t)),
		make(chan struct{}))
	b, err := io.ReadAll(c)
	if err != nil || len(b) != 0 {
		t.Fatalf("got %q, %v, want immediate close", b, err)
	}
}

func TestFirstAnswerWinsByDefault(t *testing.T) {
	r1, r2 := newRedis(t), newRedis(t)
	s1 := newSentinel(t, r1.addr())
	s2 := newSentinel(t, r2.addr())
	s2.set(func(f *sstate) {
		f.peers = []peer{{addr: s1.addr()}}
	})
	reset(t, s1, s2)
	if *majority {
		t.Fatal("majority is on by default")
	}
	pollOK(t)
	if cur() != r1.addr() {
		t.Fatalf("master %s, want %s", cur(), r1.addr())
	}
	*sentinelAddr = s2.addr() + "," + s1.addr()
	pollOK(t)
	if cur() != r2.addr() {
		t.Fatalf("master %s, want %s", cur(), r2.addr())
	}
}

func TestFirstAnswerFallsThrough(t *testing.T) {
	r1 := newRedis(t)
	dead := newSentinel(t, deadAddr(t))
	empty := newSentinel(t, "")
	good := newSentinel(t, r1.addr())
	reset(t, dead, empty, good)
	*sentinelAddr = deadAddr(t) + "," + *sentinelAddr
	pollOK(t)
	if cur() != r1.addr() {
		t.Fatalf("master %s, want %s", cur(), r1.addr())
	}
}

func TestEventSwitchesImmediatelyByDefault(t *testing.T) {
	r1, r2 := newRedis(t), newRedis(t)
	s1 := newSentinel(t, r1.addr())
	s2 := newSentinel(t, r1.addr())
	s3 := newSentinel(t, r1.addr())
	reset(t, s1, s2, s3)
	startWatch(t, s1)
	waitSubscribed(t, s1)
	waitMaster(t, r1.addr(), 2*time.Second)

	s1.publish(switchPayload("mymaster", r1.addr(), r2.addr()))
	waitMaster(t, r2.addr(), time.Second)
}

func TestSubscribeAsksThatSentinelByDefault(t *testing.T) {
	r1, r2 := newRedis(t), newRedis(t)
	s1 := newSentinel(t, r1.addr())
	s2 := newSentinel(t, r2.addr())
	reset(t, s1, s2)
	startWatch(t, s2)
	waitSubscribed(t, s2)
	waitMaster(t, r2.addr(), 2*time.Second)
}

func TestNewerEventCancelsOlderRetry(t *testing.T) {
	r1, r3 := newRedis(t), newRedis(t)
	later := deadAddr(t)
	s1 := newSentinel(t, r1.addr())
	reset(t, s1)
	startWatch(t, s1)
	waitSubscribed(t, s1)
	waitMaster(t, r1.addr(), 2*time.Second)

	s1.publish(switchPayload("mymaster", r1.addr(), later))
	time.Sleep(300 * time.Millisecond)
	s1.set(func(f *sstate) { f.master = r3.addr() })
	s1.publish(switchPayload("mymaster", later, r3.addr()))
	waitMaster(t, r3.addr(), 2*time.Second)

	l, err := net.Listen("tcp", later)
	if err != nil {
		t.Skipf("cannot reopen %s: %v", later, err)
	}
	t.Cleanup(func() { l.Close() })
	go (&fakeRedis{l: l}).serve()
	time.Sleep(time.Second)
	if cur() != r3.addr() {
		t.Fatalf("stale event retry moved master to %s",
			cur())
	}
}

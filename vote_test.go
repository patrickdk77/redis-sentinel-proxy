package main

import (
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"
)

func conns(f *fakeSentinel) int {
	n := 0
	f.set(func(s *sstate) { n = len(s.conns) })
	return n
}

func withID(f *fakeSentinel, id string) *fakeSentinel {
	f.set(func(s *sstate) { s.runid = id })
	return f
}

func TestFakePeersCannotOutvote(t *testing.T) {
	good, evil := newRedis(t), newRedis(t)
	s1 := withID(newSentinel(t, good.addr()), "id1")
	s2 := withID(newSentinel(t, good.addr()), "id2")
	s3 := withID(newSentinel(t, good.addr()), "id3")
	var fakes []*fakeSentinel
	var ps []peer
	for i := 0; i < 4; i++ {
		f := withID(newSentinel(t, evil.addr()), fmt.Sprintf("fake%d", i))
		fakes = append(fakes, f)
		ps = append(ps, peer{addr: f.addr(), runid: fmt.Sprintf("fake%d", i)})
	}
	s3.set(func(f *sstate) { f.peers = ps })
	reset(t, s1, s2, s3)
	*majority = true
	pollOK(t)
	if cur() != good.addr() {
		t.Fatalf("master %s, want %s", cur(), good.addr())
	}
	for _, f := range fakes {
		if n := conns(f); n != 0 {
			t.Fatalf("peer reported by 1 of 3 sentinels was asked %d times", n)
		}
	}
}

func TestPeerReportedByMajorityVotes(t *testing.T) {
	r1, r2 := newRedis(t), newRedis(t)
	s1 := withID(newSentinel(t, r1.addr()), "id1")
	s2 := withID(newSentinel(t, r2.addr()), "id2")
	s4 := withID(newSentinel(t, r1.addr()), "id4")
	p4 := peer{addr: s4.addr(), runid: "id4"}
	s1.set(func(f *sstate) { f.peers = []peer{p4} })
	s2.set(func(f *sstate) { f.peers = []peer{p4} })
	reset(t, s1, s2)
	*majority = true
	pollOK(t)
	if cur() != r1.addr() {
		t.Fatalf("master %s, want %s", cur(), r1.addr())
	}
	s2.set(func(f *sstate) { f.peers = nil })
	masterMu.Lock()
	masterAddr = nil
	masterMu.Unlock()
	pollFails(t, "No master has a majority of 2")
}

func TestHostnamePeerCountedOnce(t *testing.T) {
	r1, r2 := newRedis(t), newRedis(t)
	s1 := withID(newSentinel(t, r1.addr()), "id1")
	s2 := withID(newSentinel(t, r1.addr()), "id2")
	s3 := withID(newSentinel(t, r2.addr()), "id3")
	_, p3, _ := net.SplitHostPort(s3.addr())
	byName := peer{addr: "localhost:" + p3, runid: "id3"}
	s1.set(func(f *sstate) { f.peers = []peer{byName} })
	s2.set(func(f *sstate) { f.peers = []peer{byName} })
	reset(t, s1, s2, s3)
	*majority = true
	pollOK(t)
	if cur() != r1.addr() {
		t.Fatalf("master %s, want %s", cur(), r1.addr())
	}
}

func TestSameSentinelTwoAddressesCountedOnce(t *testing.T) {
	r1, r2 := newRedis(t), newRedis(t)
	a := withID(newSentinel(t, r2.addr()), "same")
	a2 := withID(newSentinel(t, r2.addr()), "same")
	b := withID(newSentinel(t, r1.addr()), "idb")
	c := withID(newSentinel(t, r1.addr()), "idc")
	reset(t, a, a2, b, c)
	*majority = true
	pollOK(t)
	if cur() != r1.addr() {
		t.Fatalf("master %s, want %s", cur(), r1.addr())
	}
}

func TestSentinelsWithoutRunIDVoteByAddress(t *testing.T) {
	r1, r2 := newRedis(t), newRedis(t)
	s1 := newSentinel(t, r1.addr())
	s2 := newSentinel(t, r1.addr())
	s3 := newSentinel(t, r2.addr())
	reset(t, s1, s2, s3)
	*majority = true
	pollOK(t)
	if cur() != r1.addr() {
		t.Fatalf("master %s, want %s", cur(), r1.addr())
	}
}

func TestRunIDTimeoutLosesVote(t *testing.T) {
	r1, r2 := newRedis(t), newRedis(t)
	s1 := withID(newSentinel(t, r1.addr()), "id1")
	s2 := withID(newSentinel(t, r2.addr()), "id2")
	s2.set(func(f *sstate) { f.hangInfo = true })
	reset(t, s1, s2)
	*majority = true
	timeoutms = 200
	pollOK(t)
	if cur() != r1.addr() {
		t.Fatalf("master %s, want %s", cur(), r1.addr())
	}
}

func TestPeerCap(t *testing.T) {
	r1 := newRedis(t)
	seed := withID(newSentinel(t, r1.addr()), "seed")
	var peers []*fakeSentinel
	var ps []peer
	for i := 0; i < 2*maxPeers; i++ {
		id := fmt.Sprintf("p%02d", i)
		f := withID(newSentinel(t, r1.addr()), id)
		peers = append(peers, f)
		ps = append(ps, peer{addr: f.addr(), runid: id})
	}
	seed.set(func(f *sstate) { f.peers = ps })
	reset(t, seed)
	*majority = true
	pollOK(t)
	asked := 0
	for _, f := range peers {
		if conns(f) > 0 {
			asked++
		}
	}
	if asked != maxPeers {
		t.Fatalf("%d of %d peers asked, want %d", asked, len(peers), maxPeers)
	}
}

func TestPeerAddrs(t *testing.T) {
	p := func(addr, id string) peerInfo {
		return peerInfo{addr: addr, id: id}
	}
	var many []peerInfo
	for i := 0; i < 15; i++ {
		many = append(many, p(fmt.Sprintf("10.0.0.%02d:1", i), fmt.Sprintf("m%d", i)))
	}
	var want10 []string
	for i := 0; i < maxPeers; i++ {
		want10 = append(want10, fmt.Sprintf("10.0.0.%02d:1", i))
	}
	cases := []struct {
		name  string
		seeds []answer
		want  []string
	}{
		{"no seeds", nil, nil},
		{"single reporter", []answer{
			{addr: "s1:1", id: "1", peersOK: true, peers: []peerInfo{p("b:1", "b"),
				p("a:1", "a")}},
		}, []string{"a:1", "b:1"}},
		{"minority report dropped", []answer{
			{addr: "s1:1", id: "1", peersOK: true, peers: []peerInfo{p("x:1", "x")}},
			{addr: "s2:1", id: "2", peersOK: true},
			{addr: "s3:1", id: "3", peersOK: true},
		}, nil},
		{"failed reporters not counted", []answer{
			{addr: "s1:1", id: "1", peersOK: true, peers: []peerInfo{p("x:1", "x")}},
			{addr: "s2:1", err: fmt.Errorf("down")},
		}, []string{"x:1"}},
		{"duplicates in one report count once", []answer{
			{addr: "s1:1", id: "1", peersOK: true, peers: []peerInfo{p("x:1", "x"),
				p("x:1", "x")}},
			{addr: "s2:1", id: "2", peersOK: true},
		}, nil},
		{"seed address skipped", []answer{
			{addr: "s1:1", id: "1", peersOK: true, peers: []peerInfo{
				p("s1:1", "other")}},
		}, nil},
		{"seed id skipped", []answer{
			{addr: "s1:1", id: "1", peersOK: true, peers: []peerInfo{p("host:1", "1")}},
		}, nil},
		{"same id two addresses", []answer{
			{addr: "s1:1", id: "1", peersOK: true, peers: []peerInfo{p("a:1", "z"),
				p("b:1", "z")}},
		}, []string{"a:1"}},
		{"no id falls back to address", []answer{
			{addr: "s1:1", peersOK: true, peers: []peerInfo{p("a:1", ""),
				p("a:1", "")}},
		}, []string{"a:1"}},
		{"capped", []answer{
			{addr: "s1:1", id: "1", peersOK: true, peers: many},
		}, want10},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := peerAddrs(c.seeds)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestPeersParsing(t *testing.T) {
	r1 := newRedis(t)
	s1 := newSentinel(t, r1.addr())
	cases := []struct {
		name, raw string
		want      []peerInfo
	}{
		{"not an array", "+OK\r\n", nil},
		{"error", "-ERR no such master\r\n", nil},
		{"entry not an array", "*1\r\n+x\r\n", []peerInfo{}},
		{"odd fields", "*1\r\n*3\r\n" + bulk("ip") + bulk("1.2.3.4") + bulk("port"), []peerInfo{}},
		{"missing port", "*1\r\n*2\r\n" + bulk("ip") + bulk("1.2.3.4"), []peerInfo{}},
		{"non-string field", "*1\r\n*4\r\n" + bulk("ip") + "*0\r\n" + bulk("port") + bulk("1"), []peerInfo{}},
		{"full", "*1\r\n*8\r\n" + bulk("ip") + bulk("::1") + bulk("port") + bulk("26379") + bulk("runid") +
			bulk("abc") + bulk("flags") +
			bulk("sentinel"), []peerInfo{
			{addr: "[::1]:26379", id: "abc"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reset(t, s1)
			s1.set(func(f *sstate) { f.peersRaw = c.raw })
			s, err := dialSentinel(s1.addr())
			if err != nil {
				t.Fatal(err)
			}
			defer s.conn.Close()
			got, err := s.peers()
			if c.want == nil {
				if err == nil {
					t.Fatalf("got %v, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) == 0 && len(c.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestRunID(t *testing.T) {
	r1 := newRedis(t)
	s1 := newSentinel(t, r1.addr())
	reset(t, s1)
	cases := []struct{ name, raw, want string }{
		{"present", bulk("# Server\r\nrun_id:abc\r\n"), "abc"},
		{"absent", bulk("# Server\r\nredis_mode:x\r\n"), ""},
		{"error reply", "-NOPERM no\r\n", ""},
		{"not a string", "*0\r\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s1.set(func(f *sstate) { f.infoRaw = c.raw })
			s, err := dialSentinel(s1.addr())
			if err != nil {
				t.Fatal(err)
			}
			defer s.conn.Close()
			got, err := s.runID()
			if err != nil || got != c.want {
				t.Fatalf("runID = %q, %v, want %q", got, err, c.want)
			}
		})
	}
	s1.set(func(f *sstate) { f.infoRaw = ""; f.hangInfo = true })
	timeoutms = 100
	s, err := dialSentinel(s1.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer s.conn.Close()
	if _, err := s.runID(); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("runID on a silent sentinel: %v", err)
	}
}

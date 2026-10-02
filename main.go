package main

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jnovack/flag"
)

var (
	masterMu    sync.Mutex
	masterAddr  *net.TCPAddr
	masterStop  = make(chan struct{})
	eventGen    atomic.Uint64
	sentinelTLS *tls.Config
	activeConns atomic.Int64
	limitLogged atomic.Int64

	eventListener = flag.Bool("eventlistener", false, "enable event listener")
	majority      = flag.Bool("majority", false, "switch master only when most sentinels agree")
	localAddr     = flag.String("listen", ":9999", "local address")
	sentinelAddr  = flag.String("sentinel", ":26379", "remote address, split with ','")
	masterName    = flag.String("master", "mymaster", "name of the master redis node")
	username      = flag.String("username", "", "username (if any) to authenticate, v6 ACLs")
	password      = flag.String("password", "", "password (if any) to authenticate")
	debug         = flag.Bool("debug", false, "sets debug mode")
	timeout       = flag.Int("timeoutms", 2000, "connect timeout in milliseconds")
	check         = flag.Int("checkms", 250, "master change check interval in milliseconds")
	maxConns      = flag.Int("maxconns", 10000, "maximum client connections, 0 for no limit")
	tlsOn         = flag.Bool("sentineltls", false, "connect to sentinels over TLS")
	tlsCA         = flag.String("sentineltlsca", "", "CA file to verify sentinel certificates")
	tlsCert       = flag.String("sentineltlscert", "", "client certificate file for sentinel TLS")
	tlsKey        = flag.String("sentineltlskey", "", "client key file for sentinel TLS")
	tlsName       = flag.String("sentineltlsservername", "", "name to verify sentinel certificates against")
	timeoutms     time.Duration
	checkms       time.Duration

	pingInterval     = 5 * time.Second
	eventRetry       = 250 * time.Millisecond
	eventRetryWindow = 30 * time.Second
	acceptBackoff    = 100 * time.Millisecond
	limitLogEvery    = int64(10)
)

const (
	maxReplyDepth = 4
	maxReplyElems = 4096
	maxReplyBytes = 1 << 20
	maxPeers      = 10
)

func main() {
	flag.Parse()

	timeoutms = time.Duration(*timeout)
	checkms = time.Duration(*check)
	if err := checkConfig(); err != nil {
		log.Fatal(err)
	}
	var err error
	sentinelTLS, err = loadTLS()
	if err != nil {
		log.Fatal(err)
	}

	setupTermHandler()

	log.Printf("Listening on %s", *localAddr)
	laddr, err := net.ResolveTCPAddr("tcp", *localAddr)
	if err != nil {
		log.Fatalf("Failed to resolve local address: %s", err)
	}

	go master()

	listener, err := net.ListenTCP("tcp", laddr)
	if err != nil {
		log.Fatal(err)
	}

	serve(listener)
}

func checkConfig() error {
	switch {
	case *timeout <= 0:
		return errors.New("timeoutms must be greater than 0")
	case *check < 0:
		return errors.New("checkms must not be negative")
	case *check == 0 && !*eventListener:
		return errors.New("checkms 0 turns off polling, which needs eventlistener")
	case *maxConns < 0:
		return errors.New("maxconns must not be negative")
	}
	return nil
}

func loadTLS() (*tls.Config, error) {
	if !*tlsOn {
		if *tlsCA != "" || *tlsCert != "" || *tlsKey != "" || *tlsName != "" {
			return nil, errors.New("sentineltls settings given without sentineltls")
		}
		return nil, nil
	}
	cfg := &tls.Config{
		ServerName: *tlsName,
		MinVersion: tls.VersionTLS12,
	}
	if *tlsCA != "" {
		pem, err := os.ReadFile(*tlsCA)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = x509.NewCertPool()
		if !cfg.RootCAs.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates in %s", *tlsCA)
		}
	}
	if (*tlsCert == "") != (*tlsKey == "") {
		return nil, errors.New("sentineltlscert and sentineltlskey must be given together")
	}
	if *tlsCert != "" {
		cert, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

func serve(listener *net.TCPListener) {
	for {
		conn, err := listener.AcceptTCP()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			log.Println(err)
			time.Sleep(acceptBackoff)
			continue
		}
		if !acquireConn() {
			conn.Close()
			continue
		}
		addr, stop := currentMaster()
		go func() {
			defer activeConns.Add(-1)
			proxy(conn, addr, stop)
		}()
	}
}

func acquireConn() bool {
	n := activeConns.Add(1)
	if *maxConns == 0 || n <= int64(*maxConns) {
		return true
	}
	activeConns.Add(-1)
	now := time.Now().Unix()
	last := limitLogged.Load()
	if now-last >= limitLogEvery && limitLogged.CompareAndSwap(last, now) {
		log.Printf("[PROXY] Connection limit %d reached, refusing new connections", *maxConns)
	}
	return false
}

func setupTermHandler() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		fmt.Println("\r\n- SigTerm issued")
		os.Exit(0)
	}()
}

func timeoutDur() time.Duration {
	return timeoutms * time.Millisecond
}

func currentMaster() (*net.TCPAddr, <-chan struct{}) {
	masterMu.Lock()
	defer masterMu.Unlock()
	return masterAddr, masterStop
}

func setMaster(addr *net.TCPAddr) {
	masterMu.Lock()
	defer masterMu.Unlock()
	if addr.String() == masterAddr.String() {
		return
	}
	log.Printf("[MASTER] Master Address changed from %s to %s \n", masterAddr.String(), addr.String())
	masterAddr = addr
	close(masterStop)
	masterStop = make(chan struct{})
}

func master() {
	if *eventListener {
		log.Println("[MASTER] Event listener enabled")
		go subForSwitchMasterEvent()
	}
	if checkms == 0 {
		log.Println("[MASTER] Master polling disabled")
		select {}
	}
	for {
		// has master changed from last time?
		err := getMasterAddr()
		if err != nil {
			log.Printf("[MASTER] Error polling for new master: %s\n", err)
		}
		if addr, _ := currentMaster(); addr == nil {
			// if we haven't discovered a master at all, then slow our roll as the cluster is
			// probably still coming up
			time.Sleep(timeoutms * time.Millisecond)
		} else {
			// if we've seen a master before, then it's time for beast mode
			time.Sleep(checkms * time.Millisecond)
		}
	}
}

func pipe(r *net.TCPConn, w *net.TCPConn, done chan<- error) {
	bytes, err := io.Copy(w, r)
	if *debug {
		log.Printf("[PROXY %s => %s] Shutting down stream; transferred %v bytes: %v\n", w.RemoteAddr().String(),
			r.RemoteAddr().String(), bytes, err)
	}
	if err == nil {
		err = w.CloseWrite()
	}
	done <- err
}

func proxy(client *net.TCPConn, redisAddr *net.TCPAddr, stop <-chan struct{}) {
	from := client.RemoteAddr().String()
	conn, err := net.DialTimeout("tcp", redisAddr.String(), timeoutDur())
	if err != nil {
		log.Printf("[PROXY %s => %s] Can't establish connection: %s\n", from, redisAddr.String(), err)
		client.Close()
		return
	}
	redis := conn.(*net.TCPConn)

	if *debug {
		log.Printf("[PROXY %s => %s] New connection\n", from, redisAddr.String())
	}
	done := make(chan error, 2)

	go pipe(client, redis, done)
	go pipe(redis, client, done)

	finished := 0
wait:
	for finished < 2 {
		select {
		case <-stop:
			break wait
		case err := <-done:
			finished++
			if err != nil {
				break wait
			}
		}
	}
	client.Close()
	redis.Close()
	for ; finished < 2; finished++ {
		<-done
	}

	if *debug {
		log.Printf("[PROXY %s => %s] Closing connection\n", from, redisAddr.String())
	}
}

func setNewMaster(host string, port string, sentinelAddress string) error {
	//getting the string address for the master node
	stringaddr := net.JoinHostPort(host, port)
	addr, err := net.ResolveTCPAddr("tcp", stringaddr)
	if err != nil {
		log.Printf("[MASTER] Unable to resolve new master (from %s) %s: %s", sentinelAddress, stringaddr, err)
		return err
	}
	cur, _ := currentMaster()
	if cur.String() == addr.String() {
		return nil
	}
	//check that there's actually someone listening on that address
	conn2, err := net.DialTimeout("tcp", addr.String(), timeoutDur())
	if err != nil {
		log.Printf("[MASTER] Error checking new master (from %s) %s: %s", sentinelAddress, stringaddr, err)
		return err
	}
	conn2.Close()

	setMaster(addr)
	return nil
}

type respError string

func (e respError) Error() string { return string(e) }

type replyReader struct {
	r     *bufio.Reader
	elems int
	bytes int
}

func readReply(r *bufio.Reader) (interface{}, error) {
	rr := &replyReader{r: r}
	return rr.read(0)
}

func (rr *replyReader) read(depth int) (interface{}, error) {
	raw, err := rr.r.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return nil, errors.New("reply line too long")
	}
	if err != nil {
		return nil, err
	}
	line := strings.TrimRight(string(raw), "\r\n")
	if len(line) == 0 {
		return nil, errors.New("empty reply line")
	}
	switch line[0] {
	case '+', ':':
		return line[1:], nil
	case '-':
		return respError(line[1:]), nil
	case '$', '*':
		n, err := strconv.Atoi(line[1:])
		if err != nil {
			return nil, fmt.Errorf("bad length in %q", line)
		}
		if n < 0 {
			return nil, nil
		}
		if line[0] == '$' {
			return rr.bulk(n)
		}
		return rr.array(n, depth)
	}
	return nil, fmt.Errorf("unexpected reply %q", line)
}

func (rr *replyReader) bulk(n int) (interface{}, error) {
	if n > maxReplyBytes-rr.bytes {
		return nil, errors.New("reply too large")
	}
	rr.bytes += n
	b := make([]byte, n+2)
	if _, err := io.ReadFull(rr.r, b); err != nil {
		return nil, err
	}
	return string(b[:n]), nil
}

func (rr *replyReader) array(n, depth int) (interface{}, error) {
	if depth >= maxReplyDepth {
		return nil, errors.New("reply nested too deep")
	}
	if n > maxReplyElems-rr.elems {
		return nil, errors.New("reply has too many elements")
	}
	rr.elems += n
	arr := make([]interface{}, n)
	for i := range arr {
		var err error
		arr[i], err = rr.read(depth + 1)
		if err != nil {
			return nil, err
		}
	}
	return arr, nil
}

type sentinel struct {
	addr string
	conn net.Conn
	r    *bufio.Reader
}

func dialSentinel(addr string) (*sentinel, error) {
	if *debug {
		log.Printf("[MASTER] Connecting to Sentinel at %v", addr)
	}
	d := &net.Dialer{Timeout: timeoutDur()}
	var conn net.Conn
	var err error
	if sentinelTLS != nil {
		conn, err = tls.DialWithDialer(d, "tcp", addr, sentinelTLS)
	} else {
		conn, err = d.Dial("tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("[MASTER] Unable to connect to Sentinel at %v: %v", addr, err)
	}
	s := &sentinel{addr: addr, conn: conn, r: bufio.NewReader(conn)}
	if err := s.auth(); err != nil {
		conn.Close()
		return nil, err
	}
	return s, nil
}

func debugCmd(args []string) string {
	if len(args) > 1 && strings.EqualFold(args[0], "AUTH") {
		return args[0] + " <redacted>"
	}
	return strings.Join(args, " ")
}

func (s *sentinel) send(args ...string) error {
	if *debug {
		fmt.Println("> ", debugCmd(args))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	_, err := io.WriteString(s.conn, b.String())
	return err
}

func (s *sentinel) do(args ...string) (interface{}, error) {
	err := s.conn.SetDeadline(time.Now().Add(timeoutDur()))
	if err != nil {
		return nil, err
	}
	if err := s.send(args...); err != nil {
		return nil, err
	}
	reply, err := readReply(s.r)
	if err != nil {
		return nil, fmt.Errorf("Error reading from Sentinel %s: %s", s.addr, err)
	}
	if *debug {
		fmt.Printf("< %v\n", reply)
	}
	return reply, nil
}

func (s *sentinel) auth() error {
	if len(*password) == 0 {
		return nil
	}
	args := []string{"AUTH", *password}
	if len(*username) > 0 {
		args = []string{"AUTH", *username, *password}
	}
	_, err := s.do(args...)
	return err
}

func (s *sentinel) getMasterAddrByName() (string, error) {
	reply, err := s.do("SENTINEL", "get-master-addr-by-name", *masterName)
	if err != nil {
		return "", err
	}
	parts, _ := reply.([]interface{})
	if len(parts) == 2 {
		host, ok1 := parts[0].(string)
		port, ok2 := parts[1].(string)
		_, err := strconv.ParseUint(port, 10, 16)
		if ok1 && ok2 && err == nil {
			return net.JoinHostPort(host, port), nil
		}
	}
	return "", fmt.Errorf("Unexpected response from Sentinel %s: %v", s.addr, reply)
}

func (s *sentinel) runID() (string, error) {
	reply, err := s.do("INFO", "server")
	if err != nil {
		return "", err
	}
	text, _ := reply.(string)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if id, ok := strings.CutPrefix(line, "run_id:"); ok {
			return id, nil
		}
	}
	return "", nil
}

type peerInfo struct {
	addr string
	id   string
}

func (s *sentinel) peers() ([]peerInfo, error) {
	reply, err := s.do("SENTINEL", "SENTINELS", *masterName)
	if err != nil {
		return nil, err
	}
	list, ok := reply.([]interface{})
	if !ok {
		return nil, fmt.Errorf("Unexpected response from Sentinel %s: %v", s.addr, reply)
	}
	var peers []peerInfo
	for _, p := range list {
		fields, _ := p.([]interface{})
		m := map[string]string{}
		for i := 0; i+1 < len(fields); i += 2 {
			k, _ := fields[i].(string)
			v, _ := fields[i+1].(string)
			m[k] = v
		}
		if m["ip"] == "" || m["port"] == "" {
			continue
		}
		down := false
		for _, f := range strings.Split(m["flags"], ",") {
			if f == "s_down" || f == "disconnected" {
				down = true
			}
		}
		if !down {
			peers = append(peers, peerInfo{
				addr: net.JoinHostPort(m["ip"], m["port"]),
				id:   m["runid"],
			})
		}
	}
	return peers, nil
}

func resolveSentinelAddress(address string) ([]net.IP, string, error) {
	sentinelHost, sentinelPort, err := net.SplitHostPort(address)
	if err != nil {
		return nil, "", fmt.Errorf("Can't find Sentinel: %s", err)
	}

	sentinels, err := net.LookupIP(sentinelHost)
	if err != nil {
		return nil, "", fmt.Errorf("Can't lookup Sentinel: %s", err)
	}
	return sentinels, sentinelPort, nil
}

func sentinelSeeds() []string {
	seen := map[string]bool{}
	var addrs []string
	for _, a := range strings.Split(*sentinelAddr, ",") {
		ips, port, err := resolveSentinelAddress(a)
		if err != nil {
			log.Println(err)
			continue
		}
		for _, ip := range ips {
			addr := net.JoinHostPort(ip.String(), port)
			if !seen[addr] {
				seen[addr] = true
				addrs = append(addrs, addr)
			}
		}
	}
	return addrs
}

type answer struct {
	addr    string
	id      string
	master  string
	peers   []peerInfo
	peersOK bool
	err     error
}

func askSentinel(addr string, vote, discover bool) answer {
	a := answer{addr: addr}
	s, err := dialSentinel(addr)
	if err != nil {
		a.err = err
		return a
	}
	defer s.conn.Close()
	a.master, a.err = s.getMasterAddrByName()
	if a.err != nil || !vote {
		return a
	}
	a.id, a.err = s.runID()
	if a.err != nil || !discover {
		return a
	}
	a.peers, err = s.peers()
	if err != nil && *debug {
		log.Println(err)
	}
	a.peersOK = err == nil
	return a
}

func askAll(addrs []string, discover bool) []answer {
	answers := make([]answer, len(addrs))
	var wg sync.WaitGroup
	for i, addr := range addrs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			answers[i] = askSentinel(addr, true, discover)
		}()
	}
	wg.Wait()
	return answers
}

func getMasterAddr() error {
	if *majority {
		return voteMaster()
	}
	for _, addr := range sentinelSeeds() {
		a := askSentinel(addr, false, false)
		if a.err == nil {
			host, port, _ := net.SplitHostPort(a.master)
			a.err = setNewMaster(host, port, addr)
			if a.err == nil {
				return nil
			}
		}
		log.Println(a.err)
	}
	return fmt.Errorf("No Sentinels returned a valid master.")
}

func peerAddrs(seeds []answer) []string {
	known := map[string]bool{}
	reporters := 0
	reports := map[peerInfo]int{}
	for _, a := range seeds {
		known["addr "+a.addr] = true
		if a.id != "" {
			known["id "+a.id] = true
		}
		if !a.peersOK {
			continue
		}
		reporters++
		seen := map[peerInfo]bool{}
		for _, p := range a.peers {
			if !seen[p] {
				seen[p] = true
				reports[p]++
			}
		}
	}
	var agreed []peerInfo
	for p, n := range reports {
		if 2*n > reporters {
			agreed = append(agreed, p)
		}
	}
	sort.Slice(agreed, func(i, j int) bool {
		if agreed[i].addr != agreed[j].addr {
			return agreed[i].addr < agreed[j].addr
		}
		return agreed[i].id < agreed[j].id
	})
	var addrs []string
	for _, p := range agreed {
		if len(addrs) == maxPeers {
			break
		}
		if known["addr "+p.addr] || (p.id != "" && known["id "+p.id]) {
			continue
		}
		known["addr "+p.addr] = true
		if p.id != "" {
			known["id "+p.id] = true
		}
		addrs = append(addrs, p.addr)
	}
	return addrs
}

func voteMaster() error {
	answers := askAll(sentinelSeeds(), true)
	peers := askAll(peerAddrs(answers), false)
	answers = append(answers, peers...)

	votes := map[string]int{}
	counted := map[string]bool{}
	total := 0
	for _, a := range answers {
		if a.err != nil {
			if *debug {
				log.Println(a.err)
			}
			continue
		}
		key := "addr " + a.addr
		if a.id != "" {
			key = "id " + a.id
		}
		if counted[key] {
			continue
		}
		counted[key] = true
		votes[a.master]++
		total++
	}
	if total == 0 {
		return fmt.Errorf("No Sentinels returned a valid master.")
	}
	for addr, n := range votes {
		if 2*n > total {
			host, port, _ := net.SplitHostPort(addr)
			return setNewMaster(host, port, fmt.Sprintf("%d of %d sentinels", n,
				total))
		}
	}
	return fmt.Errorf("No master has a majority of %d sentinels: %v", total, votes)
}

func subForSwitchMasterEvent() {
	for {
		for _, addr := range sentinelSeeds() {
			err := watchSentinel(addr)
			log.Println(err)
			if *debug {
				log.Println("[MASTER] Got disconnected from Sentinel")
			}
		}
		time.Sleep(timeoutms * time.Millisecond)
	}
}

func watchSentinel(addr string) error {
	s, err := dialSentinel(addr)
	if err != nil {
		return err
	}
	defer s.conn.Close()

	err = s.conn.SetDeadline(time.Now().Add(timeoutDur()))
	if err != nil {
		return err
	}
	err = s.send("SUBSCRIBE", "+switch-master")
	if err != nil {
		return err
	}
	if err := syncMaster(addr); err != nil {
		log.Printf("[MASTER] Error polling for new master: %s\n", err)
	}

	interval := pingInterval
	wait := timeoutDur()
	done := make(chan struct{})
	stopped := make(chan struct{})
	defer func() {
		close(done)
		<-stopped
	}()
	go func() {
		defer close(stopped)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
			}
			err := s.conn.SetWriteDeadline(time.Now().Add(wait))
			if err != nil || s.send("PING") != nil {
				s.conn.Close()
				return
			}
		}
	}()

	for {
		err := s.conn.SetReadDeadline(time.Now().Add(3 * interval))
		if err != nil {
			return err
		}
		reply, err := readReply(s.r)
		if err != nil {
			return fmt.Errorf("[MASTER] Error reading from Sentinel %s: %s", addr, err)
		}
		if *debug {
			fmt.Printf("< %v\n", reply)
		}
		msg, _ := reply.([]interface{})
		if len(msg) != 3 || msg[0] != "message" {
			continue
		}
		payload, _ := msg[2].(string)
		parts := strings.Split(payload, " ")
		if len(parts) != 5 {
			log.Printf("[MASTER] Unexpected response from Sentinel %s: %s", addr, payload)
			continue
		}
		if parts[0] != *masterName {
			log.Printf("[MASTER] Got master change event for %s, but we are listening for %s", parts[0], *masterName)
			continue
		}
		go followSwitch(net.JoinHostPort(parts[3], parts[4]), addr+" event")
	}
}

func syncMaster(addr string) error {
	if *majority {
		return getMasterAddr()
	}
	a := askSentinel(addr, false, false)
	if a.err != nil {
		return a.err
	}
	host, port, _ := net.SplitHostPort(a.master)
	return setNewMaster(host, port, addr)
}

func followSwitch(target, source string) {
	gen := eventGen.Add(1)
	want, err := net.ResolveTCPAddr("tcp", target)
	if err != nil {
		log.Printf("[MASTER] Unable to resolve new master %s: %s", target, err)
		return
	}
	until := time.Now().Add(eventRetryWindow)
	host, port, _ := net.SplitHostPort(target)
	for eventGen.Load() == gen {
		var err error
		if *majority {
			err = getMasterAddr()
		} else {
			err = setNewMaster(host, port, source)
		}
		cur, _ := currentMaster()
		if cur.String() == want.String() {
			return
		}
		if time.Now().After(until) {
			log.Printf("[MASTER] Sentinels did not agree on %s after switch-master event: %v", target, err)
			return
		}
		time.Sleep(eventRetry)
	}
}

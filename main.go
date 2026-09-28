package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jnovack/flag"
)

var (
	masterMu   sync.Mutex
	masterAddr *net.TCPAddr
	masterStop = make(chan struct{})
	eventGen   atomic.Uint64

	eventListener = flag.Bool("eventlistener", false,
		"enable event listener")
	majority = flag.Bool("majority", false,
		"switch master only when most sentinels agree")
	localAddr    = flag.String("listen", ":9999", "local address")
	sentinelAddr = flag.String("sentinel", ":26379",
		"remote address, split with ','")
	masterName = flag.String("master", "mymaster",
		"name of the master redis node")
	username = flag.String("username", "",
		"username (if any) to authenticate, v6 ACLs")
	password = flag.String("password", "",
		"password (if any) to authenticate")
	debug   = flag.Bool("debug", false, "sets debug mode")
	timeout = flag.Int("timeoutms", 2000,
		"connect timeout in milliseconds")
	check = flag.Int("checkms", 250,
		"master change check interval in milliseconds")
	timeoutms time.Duration
	checkms   time.Duration

	pingInterval     = 5 * time.Second
	eventRetry       = 250 * time.Millisecond
	eventRetryWindow = 30 * time.Second
	acceptBackoff    = 100 * time.Millisecond
)

const maxReplyLen = 1 << 20

func main() {
	flag.Parse()

	timeoutms = time.Duration(*timeout)
	checkms = time.Duration(*check)

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

	for {
		conn, err := listener.AcceptTCP()
		if err != nil {
			log.Println(err)
			time.Sleep(acceptBackoff)
			continue
		}
		addr, stop := currentMaster()
		go proxy(conn, addr, stop)
	}
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
	log.Printf("[MASTER] Master Address changed from %s to %s \n",
		masterAddr.String(), addr.String())
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
			log.Printf("[MASTER] Error polling for new "+
				"master: %s\n", err)
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
		log.Printf("[PROXY %s => %s] Shutting down stream; "+
			"transferred %v bytes: %v\n",
			w.RemoteAddr().String(),
			r.RemoteAddr().String(), bytes, err)
	}
	if err == nil {
		err = w.CloseWrite()
	}
	done <- err
}

func proxy(client *net.TCPConn, redisAddr *net.TCPAddr,
	stop <-chan struct{}) {
	from := client.RemoteAddr().String()
	conn, err := net.DialTimeout("tcp", redisAddr.String(),
		timeoutDur())
	if err != nil {
		log.Printf("[PROXY %s => %s] Can't establish "+
			"connection: %s\n", from, redisAddr.String(),
			err)
		client.Close()
		return
	}
	redis := conn.(*net.TCPConn)

	if *debug {
		log.Printf("[PROXY %s => %s] New connection\n",
			from, redisAddr.String())
	}
	defer client.Close()
	defer redis.Close()

	done := make(chan error, 2)

	go pipe(client, redis, done)
	go pipe(redis, client, done)

wait:
	for i := 0; i < 2; i++ {
		select {
		case <-stop:
			break wait
		case err := <-done:
			if err != nil {
				break wait
			}
		}
	}

	if *debug {
		log.Printf("[PROXY %s => %s] Closing connection\n",
			from, redisAddr.String())
	}
}

func setNewMaster(host string, port string,
	sentinelAddress string) error {
	//getting the string address for the master node
	stringaddr := net.JoinHostPort(host, port)
	addr, err := net.ResolveTCPAddr("tcp", stringaddr)
	if err != nil {
		log.Printf("[MASTER] Unable to resolve new "+
			"master (from %s) %s: %s", sentinelAddress,
			stringaddr, err)
		return err
	}
	cur, _ := currentMaster()
	if cur.String() == addr.String() {
		return nil
	}
	//check that there's actually someone listening on that address
	conn2, err := net.DialTimeout("tcp", addr.String(),
		timeoutDur())
	if err != nil {
		log.Printf("[MASTER] Error checking new "+
			"master (from %s) %s: %s", sentinelAddress,
			stringaddr, err)
		return err
	}
	conn2.Close()

	setMaster(addr)
	return nil
}

type respError string

func (e respError) Error() string { return string(e) }

func readReply(r *bufio.Reader) (interface{}, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
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
		if err != nil || n > maxReplyLen {
			return nil, fmt.Errorf("bad length in %q",
				line)
		}
		if n < 0 {
			return nil, nil
		}
		if line[0] == '$' {
			b := make([]byte, n+2)
			_, err := io.ReadFull(r, b)
			if err != nil {
				return nil, err
			}
			return string(b[:n]), nil
		}
		arr := make([]interface{}, n)
		for i := range arr {
			arr[i], err = readReply(r)
			if err != nil {
				return nil, err
			}
		}
		return arr, nil
	}
	return nil, fmt.Errorf("unexpected reply %q", line)
}

type sentinel struct {
	addr string
	conn net.Conn
	r    *bufio.Reader
}

func dialSentinel(addr string) (*sentinel, error) {
	if *debug {
		log.Printf("[MASTER] Connecting to Sentinel at %v",
			addr)
	}
	conn, err := net.DialTimeout("tcp", addr, timeoutDur())
	if err != nil {
		return nil, fmt.Errorf("[MASTER] Unable to connect "+
			"to Sentinel at %v: %v", addr, err)
	}
	s := &sentinel{addr: addr, conn: conn,
		r: bufio.NewReader(conn)}
	if err := s.auth(); err != nil {
		conn.Close()
		return nil, err
	}
	return s, nil
}

func (s *sentinel) send(args ...string) error {
	if *debug {
		fmt.Println("> ", strings.Join(args, " "))
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
	s.conn.SetDeadline(time.Now().Add(timeoutDur()))
	if err := s.send(args...); err != nil {
		return nil, err
	}
	reply, err := readReply(s.r)
	if err != nil {
		return nil, fmt.Errorf("Error reading from "+
			"Sentinel %s: %s", s.addr, err)
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
	reply, err := s.do("SENTINEL", "get-master-addr-by-name",
		*masterName)
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
	return "", fmt.Errorf("Unexpected response from "+
		"Sentinel %s: %v", s.addr, reply)
}

func (s *sentinel) peers() ([]string, error) {
	reply, err := s.do("SENTINEL", "SENTINELS", *masterName)
	if err != nil {
		return nil, err
	}
	list, ok := reply.([]interface{})
	if !ok {
		return nil, fmt.Errorf("Unexpected response from "+
			"Sentinel %s: %v", s.addr, reply)
	}
	var addrs []string
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
			addrs = append(addrs,
				net.JoinHostPort(m["ip"], m["port"]))
		}
	}
	return addrs, nil
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
	master string
	peers  []string
	err    error
}

func askSentinel(addr string, discover bool) answer {
	s, err := dialSentinel(addr)
	if err != nil {
		return answer{err: err}
	}
	defer s.conn.Close()
	var a answer
	a.master, a.err = s.getMasterAddrByName()
	if discover && a.err == nil {
		a.peers, err = s.peers()
		if err != nil && *debug {
			log.Println(err)
		}
	}
	return a
}

func askAll(addrs []string, discover bool) []answer {
	answers := make([]answer, len(addrs))
	var wg sync.WaitGroup
	for i, addr := range addrs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			answers[i] = askSentinel(addr, discover)
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
		a := askSentinel(addr, false)
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

func voteMaster() error {
	seeds := sentinelSeeds()
	answers := askAll(seeds, true)
	asked := map[string]bool{}
	for _, addr := range seeds {
		asked[addr] = true
	}
	var peers []string
	for _, a := range answers {
		for _, p := range a.peers {
			if !asked[p] {
				asked[p] = true
				peers = append(peers, p)
			}
		}
	}
	answers = append(answers, askAll(peers, false)...)

	votes := map[string]int{}
	total := 0
	for _, a := range answers {
		if a.err != nil {
			if *debug {
				log.Println(a.err)
			}
			continue
		}
		votes[a.master]++
		total++
	}
	if total == 0 {
		return fmt.Errorf("No Sentinels returned a valid " +
			"master.")
	}
	for addr, n := range votes {
		if 2*n > total {
			host, port, _ := net.SplitHostPort(addr)
			return setNewMaster(host, port,
				fmt.Sprintf("%d of %d sentinels", n,
					total))
		}
	}
	return fmt.Errorf("No master has a majority of %d "+
		"sentinels: %v", total, votes)
}

func subForSwitchMasterEvent() {
	for {
		for _, addr := range sentinelSeeds() {
			err := watchSentinel(addr)
			log.Println(err)
			if *debug {
				log.Println("[MASTER] Got " +
					"disconnected from Sentinel")
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

	s.conn.SetDeadline(time.Now().Add(timeoutDur()))
	err = s.send("SUBSCRIBE", "+switch-master")
	if err != nil {
		return err
	}
	if err := syncMaster(addr); err != nil {
		log.Printf("[MASTER] Error polling for new master: "+
			"%s\n", err)
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
			s.conn.SetWriteDeadline(time.Now().Add(wait))
			if s.send("PING") != nil {
				s.conn.Close()
				return
			}
		}
	}()

	for {
		s.conn.SetReadDeadline(time.Now().Add(3 * interval))
		reply, err := readReply(s.r)
		if err != nil {
			return fmt.Errorf("[MASTER] Error reading "+
				"from Sentinel %s: %s", addr, err)
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
			log.Printf("[MASTER] Unexpected response "+
				"from Sentinel %s: %s", addr, payload)
			continue
		}
		if parts[0] != *masterName {
			log.Printf("[MASTER] Got master change "+
				"event for %s, but we are listening "+
				"for %s",
				parts[0], *masterName)
			continue
		}
		go followSwitch(net.JoinHostPort(parts[3], parts[4]),
			addr+" event")
	}
}

func syncMaster(addr string) error {
	if *majority {
		return getMasterAddr()
	}
	a := askSentinel(addr, false)
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
		log.Printf("[MASTER] Unable to resolve new master "+
			"%s: %s", target, err)
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
			log.Printf("[MASTER] Sentinels did not "+
				"agree on %s after switch-master "+
				"event: %v",
				target, err)
			return
		}
		time.Sleep(eventRetry)
	}
}

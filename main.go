package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jnovack/flag"
)

var (
	masterAddr *net.TCPAddr

	eventListener = flag.Bool("eventlistener", false, "enable event listener")
	localAddr     = flag.String("listen", ":9999", "local address")
	sentinelAddr  = flag.String("sentinel", ":26379", "remote address, split with ','")
	masterName    = flag.String("master", "mymaster", "name of the master redis node")
	username      = flag.String("username", "", "username (if any) to authenticate, v6 ACLs")
	password      = flag.String("password", "", "password (if any) to authenticate")
	debug         = flag.Bool("debug", false, "sets debug mode")
	timeout       = flag.Int("timeoutms", 2000, "connect timeout in milliseconds")
	check         = flag.Int("checkms", 250, "master change check interval in milliseconds")
	timeoutms     time.Duration
	checkms       time.Duration
)

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

	stopChan := make(chan string)
	go master(&stopChan)

	listener, err := net.ListenTCP("tcp", laddr)
	if err != nil {
		log.Fatal(err)
	}

	for {
		conn, err := listener.AcceptTCP()
		if err != nil {
			log.Println(err)
			continue
		}
		go proxy(conn, masterAddr, stopChan)
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

func master(stopChan *chan string) {
	var err error
	if *eventListener {
		log.Println("[MASTER] Event listener enabled")
		go subForSwitchMasterEvent(stopChan)
	}
	if checkms == 0 {
		log.Println("[MASTER] Master polling disabled")
		select {}
	}
	for {
		// has master changed from last time?
		err = getMasterAddr(stopChan)
		if err != nil {
			log.Printf("[MASTER] Error polling for new master: %s\n", err)
		}
		if masterAddr == nil {
			// if we haven't discovered a master at all, then slow our roll as the cluster is
			// probably still coming up
			time.Sleep(checkms * time.Second)
		} else {
			// if we've seen a master before, then it's time for beast mode
			time.Sleep(checkms * time.Millisecond)
		}
	}
}

func pipe(r net.Conn, w net.Conn, proxyChan chan<- string) {
	bytes, err := io.Copy(w, r)
	if *debug {
		log.Printf("[PROXY %s => %s] Shutting down stream; transferred %v bytes: %v\n", w.RemoteAddr().String(), r.RemoteAddr().String(), bytes, err)
	}
	close(proxyChan)
}

// pass a stopChan to the go routtine
func proxy(client *net.TCPConn, redisAddr *net.TCPAddr, stopChan <-chan string) {
	redis, err := net.DialTimeout("tcp", redisAddr.String(), timeoutms*time.Millisecond)
	if err != nil {
		log.Printf("[PROXY %s => %s] Can't establish connection: %s\n", client.RemoteAddr().String(), redisAddr.String(), err)
		client.Close()
		return
	}

	if *debug {
		log.Printf("[PROXY %s => %s] New connection\n", client.RemoteAddr().String(), redisAddr.String())
	}
	defer client.Close()
	defer redis.Close()

	clientChan := make(chan string)
	redisChan := make(chan string)

	go pipe(client, redis, redisChan)
	go pipe(redis, client, clientChan)

	select {
	case <-stopChan:
	case <-clientChan:
	case <-redisChan:
	}

	if *debug {
		log.Printf("[PROXY %s => %s] Closing connection\n", client.RemoteAddr().String(), redisAddr.String())
	}
}

func setNewMaster(host string, port string, sentinelAddress string, stopChan *chan string) error {
	//getting the string address for the master node
	stringaddr := net.JoinHostPort(host, port)
	addr, err := net.ResolveTCPAddr("tcp", stringaddr)
	if err != nil {
		log.Printf("[MASTER] Unable to resolve new master (from %s) %s: %s", sentinelAddress, stringaddr, err)
		return err
	}
	//check that there's actually someone listening on that address
	conn2, err := net.DialTimeout("tcp", addr.String(), timeoutms*time.Millisecond)
	if err != nil {
		log.Printf("[MASTER] Error checking new master (from %s) %s: %s", sentinelAddress, stringaddr, err)
		return err
	}
	defer conn2.Close()

	if addr != nil && addr.String() != masterAddr.String() {
		log.Printf("[MASTER] Master Address changed from %s to %s \n", masterAddr.String(), addr.String())
		masterAddr = addr
		close(*stopChan)
		*stopChan = make(chan string)
	}
	return err
}

func writeToConn(conn net.Conn, command string) {
	if *debug {
		fmt.Println("> ", command)
	}
	conn.Write([]byte(fmt.Sprintf("%s\n", command)))
}

func authSentinel(conn net.Conn) {
	if len(*password) > 0 {
		if len(*username) > 0 {
			writeToConn(conn, fmt.Sprintf("AUTH %s %s", *username, *password))
		} else {
			writeToConn(conn, fmt.Sprintf("AUTH %s", *password))
		}
		authResp := make([]byte, 256)
		conn.Read(authResp)

		if *debug {
			fmt.Print("< ", string(authResp))
		}
	}
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

func getSentinelConn(host net.IP, port string) (net.Conn, error) {
	sentineladdr := net.JoinHostPort(host.String(), port)
	if *debug {
		log.Printf("[MASTER] Connecting to Sentinel at %v:%v", host, port)
	}
	conn, err := net.DialTimeout("tcp", sentineladdr, timeoutms*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("[MASTER] Unable to connect to Sentinel at %v:%v: %v", host, port, err)
	}
	authSentinel(conn)
	return conn, nil
}

func getMasterAddrByName(conn net.Conn, stopChan *chan string) error {
	sentinelAddress := conn.RemoteAddr().String()
	writeToConn(conn, fmt.Sprintf("sentinel get-master-addr-by-name %s", *masterName))
	b := make([]byte, 256)
	_, err := conn.Read(b)
	if err != nil {
		log.Printf("[MASTER] Error reading from Sentinel %s: %s", sentinelAddress, err)
	}

	parts := strings.Split(string(b), "\r\n")
	if *debug {
		fmt.Println("< ", string(b))
	}

	if len(parts) < 5 {
		return fmt.Errorf("Unexpected response from Sentinel %s: %s", sentinelAddress, string(b))
	}
	return setNewMaster(parts[2], parts[4], sentinelAddress, stopChan)
}

func subForSwitchMasterEvent(stopChan *chan string) {
	sentinelAddress_list := strings.Split(*sentinelAddr, ",")
	for {
		for _, sentinelAddress := range sentinelAddress_list {
			sentinels, sentinelPort, err := resolveSentinelAddress(sentinelAddress)
			if err != nil {
				log.Panicln(err)
				return
			}
			for _, sentinelIP := range sentinels {
				conn, err := getSentinelConn(sentinelIP, sentinelPort)
				if err != nil {
					log.Println(err)
					continue
				}
				defer conn.Close()

				getMasterAddrByName(conn, stopChan)
				writeToConn(conn, "subscribe +switch-master")
				reader := bufio.NewReader(conn)
				for {
					message, err := reader.ReadString('\n')
					if err != nil {
						fmt.Println("Error reading from connection:", err)
						break
					}
					if *debug {
						fmt.Print("< ", message)
					}
					parts := strings.Split(message, " ")
					if len(parts) == 1 {
						continue
					}
					if len(parts) != 5 {
						log.Printf("[MASTER] Unexpected response from Sentinel %v:%v: %s", sentinelIP, sentinelPort, message)
						continue
					}
					if parts[0] != *masterName {
						log.Printf("[MASTER] Got master change event for %s, but we are listening for %s", parts[0], *masterName)
						continue
					}

					setNewMaster(parts[3], parts[4], fmt.Sprintf("%v:%v", sentinelIP, sentinelPort), stopChan)
				}
				if *debug {
					log.Println("[MASTER] Got disconnected from Sentinel")
				}
			}
		}
	}
}

func getMasterAddr(stopChan *chan string) error {
	sentinelAddress_list := strings.Split(*sentinelAddr, ",")
	for _, sentinelAddress := range sentinelAddress_list {
		sentinels, sentinelPort, err := resolveSentinelAddress(sentinelAddress)
		if err != nil {
			return err
		}
		for _, sentinelIP := range sentinels {
			conn, err := getSentinelConn(sentinelIP, sentinelPort)
			if err != nil {
				log.Println(err)
				continue
			}
			defer conn.Close()

			err = getMasterAddrByName(conn, stopChan)
			if err == nil {
				return nil
			}
			log.Println(err)
			continue
		}
	}
	return fmt.Errorf("No Sentinels returned a valid master.")
}

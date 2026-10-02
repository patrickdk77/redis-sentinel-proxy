package main

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type fakeServer struct {
	l     net.Listener
	role  string
	pass  string
	mu    sync.Mutex
	auths []string
}

func bulk(s string) string {
	return fmt.Sprintf("$%d\r\n%s\r\n", len(s), s)
}

func newServer(t *testing.T, role, pass string) *fakeServer {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeServer{l: l, role: role, pass: pass}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(line[1:]))
	if err != nil {
		return nil, err
	}
	var args []string
	for i := 0; i < n; i++ {
		if _, err := r.ReadString('\n'); err != nil {
			return nil, err
		}
		arg, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		args = append(args, strings.TrimSpace(arg))
	}
	return args, nil
}

func (f *fakeServer) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	authed := f.pass == ""
	for {
		args, err := readCommand(r)
		if err != nil || len(args) == 0 {
			return
		}
		switch strings.ToLower(args[0]) {
		case "auth":
			f.mu.Lock()
			f.auths = append(f.auths, strings.Join(args[1:], " "))
			f.mu.Unlock()
			if args[len(args)-1] == f.pass {
				authed = true
				c.Write([]byte("+OK\r\n"))
			} else {
				c.Write([]byte("-WRONGPASS bad\r\n"))
			}
		case "role":
			if !authed {
				c.Write([]byte("-NOAUTH x\r\n"))
				continue
			}
			c.Write([]byte(f.role))
		default:
			c.Write([]byte("-ERR unknown command\r\n"))
		}
	}
}

func TestCheckRoles(t *testing.T) {
	master := "*3\r\n" + bulk("master") + ":0\r\n*0\r\n"
	cases := []struct {
		name, reply string
		want        int
	}{
		{"master", master, 0},
		{"replica connected", "*5\r\n" + bulk("slave") + bulk("127.0.0.1") + ":6379\r\n" +
			bulk("connected") + ":0\r\n", 127},
		{"replica disconnected", "*5\r\n" + bulk("slave") + bulk("127.0.0.1") + ":6379\r\n" +
			bulk("connect") + ":-1\r\n", 127},
		{"sentinel", "*2\r\n" + bulk("sentinel") + "*0\r\n", 127},
		{"not an array", "+OK\r\n", 1},
		{"empty array", "*0\r\n", 1},
		{"short replica reply", "*1\r\n" + bulk("slave"), 127},
		{"error reply", "-ERR nope\r\n", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newServer(t, c.reply, "")
			got := check(f.l.Addr().String(), "", "")
			if got != c.want {
				t.Fatalf("check = %d, want %d", got, c.want)
			}
		})
	}
}

func TestCheckAuth(t *testing.T) {
	master := "*3\r\n" + bulk("master") + ":0\r\n*0\r\n"
	f := newServer(t, master, "secret")
	if got := check(f.l.Addr().String(), "", "wrong"); got != 1 {
		t.Fatalf("wrong password: check = %d, want 1", got)
	}
	if got := check(f.l.Addr().String(), "", ""); got != 1 {
		t.Fatalf("no password: check = %d, want 1", got)
	}
	got := check(f.l.Addr().String(), "u", "secret")
	if got != 0 {
		t.Fatalf("right password: check = %d, want 0", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.auths[len(f.auths)-1] != "u secret" {
		t.Fatalf("auths %q, want last \"u secret\"", f.auths)
	}
}

func TestCheckUnreachable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	if got := check(addr, "", ""); got != 1 {
		t.Fatalf("check = %d, want 1", got)
	}
}

func TestSettings(t *testing.T) {
	cases := []struct {
		name             string
		env              map[string]string
		host, user, pass string
	}{
		{"defaults", nil, "localhost:9999", "", ""},
		{"listen", map[string]string{"LISTEN": ":6379"}, ":6379", "", ""},
		{"password", map[string]string{"PASSWORD": "p"}, "localhost:9999", "", "p"},
		{"redis password wins", map[string]string{
			"PASSWORD": "p", "REDIS_PASSWORD": "rp",
			"USERNAME": "u"},
			"localhost:9999", "u", "rp"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			keys := []string{"LISTEN", "USERNAME", "PASSWORD", "REDIS_PASSWORD"}
			for _, k := range keys {
				t.Setenv(k, c.env[k])
			}
			h, u, p := settings()
			if h != c.host || u != c.user || p != c.pass {
				t.Fatalf("settings = %q %q %q, want %q %q %q", h, u, p, c.host, c.user, c.pass)
			}
		})
	}
}

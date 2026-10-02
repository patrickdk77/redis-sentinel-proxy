package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type pki struct {
	dir    string
	caFile string
	ca     *x509.Certificate
	caKey  *ecdsa.PrivateKey
	pool   *x509.CertPool
	serial int64
}

func writePEM(t *testing.T, path, kind string, der []byte) {
	t.Helper()
	b := pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	p := &pki{dir: t.TempDir(), ca: ca, caKey: key, pool: x509.NewCertPool(), serial: 1}
	p.pool.AddCert(ca)
	p.caFile = filepath.Join(p.dir, "ca.pem")
	writePEM(t, p.caFile, "CERTIFICATE", der)
	return p
}

func (p *pki) issue(t *testing.T, name string, dns []string,
	ips []net.IP, client bool) (string, string, tls.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p.serial++
	usage := x509.ExtKeyUsageServerAuth
	if client {
		usage = x509.ExtKeyUsageClientAuth
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(p.serial),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     dns,
		IPAddresses:  ips,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile := filepath.Join(p.dir, name+".pem")
	keyFile := filepath.Join(p.dir, name+"-key.pem")
	writePEM(t, certFile, "CERTIFICATE", der)
	writePEM(t, keyFile, "EC PRIVATE KEY", kder)
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile, cert
}

var localIP = []net.IP{net.IPv4(127, 0, 0, 1)}

func newTLSSentinel(t *testing.T, master string, cfg *tls.Config) *fakeSentinel {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return startSentinel(t, tls.NewListener(l, cfg), master)
}

func useTLS(t *testing.T) {
	t.Helper()
	*tlsOn = true
	cfg, err := loadTLS()
	if err != nil {
		t.Fatal(err)
	}
	sentinelTLS = cfg
}

func TestSentinelTLS(t *testing.T) {
	p := newPKI(t)
	_, _, srv := p.issue(t, "srv", nil, localIP, false)
	r1 := newRedis(t)
	s := newTLSSentinel(t, r1.addr(), &tls.Config{Certificates: []tls.Certificate{srv}})
	reset(t, s)
	*tlsCA = p.caFile
	useTLS(t)
	pollOK(t)
	if cur() != r1.addr() {
		t.Fatalf("master %s, want %s", cur(), r1.addr())
	}
}

func TestSentinelTLSUntrusted(t *testing.T) {
	p := newPKI(t)
	_, _, srv := p.issue(t, "srv", nil, localIP, false)
	r1 := newRedis(t)
	s := newTLSSentinel(t, r1.addr(), &tls.Config{Certificates: []tls.Certificate{srv}})
	reset(t, s)
	useTLS(t)
	pollFails(t, "No Sentinels returned")
	other := newPKI(t)
	*tlsCA = other.caFile
	useTLS(t)
	pollFails(t, "No Sentinels returned")
}

func TestSentinelTLSServerName(t *testing.T) {
	p := newPKI(t)
	_, _, srv := p.issue(t, "srv", []string{"sentinel.test"}, nil, false)
	r1 := newRedis(t)
	s := newTLSSentinel(t, r1.addr(), &tls.Config{Certificates: []tls.Certificate{srv}})
	reset(t, s)
	*tlsCA = p.caFile
	useTLS(t)
	pollFails(t, "No Sentinels returned")
	*tlsName = "sentinel.test"
	useTLS(t)
	pollOK(t)
	*tlsName = "wrong.test"
	useTLS(t)
	masterMu.Lock()
	masterAddr = nil
	masterMu.Unlock()
	pollFails(t, "No Sentinels returned")
}

func TestSentinelMutualTLS(t *testing.T) {
	p := newPKI(t)
	_, _, srv := p.issue(t, "srv", nil, localIP, false)
	cf, kf, _ := p.issue(t, "client", nil, nil, true)
	r1 := newRedis(t)
	s := newTLSSentinel(t, r1.addr(), &tls.Config{
		Certificates: []tls.Certificate{srv},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    p.pool,
	})
	reset(t, s)
	*tlsCA = p.caFile
	useTLS(t)
	pollFails(t, "No Sentinels returned")
	*tlsCert, *tlsKey = cf, kf
	useTLS(t)
	pollOK(t)
	if cur() != r1.addr() {
		t.Fatalf("master %s, want %s", cur(), r1.addr())
	}
}

func TestSentinelTLSMismatch(t *testing.T) {
	p := newPKI(t)
	_, _, srv := p.issue(t, "srv", nil, localIP, false)
	r1 := newRedis(t)
	plain := newSentinel(t, r1.addr())
	reset(t, plain)
	*tlsCA = p.caFile
	useTLS(t)
	pollFails(t, "No Sentinels returned")
	secure := newTLSSentinel(t, r1.addr(), &tls.Config{Certificates: []tls.Certificate{srv}})
	reset(t, secure)
	pollFails(t, "No Sentinels returned")
}

func TestSentinelTLSEvents(t *testing.T) {
	p := newPKI(t)
	_, _, srv := p.issue(t, "srv", nil, localIP, false)
	r1, r2 := newRedis(t), newRedis(t)
	s := newTLSSentinel(t, r1.addr(), &tls.Config{Certificates: []tls.Certificate{srv}})
	reset(t, s)
	*tlsCA = p.caFile
	useTLS(t)
	startWatch(t, s)
	waitSubscribed(t, s)
	waitMaster(t, r1.addr(), 2*time.Second)
	s.set(func(f *sstate) { f.master = r2.addr() })
	s.publish(switchPayload("mymaster", r1.addr(), r2.addr()))
	waitMaster(t, r2.addr(), 2*time.Second)
}

func TestLoadTLS(t *testing.T) {
	p := newPKI(t)
	cf, kf, _ := p.issue(t, "client", nil, nil, true)
	_, kf2, _ := p.issue(t, "other", nil, nil, true)
	garbage := filepath.Join(p.dir, "garbage.pem")
	err := os.WriteFile(garbage, []byte("x"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(p.dir, "missing.pem")
	type flags struct {
		on                  bool
		ca, cert, key, name string
	}
	cases := []struct {
		name string
		f    flags
		want string
	}{
		{"off", flags{}, ""},
		{"off with ca", flags{ca: p.caFile}, "given without sentineltls"},
		{"off with cert", flags{cert: cf, key: kf}, "given without sentineltls"},
		{"off with name", flags{name: "x"}, "given without sentineltls"},
		{"on, system roots", flags{on: true}, ""},
		{"on, ca", flags{on: true, ca: p.caFile}, ""},
		{"missing ca", flags{on: true, ca: missing}, "no such file"},
		{"ca without certs", flags{on: true, ca: garbage}, "no certificates"},
		{"cert without key", flags{on: true, cert: cf}, "must be given together"},
		{"key without cert", flags{on: true, key: kf}, "must be given together"},
		{"mismatched pair", flags{on: true, cert: cf, key: kf2}, "private key does not match"},
		{"missing cert", flags{on: true, cert: missing, key: kf}, "no such file"},
		{"full", flags{on: true, ca: p.caFile, cert: cf, key: kf, name: "sentinel.test"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reset(t)
			*tlsOn = c.f.on
			*tlsCA, *tlsCert = c.f.ca, c.f.cert
			*tlsKey, *tlsName = c.f.key, c.f.name
			cfg, err := loadTLS()
			if c.want != "" {
				if err == nil || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("error %v, want %q", err, c.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !c.f.on {
				if cfg != nil {
					t.Fatal("TLS config while off")
				}
				return
			}
			if cfg.MinVersion != tls.VersionTLS12 {
				t.Fatalf("MinVersion %x", cfg.MinVersion)
			}
			if cfg.ServerName != c.f.name {
				t.Fatalf("ServerName %q", cfg.ServerName)
			}
			if (cfg.RootCAs != nil) != (c.f.ca != "") {
				t.Fatalf("RootCAs set: %v", cfg.RootCAs != nil)
			}
			if (len(cfg.Certificates) == 1) !=
				(c.f.cert != "") {
				t.Fatalf("%d client certificates", len(cfg.Certificates))
			}
		})
	}
}

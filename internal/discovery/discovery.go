// Package discovery handles Carvera UDP machine discovery on port 3333.
//
// The real machine broadcasts "name,ip,port,busy" once per second. The official
// controller binds UDP 3333 and reads those datagrams to populate its machine
// list, then opens TCP to the advertised ip:port.
//
// To make the proxy transparent, we re-advertise the machine under the proxy's
// own address so the controller connects to us instead of the machine. We learn
// the real machine's identity by listening to its genuine broadcasts.
package discovery

import (
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const Port = 3333

// Machine is a parsed discovery record.
type Machine struct {
	Name string
	IP   string
	Port int
	Busy bool
}

func parse(s string) (Machine, bool) {
	f := strings.Split(strings.TrimSpace(s), ",")
	if len(f) < 4 {
		return Machine{}, false
	}
	port, err := strconv.Atoi(strings.TrimSpace(f[2]))
	if err != nil {
		return Machine{}, false
	}
	return Machine{Name: f[0], IP: f[1], Port: port, Busy: strings.TrimSpace(f[3]) == "1"}, true
}

func (m Machine) format() string {
	busy := "0"
	if m.Busy {
		busy = "1"
	}
	return fmt.Sprintf("%s,%s,%d,%s", m.Name, m.IP, m.Port, busy)
}

// Listener watches for genuine machine broadcasts and reports the most recently
// seen machine. It binds 3333 with SO_REUSEADDR/REUSEPORT so it can coexist with
// the controller (which also binds 3333) on the same host during testing.
type Listener struct {
	mu   sync.RWMutex
	last *Machine
	seen time.Time
}

// Latest returns the most recently observed machine, or false if none yet.
func (l *Listener) Latest() (Machine, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.last == nil {
		return Machine{}, false
	}
	return *l.last, true
}

// Listen blocks, reading machine broadcasts until the conn is closed. Records
// whose name matches selfName are ignored so the proxy never learns from its
// own re-advertisements.
func (l *Listener) Listen(conn *net.UDPConn, selfName string) {
	buf := make([]byte, 256)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		m, ok := parse(string(buf[:n]))
		if !ok || m.Name == selfName {
			continue
		}
		l.mu.Lock()
		first := l.last == nil
		l.last = &m
		l.seen = time.Now()
		l.mu.Unlock()
		if first {
			log.Printf("discovery: learned machine %q at %s:%d (busy=%v)", m.Name, m.IP, m.Port, m.Busy)
		}
	}
}

// Advertiser re-broadcasts a machine record pointing at the proxy. advertise()
// is what the controller will see and connect to.
type Advertiser struct {
	Listener   *Listener
	ProxyIP    string // address the controller should connect to (this host)
	ProxyPort  int    // proxy's TCP listen port
	NameSuffix string // appended to the real machine name, e.g. " (proxy)"
}

// Run broadcasts once per second to the given broadcast address until stop is
// closed. It only advertises once a real machine has been learned, copying its
// busy flag through.
func (a *Advertiser) Run(broadcast string, stop <-chan struct{}) error {
	dst, err := net.ResolveUDPAddr("udp4", net.JoinHostPort(broadcast, strconv.Itoa(Port)))
	if err != nil {
		return err
	}
	conn, err := net.DialUDP("udp4", nil, dst)
	if err != nil {
		return err
	}
	defer conn.Close()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return nil
		case <-ticker.C:
			m, ok := a.Listener.Latest()
			if !ok {
				continue
			}
			adv := Machine{
				Name: m.Name + a.NameSuffix,
				IP:   a.ProxyIP,
				Port: a.ProxyPort,
				Busy: m.Busy,
			}
			if _, err := conn.Write([]byte(adv.format())); err != nil {
				log.Printf("discovery: advertise write failed: %v", err)
			}
		}
	}
}

// OpenListenSocket binds the discovery port for reading, allowing address reuse
// so it can run alongside other listeners on the same host.
func OpenListenSocket() (*net.UDPConn, error) {
	addr := &net.UDPAddr{IP: net.IPv4zero, Port: Port}
	return net.ListenUDP("udp4", addr)
}

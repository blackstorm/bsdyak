package dns

import (
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	mdns "github.com/miekg/dns"
)

// Server is an embedded lightweight per-project DNS server listening on the
// project VNET bridge gateway IP (port 53), resolving "<service>" and
// "<service>.<project>.yak." to active jail IPs and forwarding other queries upstream.
type Server struct {
	project  string
	upstream string

	mu      sync.RWMutex
	records map[string]net.IP

	udpSrv *mdns.Server
	tcpSrv *mdns.Server
	addr   string
}

func StartServer(listenAddr, project, upstream string) (*Server, error) {
	if upstream == "" {
		upstream = discoverHostUpstreamDNS()
	}
	s := &Server{
		project:  strings.ToLower(project),
		upstream: upstream,
		records:  make(map[string]net.IP),
	}

	mux := mdns.NewServeMux()
	mux.HandleFunc(".", s.handleDNS)

	pc, err := net.ListenPacket("udp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("dns listen udp %s: %w", listenAddr, err)
	}
	actualAddr := pc.LocalAddr().String()
	s.addr = actualAddr

	udpReady := make(chan struct{})
	s.udpSrv = &mdns.Server{
		PacketConn:        pc,
		Handler:           mux,
		NotifyStartedFunc: func() { close(udpReady) },
	}
	go func() {
		_ = s.udpSrv.ActivateAndServe()
	}()

	ln, err := net.Listen("tcp", actualAddr)
	if err == nil {
		tcpReady := make(chan struct{})
		s.tcpSrv = &mdns.Server{
			Listener:          ln,
			Handler:           mux,
			NotifyStartedFunc: func() { close(tcpReady) },
		}
		go func() {
			_ = s.tcpSrv.ActivateAndServe()
		}()
		select {
		case <-tcpReady:
		case <-time.After(2 * time.Second):
		}
	}

	select {
	case <-udpReady:
	case <-time.After(2 * time.Second):
	}
	return s, nil
}

func (s *Server) Addr() string {
	return s.addr
}

func (s *Server) SetRecord(service, ipStr string) error {
	ip := net.ParseIP(ipStr).To4()
	if ip == nil {
		return fmt.Errorf("invalid IPv4 address %q for service %q", ipStr, service)
	}
	s.mu.Lock()
	s.records[strings.ToLower(service)] = ip
	s.mu.Unlock()
	return nil
}

func (s *Server) RemoveRecord(service string) {
	s.mu.Lock()
	delete(s.records, strings.ToLower(service))
	s.mu.Unlock()
}

func (s *Server) lookupLocal(qname string) (net.IP, bool) {
	name := strings.TrimSuffix(strings.ToLower(qname), ".")
	if name == "" {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	if ip, ok := s.records[name]; ok {
		return ip, true
	}
	suffix := "." + s.project + ".yak"
	if strings.HasSuffix(name, suffix) {
		svc := strings.TrimSuffix(name, suffix)
		if ip, ok := s.records[svc]; ok {
			return ip, true
		}
	}
	return nil, false
}

func (s *Server) handleDNS(w mdns.ResponseWriter, r *mdns.Msg) {
	msg := new(mdns.Msg)
	msg.SetReply(r)
	msg.Authoritative = true

	if len(r.Question) == 0 {
		_ = w.WriteMsg(msg)
		return
	}

	q := r.Question[0]
	if ip, ok := s.lookupLocal(q.Name); ok {
		if q.Qtype == mdns.TypeA || q.Qtype == mdns.TypeANY {
			msg.Answer = append(msg.Answer, &mdns.A{
				Hdr: mdns.RR_Header{
					Name:   q.Name,
					Rrtype: mdns.TypeA,
					Class:  mdns.ClassINET,
					Ttl:    5,
				},
				A: ip,
			})
		}
		_ = w.WriteMsg(msg)
		return
	}

	if s.upstream != "" {
		client := &mdns.Client{Timeout: 3 * time.Second}
		resp, _, err := client.Exchange(r, s.upstream)
		if err == nil && resp != nil {
			resp.Id = r.Id
			_ = w.WriteMsg(resp)
			return
		}
	}

	msg.Rcode = mdns.RcodeNameError
	_ = w.WriteMsg(msg)
}

func (s *Server) Close() error {
	var err error
	if s.udpSrv != nil {
		err = s.udpSrv.Shutdown()
	}
	if s.tcpSrv != nil {
		_ = s.tcpSrv.Shutdown()
	}
	return err
}

func discoverHostUpstreamDNS() string {
	data, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return "1.1.1.1:53"
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 && fields[0] == "nameserver" {
			ip := net.ParseIP(fields[1])
			if ip != nil && !ip.IsLoopback() {
				return net.JoinHostPort(fields[1], "53")
			}
		}
	}
	return "1.1.1.1:53"
}

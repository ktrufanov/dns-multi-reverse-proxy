/*
Binary dns_reverse_proxy is a DNS reverse proxy to route queries to DNS servers.

To illustrate, imagine an HTTP reverse proxy but for DNS.
It listens on both TCP/UDP IPv4/IPv6 on specified port.
Since the upstream servers will not see the real client IPs but the proxy,
you can specify a list of IPs allowed to transfer (AXFR/IXFR).

Example usage:

	$ go run dns_reverse_proxy.go -address :53 \
	        -default 8.8.8.8:53 \
	        -route .example.com.=8.8.4.4:53 \
	        -route .example2.com.=8.8.4.4:53,1.1.1.1:53 \
	        -allow-transfer 1.2.3.4,::1

A query for example.net or example.com will go to 8.8.8.8:53, the default.
However, a query for subdomain.example.com will go to 8.8.4.4:53. -default
is optional - if it is not given then the server will return a failure for
queries for domains where a route has not been given.
*/
package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/miekg/dns"
)

type flagStringList []string

func (i *flagStringList) String() string {
	return fmt.Sprint(*i)
}

func (i *flagStringList) Set(value string) error {
	*i = append(*i, value)
	return nil
}

var (
	address = flag.String("address", ":53", "Address to listen to (TCP and UDP)")

	defaultServer = flag.String("default", "",
		"Default DNS server where to send queries if no route matched (host:port)")

	routeLists flagStringList
	routes     map[string][]string

	allowTransfer = flag.String("allow-transfer", "",
		"List of IPs allowed to transfer (AXFR/IXFR)")
	transferIPs []string
)

func init() {
	rand.Seed(time.Now().Unix())
	flag.Var(&routeLists, "route", "List of routes where to send queries (domain=host:port,[host:port,...])")
}

func main() {
	flag.Parse()

	transferIPs = strings.Split(*allowTransfer, ",")
	routes = make(map[string][]string)
	for _, routeList := range routeLists {
		s := strings.SplitN(routeList, "=", 2)
		if len(s) != 2 || len(s[0]) == 0 || len(s[1]) == 0 {
			log.Fatal("invalid -route, must be domain=host:port,[host:port,...]")
		}
		var backends []string
		for _, backend := range strings.Split(s[1], ",") {
			if !validHostPort(backend) {
				log.Fatalf("invalid host:port for %v", backend)
			}
			backends = append(backends, backend)
		}
		if !strings.HasSuffix(s[0], ".") {
			s[0] += "."
		}
		routes[strings.ToLower(s[0])] = backends
	}

	udpServer := &dns.Server{Addr: *address, Net: "udp"}
	tcpServer := &dns.Server{Addr: *address, Net: "tcp"}
	dns.HandleFunc(".", route)
	go func() {
		if err := udpServer.ListenAndServe(); err != nil {
			log.Fatal(err)
		}
	}()
	go func() {
		if err := tcpServer.ListenAndServe(); err != nil {
			log.Fatal(err)
		}
	}()

	// Wait for SIGINT or SIGTERM
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs

	udpServer.Shutdown()
	tcpServer.Shutdown()
}

func validHostPort(s string) bool {
	host, port, err := net.SplitHostPort(s)
	if err != nil || host == "" || port == "" {
		return false
	}
	return true
}

func route(w dns.ResponseWriter, req *dns.Msg) {
	if len(req.Question) == 0 || !allowed(w, req) {
		dns.HandleFailed(w, req)
		return
	}

	lcName := strings.ToLower(req.Question[0].Name)
	var finishResp *dns.Msg
	finishResp = nil
	for name, addrs := range routes {
		if strings.HasSuffix(lcName, name) {
			addr := addrs[0]
			collectedAddrs := map[string]bool{}

			for n := range addrs {
				addr = addrs[n]

				resp, err := proxy(addr, w, req)
				if err != nil {
					dns.HandleFailed(w, req)
				}
				if resp != nil {
					if finishResp == nil {
						finishResp = resp
						for _, d := range resp.Answer {
							find := strings.Split(d.String(), "\t")[4]
							collectedAddrs[find] = true
						}
					} else {
						for _, d := range resp.Answer {
							find := strings.Split(d.String(), "\t")[4]
							if _, ok := collectedAddrs[find]; !ok {
								collectedAddrs[find] = true
								finishResp.Answer = append(finishResp.Answer, d)
							}
						}
					}
				}
			}
			w.WriteMsg(finishResp)
			return
		}
	}

	if *defaultServer == "" {
		dns.HandleFailed(w, req)
		return
	}

	resp, err := proxy(*defaultServer, w, req)
	if err != nil {
		dns.HandleFailed(w, req)
	}
	if resp != nil {
		w.WriteMsg(resp)
	}
}

func isTransfer(req *dns.Msg) bool {
	for _, q := range req.Question {
		switch q.Qtype {
		case dns.TypeIXFR, dns.TypeAXFR:
			return true
		}
	}
	return false
}

func allowed(w dns.ResponseWriter, req *dns.Msg) bool {
	if !isTransfer(req) {
		return true
	}
	remote, _, _ := net.SplitHostPort(w.RemoteAddr().String())
	for _, ip := range transferIPs {
		if ip == remote {
			return true
		}
	}
	return false
}

func proxy(addr string, w dns.ResponseWriter, req *dns.Msg) (*dns.Msg, error) {
	transport := "udp"
	if _, ok := w.RemoteAddr().(*net.TCPAddr); ok {
		transport = "tcp"
	}
	if isTransfer(req) {
		if transport != "tcp" {
			return nil, fmt.Errorf("trnasfer only by tcp")
		}
		t := new(dns.Transfer)
		c, err := t.In(req, addr)
		if err != nil {
			return nil, err
		}
		if err = t.Out(w, req, c); err != nil {
			return nil, err
		}
		return nil, nil
	}
	c := &dns.Client{Net: transport}
	resp, _, err := c.Exchange(req, addr)
	if err != nil {
		return nil, err
	}

	//w.WriteMsg(resp)
	return resp, nil
}

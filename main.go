package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

var (
	ZONE         = os.Getenv("ZONE")
	IPINFO_TOKEN = os.Getenv("IPINFO_TOKEN")
	DNS_HOST     = os.Getenv("DNS_HOST")
)

type AddressInfo struct {
	Country string `json:"country"`
	Org     string `json:"org"`
	Ip      string `json:"ip"`
}

type OneToOneEventBus struct {
	mu     sync.Mutex
	topics map[int]chan *AddressInfo
}

func (eb *OneToOneEventBus) Publish(topic int, addrInfo *AddressInfo) error {
	eb.mu.Lock()
	fmt.Println("Pub to", topic)
	defer eb.mu.Unlock()
	ch, here := eb.topics[topic]
	if !here {
		return fmt.Errorf("Topic %s does not exist yet.", topic)
	}
	ch <- addrInfo
	return nil
}

func (eb *OneToOneEventBus) Subscribe(topic int) chan *AddressInfo {
	eb.mu.Lock()
	defer eb.mu.Unlock()
	_, here := eb.topics[topic]
	if !here {
		fmt.Println("Created topic", topic)
		eb.topics[topic] = make(chan *AddressInfo, 20)
	}
	fmt.Println("Subscribed to topic", topic)
	return eb.topics[topic]
}

func NewOneToOneEB() *OneToOneEventBus {
	return &OneToOneEventBus{
		topics: make(map[int]chan *AddressInfo),
	}
}

func (eb *OneToOneEventBus) Close(topic int) error {
	eb.mu.Lock()
	fmt.Println("Closing", topic)
	defer eb.mu.Unlock()
	ch, here := eb.topics[topic]
	if !here {
		return fmt.Errorf("Topic %d does not exist yet.", topic)
	}
	close(ch)
	delete(eb.topics, topic)
	return nil
}

var (
	bus       = NewOneToOneEB()
	addrCache sync.Map
)

func main() {
	// DNS Server
	dnsServer := &dns.Server{Addr: DNS_HOST, Net: "udp"}
	dns.HandleFunc(ZONE, handleRequest)
	go dnsServer.ListenAndServe()

	// HTTP Server
	pwd, _ := os.Getwd()
	http.Handle("/", http.FileServer(http.Dir(pwd+"/static")))
	http.HandleFunc("/results/", sseHandler)
	http.ListenAndServe(":8080", nil)
}

func sseHandler(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher := w.(http.Flusher)
	encoder := json.NewEncoder(w)

	uid := rand.Intn(100000000000)

	w.Write([]byte("data: "))
	encoder.Encode(struct {
		Uid int `json:"uid"`
	}{uid})
	w.Write([]byte("\n\n"))
	flusher.Flush()

	fmt.Println("Hello", uid)
	ch := bus.Subscribe(uid)
	defer bus.Close(uid)

	timeout := time.After(15 * time.Second)
	for i := 0; i < 100; i++ {
		select {
		case b := <-ch:
			fmt.Println("New", b)
			w.Write([]byte("data: "))
			encoder.Encode(b)
			w.Write([]byte("\n\n"))
			flusher.Flush()
		case <-timeout:
			fmt.Println("Timeout!", uid)
			return
		}
	}
}

// Inspired by the splitHostPort function in the stdlib (url package)
func extractHostname(addr net.Addr) string {
	hostPort := addr.String()
	colon := strings.LastIndexByte(hostPort, ':')
	if colon == -1 {
		return hostPort
	}
	hostname := hostPort[:colon]
	if strings.HasPrefix(hostname, "[") && strings.HasSuffix(hostname, "]") {
		hostname = hostname[1 : len(hostname)-1]
	}
	return hostname
}

func getAddressInfo(addr string) *AddressInfo {
	url := "https://ipinfo.io/" + addr + "?token=" + IPINFO_TOKEN
	fmt.Println("GET", url)
	resp, _ := http.Get(url)
	defer resp.Body.Close()
	decoder := json.NewDecoder(resp.Body)
	addrInfo := new(AddressInfo)
	decoder.Decode(addrInfo)
	return addrInfo
}

func handleRequest(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetRcode(r, dns.RcodeNameError)
	addr := w.RemoteAddr()
	for _, q := range r.Question {
		parts := strings.Split(q.Name, ".")
		uid, _ := strconv.Atoi(parts[0])
		increment := parts[1]
		fmt.Println(uid, increment)
		go func() {
			var addrInfo *AddressInfo
			host := extractHostname(addr)
			val, ok := addrCache.Load(host)
			if !ok {
				fmt.Println("No cache for", host)
				addrInfo = getAddressInfo(host)
				addrCache.Store(host, addrInfo)
			} else {
				addrInfo = val.(*AddressInfo)
				fmt.Println("Cache hit", host)
			}
			err := bus.Publish(uid, addrInfo)
			if err != nil {
				fmt.Println("Error", err)
			}
		}()
	}
	w.WriteMsg(m)
}

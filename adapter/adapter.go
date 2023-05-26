package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/Dreamacro/clash/common/queue"
	"github.com/Dreamacro/clash/component/dialer"
	C "github.com/Dreamacro/clash/constant"

	"go.uber.org/atomic"
)

const DefaultUrl = "_clash_"

type Proxy struct {
	C.ProxyAdapter
	history sync.Map
	alive   sync.Map
	//history map[string]*queue.Queue
	//alive   map[string]*atomic.Bool
}

// Alive implements C.Proxy
func (p *Proxy) Alive(url string) bool {
	//if url == "" {
	//	for _, a := range p.alive {
	//		if a.Load() {
	//			return true
	//		}
	//	}
	//	return false
	//} else {
	//	if v, ok := p.alive[url]; ok {
	//		return v.Load()
	//	}
	//	return false
	//}
	if url == "" {
		aliveB := false
		p.alive.Range(func(key, value any) bool {
			a := value.(*atomic.Bool)
			if a.Load() {
				aliveB = true
				return false
			}
			return true
		})
		return aliveB
	} else {
		if v, ok := p.alive.Load(url); ok {
			return v.(*atomic.Bool).Load()
		}
		return false
	}
}
func (p *Proxy) StoreAlive(url string, alive bool) {
	if v, ok := p.alive.Load(url); ok {
		v.(*atomic.Bool).Store(alive)
	} else {
		p.alive.Store(url, atomic.NewBool(alive))
	}
}

// Dial implements C.Proxy
func (p *Proxy) Dial(metadata *C.Metadata) (C.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), C.DefaultTCPTimeout)
	defer cancel()
	return p.DialContext(ctx, metadata)
}

// DialContext implements C.ProxyAdapter
func (p *Proxy) DialContext(ctx context.Context, metadata *C.Metadata, opts ...dialer.Option) (C.Conn, error) {
	conn, err := p.ProxyAdapter.DialContext(ctx, metadata, opts...)
	//p.alive[metadata.Url].Store(err == nil)
	p.StoreAlive(metadata.Url, err == nil)
	return conn, err
}

// DialUDP implements C.ProxyAdapter
func (p *Proxy) DialUDP(metadata *C.Metadata) (C.PacketConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), C.DefaultUDPTimeout)
	defer cancel()
	return p.ListenPacketContext(ctx, metadata)
}

// ListenPacketContext implements C.ProxyAdapter
func (p *Proxy) ListenPacketContext(ctx context.Context, metadata *C.Metadata, opts ...dialer.Option) (C.PacketConn, error) {
	pc, err := p.ProxyAdapter.ListenPacketContext(ctx, metadata, opts...)
	//p.alive[metadata.Url].Store(err == nil)
	p.StoreAlive(metadata.Url, err == nil)
	return pc, err
}

// DelayHistories implements C.Proxy
func (p *Proxy) DelayHistories() map[string][]C.DelayHistory {

	hs := make(map[string][]C.DelayHistory)
	p.history.Range(func(key, value any) bool {
		k := key.(string)
		queue := value.(*queue.Queue).Copy()
		histories := []C.DelayHistory{}
		for _, item := range queue {
			histories = append(histories, item.(C.DelayHistory))
		}
		hs[k] = histories
		return true
	})
	//for k, _ := range p.history {
	//	queue := p.history[k].Copy()
	//	histories := []C.DelayHistory{}
	//	for _, item := range queue {
	//		histories = append(histories, item.(C.DelayHistory))
	//	}
	//	hs[k] = histories
	//}
	return hs
}

// DelayHistory implements C.Proxy
func (p *Proxy) DelayHistory(url string) []C.DelayHistory {
	histories := []C.DelayHistory{}
	if v, ok := p.history.Load(url); ok {
		queue := v.(*queue.Queue).Copy()
		for _, item := range queue {
			histories = append(histories, item.(C.DelayHistory))
		}
	}

	return histories
}

// LastDelay return last history record. if proxy is not alive, return the max value of uint16.
// implements C.Proxy
func (p *Proxy) LastDelay(url string) (delay uint16) {
	var max uint16 = 0xffff
	if !p.Alive(url) {
		return max
	}
	var last any
	if v, ok := p.history.Load(url); ok {
		last = v.(*queue.Queue).Last()
		if last == nil {
			return max
		}
	}
	history := last.(C.DelayHistory)
	if history.Delay == 0 {
		return max
	}
	return history.Delay
}

// MarshalJSON implements C.ProxyAdapter
func (p *Proxy) MarshalJSON() ([]byte, error) {
	inner, err := p.ProxyAdapter.MarshalJSON()
	if err != nil {
		return inner, err
	}

	mapping := map[string]any{}
	json.Unmarshal(inner, &mapping)
	mapping["history"] = p.DelayHistories()
	mapping["name"] = p.Name()
	mapping["udp"] = p.SupportUDP()
	return json.Marshal(mapping)
}

// URLTest get the delay for the specified URL
// implements C.Proxy
func (p *Proxy) URLTest(ctx context.Context, url string) (delay, meanDelay uint16, err error) {
	defer func() {
		p.StoreAlive(url, err == nil)
		//p.alive[url].Store(err == nil)
		record := C.DelayHistory{Time: time.Now()}
		if err == nil {
			record.Delay = delay
			record.MeanDelay = meanDelay
		}
		if v, ok := p.history.Load(url); ok {
			q := v.(*queue.Queue)
			q.Put(record)
			if q.Len() > 10 {
				q.Pop()
			}
		}
	}()

	addr, err := urlToMetadata(url)
	if err != nil {
		return
	}

	start := time.Now()
	instance, err := p.DialContext(ctx, &addr)
	if err != nil {
		return
	}
	defer instance.Close()

	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		return
	}
	req = req.WithContext(ctx)

	transport := &http.Transport{
		Dial: func(string, string) (net.Conn, error) {
			return instance, nil
		},
		// from http.DefaultTransport
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	client := http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer client.CloseIdleConnections()

	resp, err := client.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
	delay = uint16(time.Since(start) / time.Millisecond)

	resp, err = client.Do(req)
	if err != nil {
		// ignore error because some server will hijack the connection and close immediately
		return delay, 0, nil
	}
	resp.Body.Close()
	meanDelay = uint16(time.Since(start) / time.Millisecond / 2)

	return
}

func NewProxy(adapter C.ProxyAdapter) *Proxy {
	p := &Proxy{ProxyAdapter: adapter}
	p.history.Store(DefaultUrl, queue.New(10))
	p.alive.Store(DefaultUrl, atomic.NewBool(true))
	//history := map[string]*queue.Queue{DefaultUrl: queue.New(10)}
	//alive := map[string]*atomic.Bool{DefaultUrl: atomic.NewBool(true)}
	return p
}
func urlToMetadata(rawURL string) (addr C.Metadata, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return
	}

	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		default:
			err = fmt.Errorf("%s scheme not Support", rawURL)
			return
		}
	}

	addr = C.Metadata{
		Url:     rawURL,
		Host:    u.Hostname(),
		DstIP:   nil,
		DstPort: port,
	}
	return
}

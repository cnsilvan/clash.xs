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

type Proxy struct {
	C.ProxyAdapter
	history sync.Map
	alive   sync.Map
}

// Alive implements C.Proxy
func (p *Proxy) Alive(url string) bool {
	if url == "" {
		times := 0
		p.alive.Range(func(key, value any) bool {
			times = times + 1
			a := value.(*atomic.Bool)
			if a.Load() {
				times = 0
				return false
			}
			return true
		})
		return times == 0
	} else {
		if v, ok := p.alive.Load(url); ok {
			return v.(*atomic.Bool).Load()
		}
		return true
	}
}
func (p *Proxy) StoreAlive(url string, alive bool) {
	if v, ok := p.alive.LoadOrStore(url, atomic.NewBool(alive)); ok {
		v.(*atomic.Bool).Store(alive)
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
	p.StoreAlive(metadata.Url, err == nil)
	return pc, err
}

// DelayHistories implements C.Proxy
func (p *Proxy) DelayHistories() map[string][]C.DelayHistory {

	hs := make(map[string][]C.DelayHistory)
	p.history.Range(func(key, value any) bool {
		k := key.(string)
		q := value.(*queue.Queue).Copy()
		var histories []C.DelayHistory
		for _, item := range q {
			histories = append(histories, item.(C.DelayHistory))
		}
		hs[k] = histories
		return true
	})
	return hs
}
func (p *Proxy) FlatDelayHistories() []C.DelayHistory {
	hs := p.DelayHistories()
	hss := make([]C.DelayHistory, 0)
	for _, histories := range hs {
		hss = append(hss, histories...)
	}
	return hss
}

// DelayHistory implements C.Proxy
func (p *Proxy) DelayHistory(url string) []C.DelayHistory {
	histories := make([]C.DelayHistory, 0)
	if v, ok := p.history.Load(url); ok {
		q := v.(*queue.Queue).Copy()
		for _, item := range q {
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
	}
	if last == nil {
		return max
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
	mapping["history"] = p.FlatDelayHistories()
	mapping["histories"] = p.DelayHistories()
	mapping["name"] = p.Name()
	mapping["udp"] = p.SupportUDP()
	return json.Marshal(mapping)
}

// URLTest get the delay for the specified URL
// implements C.Proxy
func (p *Proxy) URLTest(ctx context.Context, url string) (delay, meanDelay uint16, err error) {
	defer func() {
		p.StoreAlive(url, err == nil)
		record := C.DelayHistory{Time: time.Now(), URL: url}
		if err == nil {
			record.Delay = delay
			record.MeanDelay = meanDelay
		}
		qu := queue.New(10)
		qu.Put(record)
		if v, ok := p.history.LoadOrStore(url, qu); ok {
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

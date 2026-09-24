// Package proxy is the reverse proxy between a target and the test cluster
// (DESIGN.md §5.2). It records every request, streams watches and injects
// faults.
package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"slices"
	"sync"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// Options configure a Proxy.
type Options struct {
	// Seed drives RequestMatcher.Fraction, so a replay injects the same
	// faults into the same requests.
	Seed int64
}

// Proxy forwards a target's traffic to the test cluster.
type Proxy struct {
	server   *http.Server
	reverse  *httputil.ReverseProxy
	url      string
	seed     int64
	inFlight sync.WaitGroup

	mu  sync.Mutex
	log []Request
	// faults are every fault the proxy was given, removed or not, in order.
	// A FaultID is an index into them.
	faults []*injectedFault
}

// Start listens on 127.0.0.1 over plain HTTP and forwards to the server cfg
// names, with cfg's credentials. The caller must call Stop.
func Start(cfg *rest.Config, opts Options) (*Proxy, error) {
	transport, err := rest.TransportFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("building the upstream transport: %w", err)
	}
	upstream, _, err := rest.DefaultServerUrlFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("reading the upstream URL: %w", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listening for the target: %w", err)
	}

	p := &Proxy{url: "http://" + listener.Addr().String(), seed: opts.Seed}
	p.reverse = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.Header.Del("Authorization")
			r.SetURL(upstream)
		},
		Transport: transport,
		// Watch responses must reach the target as they arrive.
		FlushInterval: -1,
		// The request log records the 502, which the default handler also
		// writes to stderr.
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, _ error) {
			if r.Context().Err() != nil {
				return // The target hung up, so it sees no status.
			}
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	p.server = &http.Server{Handler: http.HandlerFunc(p.serve)}
	go p.server.Serve(listener)
	return p, nil
}

// URL is the address the target sends its requests to.
func (p *Proxy) URL() string { return p.url }

// Stop closes the listener and every connection through it.
func (p *Proxy) Stop() error {
	if err := p.server.Close(); err != nil {
		return fmt.Errorf("stopping the proxy: %w", err)
	}
	p.inFlight.Wait()
	return nil
}

// Kubeconfig writes a kubeconfig that points at the proxy, names namespace and
// carries no credentials, for the launcher to hand the target.
func (p *Proxy) Kubeconfig(path, namespace string) error {
	const name = "botbox"
	config := clientcmdapi.Config{
		Clusters:       map[string]*clientcmdapi.Cluster{name: {Server: p.URL()}},
		Contexts:       map[string]*clientcmdapi.Context{name: {Cluster: name, Namespace: namespace}},
		CurrentContext: name,
	}
	if err := clientcmd.WriteToFile(config, path); err != nil {
		return fmt.Errorf("writing the kubeconfig: %w", err)
	}
	return nil
}

// Log copies the requests recorded so far. A request is appended when it
// arrives and completed when its exchange ends, so a record whose exchange is
// still in flight carries a zero Latency. The response reaches the client
// before that completion, so a caller that needs finished records waits for
// quiescence, as a settle wait does (DESIGN.md §5.5).
func (p *Proxy) Log() []Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.log)
}

// WriteLog writes the log as one JSON object per line (`requests.jsonl`,
// DESIGN.md §11).
func (p *Proxy) WriteLog(w io.Writer) error {
	encoder := json.NewEncoder(w)
	for _, request := range p.Log() {
		if err := encoder.Encode(request); err != nil {
			return fmt.Errorf("writing the request log: %w", err)
		}
	}
	return nil
}

func (p *Proxy) serve(w http.ResponseWriter, r *http.Request) {
	// Upgraded connections (exec, port-forward) carry no API requests.
	if r.Header.Get("Upgrade") != "" {
		p.reverse.ServeHTTP(w, r)
		return
	}
	p.inFlight.Add(1)
	defer p.inFlight.Done()

	request := newRequest(r)
	action := p.faultFor(request)
	if action != nil {
		request.Fault = action.String()
	}
	index := p.appendRecord(request)
	defer func() {
		p.updateRecord(index, func(r *Request) { r.Latency = time.Since(request.Start) })
	}()

	response := &recorder{ResponseWriter: w, proxy: p, index: index}
	switch action := action.(type) {
	case Error:
		writeStatus(response, action.Code, request)
		return
	case Drop:
		drop(response)
		return
	case Delay:
		if !sleep(r.Context(), action.For) {
			return
		}
	}
	p.reverse.ServeHTTP(response, r)
}

func (p *Proxy) appendRecord(request Request) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.log = append(p.log, request)
	return len(p.log) - 1
}

func (p *Proxy) updateRecord(index int, update func(*Request)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	update(&p.log[index])
}

// recorder notes the status the reverse proxy writes. Unwrap lets
// http.ResponseController reach the flusher the streaming of watches needs.
type recorder struct {
	http.ResponseWriter
	proxy *Proxy
	index int
}

func (rec *recorder) WriteHeader(status int) {
	rec.proxy.updateRecord(rec.index, func(r *Request) { r.Status = status })
	rec.ResponseWriter.WriteHeader(status)
}

func (rec *recorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

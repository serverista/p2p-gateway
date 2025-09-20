package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
	crypto "github.com/libp2p/go-libp2p/core/crypto"
	host "github.com/libp2p/go-libp2p/core/host"
	network "github.com/libp2p/go-libp2p/core/network"
	peer "github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

const ProtocolID = "/serverista-proxy/1.0.0"

// ProxyRequest is the wire request from a peer to the proxy server.
type ProxyRequest struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"` // appended to base API URL
	Headers map[string]string `json:"headers,omitempty"`
	Body    []byte            `json:"body,omitempty"` // binary payload (base64 in JSON automatically)
}

// ProxyResponse is the wire response from the server back to the peer.
type ProxyResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    []byte            `json:"body,omitempty"`
	Error   string            `json:"error,omitempty"`
}

// writeMessage writes length-prefixed JSON to w
func writeMessage(w io.Writer, v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	// length prefix uint32 big-endian
	var lenbuf [4]byte
	binary.BigEndian.PutUint32(lenbuf[:], uint32(len(b)))
	if _, err := w.Write(lenbuf[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// readMessage reads a length-prefixed JSON message into dst (expects pointer)
func readMessage(r io.Reader, dst interface{}) error {
	var lenbuf [4]byte
	if _, err := io.ReadFull(r, lenbuf[:]); err != nil {
		return err
	}
	l := binary.BigEndian.Uint32(lenbuf[:])
	if l == 0 {
		return fmt.Errorf("zero-length message")
	}
	data := make([]byte, l)
	if _, err := io.ReadFull(r, data); err != nil {
		return err
	}
	return json.Unmarshal(data, dst)
}

// proxyHandler handles incoming libp2p streams, decodes ProxyRequest, forwards to HTTP API and responds.
func proxyHandler(h host.Host, baseURL string) func(network.Stream) {
	httpClient := &http.Client{

		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			// allow high performance defaults
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,

			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, // skip certificate verification
			},
		},
	}

	return func(s network.Stream) {
		defer s.Close()
		remote := s.Conn().RemotePeer()
		log.Printf("new stream from peer %s", remote.String())

		// Use buffered reader/writer for efficiency
		br := bufio.NewReader(s)
		bw := bufio.NewWriter(s)

		var req ProxyRequest
		if err := readMessage(br, &req); err != nil {
			log.Printf("failed to read request from %s: %v", remote.String(), err)
			resp := ProxyResponse{Error: fmt.Sprintf("invalid request: %v", err)}
			_ = writeMessage(bw, resp)
			_ = bw.Flush()
			return
		}

		// Build the HTTP request
		targetURL := baseURL + req.Path
		httpReq, err := http.NewRequest(req.Method, targetURL, nil)
		if err != nil {
			log.Printf("bad request build: %v", err)
			resp := ProxyResponse{Error: fmt.Sprintf("bad request: %v", err)}
			_ = writeMessage(bw, resp)
			_ = bw.Flush()
			return
		}
		// attach body if present
		if len(req.Body) > 0 {
			httpReq.Body = io.NopCloser(io.NopCloser(bytesReader(req.Body)))
			httpReq.ContentLength = int64(len(req.Body))
		}
		// copy headers
		for k, v := range req.Headers {
			fmt.Println("Added header to request: ", k, v)
			httpReq.Header.Set(k, v)
		}

		// Do the HTTP request
		httpResp, err := httpClient.Do(httpReq)
		if err != nil {
			log.Printf("http forward error: %v", err)
			resp := ProxyResponse{Error: fmt.Sprintf("error forwarding to API: %v", err)}
			_ = writeMessage(bw, resp)
			_ = bw.Flush()
			return
		}
		defer httpResp.Body.Close()

		body, err := io.ReadAll(httpResp.Body)
		if err != nil {
			log.Printf("error reading http response: %v", err)
			resp := ProxyResponse{Error: fmt.Sprintf("error reading API response: %v", err)}
			_ = writeMessage(bw, resp)
			_ = bw.Flush()
			return
		}

		// collect headers
		respHeaders := make(map[string]string)
		for k, vs := range httpResp.Header {
			if len(vs) > 0 {
				respHeaders[k] = vs[0] // keep first value; extend if you need multi-valued headers
			}
		}

		resp := ProxyResponse{
			Status:  httpResp.StatusCode,
			Headers: respHeaders,
			Body:    body,
		}

		if err := writeMessage(bw, resp); err != nil {
			log.Printf("error writing response to %s: %v", remote.String(), err)
			return
		}
		if err := bw.Flush(); err != nil {
			log.Printf("flush error to %s: %v", remote.String(), err)
		}
		log.Printf("proxied %s %s -> %d bytes returned to %s", req.Method, req.Path, len(body), remote.String())
	}
}

// bytesReader returns an io.Reader for a byte slice (no allocations beyond slice)
func bytesReader(b []byte) io.Reader { return bytesReaderImpl{b: b} }

type bytesReaderImpl struct{ b []byte }

func (br bytesReaderImpl) Read(p []byte) (int, error) {
	if len(br.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, br.b)
	br.b = br.b[n:]
	return n, nil
}

func main() {
	listen := flag.String("listen", "/ip4/127.0.0.1/tcp/4001", "multiaddr to listen on")
	keyfile := flag.String("keyfile", "", "optional private key file (base64) — if empty a new keypair is generated")
	apiBase := flag.String("api", "", "HTTP API base URL (e.g. https://api.serverista.com) — REQUIRED")
	flag.Parse()

	if *apiBase == "" {
		log.Fatal("missing -api (HTTP API base URL). Example: -api https://api.serverista.com")
	}

	// context for shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// handle Ctrl+C / SIGTERM
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("Received signal %s, shutting down...", sig)
		cancel()
	}()

	var prv crypto.PrivKey
	var err error
	if *keyfile == "" {
		prv, _, err = crypto.GenerateKeyPair(crypto.Ed25519, -1)
		if err != nil {
			log.Fatalf("keygen failed: %v", err)
		}
		privBytes, err := crypto.MarshalPrivateKey(prv)
		if err != nil {
			log.Fatalf("failed to marshal private key: %v", err)
		}
		fmt.Println("Private key (base64):")
		fmt.Println(base64.StdEncoding.EncodeToString(privBytes))
		fmt.Println()
	} else {
		b, err := os.ReadFile(*keyfile)
		if err != nil {
			log.Fatalf("read keyfile: %v", err)
		}
		bts, err := base64.StdEncoding.DecodeString(string(b))
		if err != nil {
			log.Fatalf("read keyfile: %v", err)
		}
		priv, err := crypto.UnmarshalPrivateKey(bts)
		if err != nil {
			log.Fatalf("unmarshal key: %v", err)
		}
		prv = priv
	}

	// create libp2p host
	h, err := libp2p.New(
		libp2p.Identity(prv),
		libp2p.DisableRelay(),
		libp2p.DisableIdentifyAddressDiscovery(),
		libp2p.NoListenAddrs,
		libp2p.ListenAddrStrings(*listen),
	)
	if err != nil {
		log.Fatalf("libp2p new host failed: %v", err)
	}

	// close host when context is cancelled
	go func() {
		<-ctx.Done()
		log.Println("Closing libp2p host...")
		if err := h.Close(); err != nil {
			log.Printf("Error closing host: %v", err)
		}
	}()

	// listen multiaddr
	maddr, err := ma.NewMultiaddr(*listen)
	if err != nil {
		log.Fatalf("invalid listen multiaddr: %v", err)
	}
	if err := h.Network().Listen(maddr); err != nil {
		log.Fatalf("failed to listen on %s: %v", maddr, err)
	}

	// set stream handler
	h.SetStreamHandler(ProtocolID, proxyHandler(h, *apiBase))

	// print info
	info := host.InfoFromHost(h)
	addrs, _ := peer.AddrInfoToP2pAddrs(info)
	fmt.Printf("PeerID: %s\n", h.ID().String())
	fmt.Println("Listen addrs:")
	for _, a := range h.Addrs() {
		fmt.Println("  ", a.String())
	}
	fmt.Println("\nMultiaddrs (with peer id) you can share with clients:")
	for _, a := range addrs {
		fmt.Println("  ", a.String())
	}
	fmt.Println("\nProtocol:", ProtocolID)
	fmt.Println("HTTP API base:", *apiBase)

	// wait until context is cancelled
	<-ctx.Done()
	log.Println("Shutdown complete")
}

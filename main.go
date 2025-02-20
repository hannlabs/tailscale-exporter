package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	proping "github.com/prometheus-community/pro-bing"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"tailscale.com/client/tailscale"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/util/dnsname"
)

type arrayFlags []string

func (s arrayFlags) String() string {
	return strings.Join(s, ",")
}

func (s *arrayFlags) Set(value string) error {
	log.Printf("arrayFlags set: %s", value)
	*s = append(*s, strings.Split(value, ",")...)
	log.Printf("arrayFlags after set: %s", *s)
	return nil
}

type arguments struct {
	socketPath        *string
	pingActive        *bool
	pingTags          *arrayFlags
	pingTimeout       *time.Duration
	pingWithTailscale *bool
}

func NewArguments() arguments {
	var args arguments
	args.socketPath = flag.String("socket", "/var/run/tailscale/tailscaled.sock", "Tailscale socket path")
	args.pingActive = flag.Bool("pingActive", false, "ping active peers")
	args.pingTags = new(arrayFlags)
	flag.Var(args.pingTags, "pingTags", "ping peers with the tag")
	args.pingTimeout = flag.Duration("pingTimeout", time.Second, "ping timeout")
	args.pingWithTailscale = flag.Bool("pingWithTailscale", false, "ping with tailscale ping")
	return args
}

type metrics struct {
	scrapesCounter prometheus.Counter
	txBytes        *prometheus.GaugeVec
	rxBytes        *prometheus.GaugeVec
	pingLatency    *prometheus.GaugeVec
	directConnect  *prometheus.GaugeVec
}

func NewMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		scrapesCounter: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "tailscale_scrapes_total",
			Help: "Total number of scrapes performed",
		}),
		txBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "tailscale_network_transmit_bytes",
			Help: "Current transmitted bytes",
		}, []string{"src_ip", "src_host", "dst_ip", "dst_host"}),
		rxBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "tailscale_network_receive_bytes",
			Help: "Current received bytes",
		}, []string{"src_ip", "src_host", "dst_ip", "dst_host"}),
		pingLatency: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "tailscale_network_ping_latency",
			Help: "Ping latency in milliseconds",
		}, []string{"src_ip", "src_host", "dst_ip", "dst_host"}),
		directConnect: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "tailscale_network_direct_connect",
			Help: "Direct connection status (1 if direct, 0 otherwise)",
		}, []string{"src_ip", "src_host", "dst_ip", "dst_host"}),
	}

	reg.MustRegister(m.scrapesCounter, m.txBytes, m.rxBytes, m.pingLatency, m.directConnect)
	return m
}

func main() {

	args := NewArguments()
	flag.Parse()

	log.Printf("socketPath: %s, pingActive: %t, pingTags: %v", *args.socketPath, *args.pingActive, args.pingTags.String())

	// Initialize Tailscale client
	client := &tailscale.LocalClient{
		Socket:        *args.socketPath,
		UseSocketOnly: true,
	}

	// Create Prometheus registry
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	// Create HTTP handler with metrics collection
	handler := promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		Registry: reg,
		Timeout:  10 * time.Second,
	})

	// Wrap handler with metrics collection
	http.Handle("/metrics", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := collectMetrics(client, m, &args); err != nil {
			log.Printf("Error collecting metrics: %v", err)
		}
		handler.ServeHTTP(w, r)
	}))

	// Start metrics server
	log.Println("Starting server on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func collectMetrics(client *tailscale.LocalClient, m *metrics, args *arguments) error {
	ctx := context.Background()

	m.scrapesCounter.Inc()

	// Get local status
	status, err := client.Status(ctx)
	if err != nil {
		return fmt.Errorf("failed to get status: %w", err)
	}

	selfHost := dnsname.SanitizeHostname(status.Self.HostName)

	selfIP := ""
	if len(status.Self.TailscaleIPs) > 0 {
		selfIP = status.Self.TailscaleIPs[0].String()
	}

	// Reset metrics before each collection
	m.txBytes.Reset()
	m.rxBytes.Reset()
	m.pingLatency.Reset()
	m.directConnect.Reset()

	var wg sync.WaitGroup
	mu := &sync.Mutex{}

	// Process each peer
	for _, peer := range status.Peer {

		peerHost := dnsname.SanitizeHostname(peer.HostName)

		peerIP := ""
		if len(peer.TailscaleIPs) > 0 {
			peerIP = peer.TailscaleIPs[0].String()
		}

		labels := prometheus.Labels{
			"src_ip":   selfIP,
			"src_host": selfHost,
			"dst_ip":   peerIP,
			"dst_host": peerHost,
		}

		var direct int
		if peer.CurAddr != "" {
			direct = 1
		}

		// Set connection metrics
		m.txBytes.With(labels).Add(float64(peer.TxBytes))
		m.rxBytes.With(labels).Add(float64(peer.RxBytes))
		m.directConnect.With(labels).Set(float64(direct))

		var pingPeer bool = false

		if *args.pingActive && peer.Active {
			pingPeer = true
			log.Printf("peer %s is active, ping it", peerHost)
		}

		if !pingPeer && peer.Tags != nil {
			for i := range peer.Tags.LenIter() {
				peerTag := peer.Tags.At(i)

				if slices.Contains(*args.pingTags, peerTag) {
					pingPeer = true
				}
			}
		}

		// Concurrent ping measurements
		if pingPeer {
			wg.Add(1)
			go func(p *ipnstate.PeerStatus) {
				defer wg.Done()

				ip, err := netip.ParseAddr(p.TailscaleIPs[0].String())
				if err != nil {
					return
				}

				var latency int64 = -1

				// Timeout ping after 2 seconds
				pingCtx, cancel := context.WithTimeout(ctx, *args.pingTimeout)
				defer cancel()


				if *args.pingWithTailscale {


					result, err := client.Ping(pingCtx, ip, tailcfg.PingICMP)
					if err == nil {
						latency = time.Duration(result.LatencySeconds * float64(time.Second)).Milliseconds()
					}

				} else {
					pinger, err := proping.NewPinger(ip.String())
					if err != nil {
						return
					}
					pinger.Timeout = *args.pingTimeout
					pinger.Count = 1
					
					if err = pinger.RunWithContext(pingCtx); err == nil {
						stats := pinger.Statistics()
						if stats.PacketsRecv > 0 {
							latency = int64(stats.AvgRtt.Milliseconds())
						}
					}
				}

				mu.Lock()
				m.pingLatency.With(labels).Set(float64(latency))
				mu.Unlock()
			}(peer)
		}
	}

	wg.Wait()
	return nil
}

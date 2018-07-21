// Copyright 2016 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Binary dnsmasq_exporter is a Prometheus exporter for dnsmasq statistics.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/log"
)

var (
	listen = flag.String("listen",
		"localhost:9153",
		"listen address")

	leasesPath = flag.String("leases_path",
		"/var/lib/misc/dnsmasq.leases",
		"path to the dnsmasq leases file")

	dnsmasqAddr = flag.String("dnsmasq",
		"localhost:53",
		"dnsmasq host:port address")
	metricsPath = flag.String("metrics_path",
		"/metrics",
		"path under which metrics are served")
)

var (
	// floatMetrics contains prometheus Gauges, keyed by the stats DNS record
	// they correspond to.
	floatMetrics = map[string]prometheus.Gauge{
		"cachesize.bind.": prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dnsmasq_cachesize",
			Help: "configured size of the DNS cache",
		}),

		"insertions.bind.": prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dnsmasq_insertions",
			Help: "DNS cache insertions",
		}),

		"evictions.bind.": prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dnsmasq_evictions",
			Help: "DNS cache exictions: numbers of entries which replaced an unexpired cache entry",
		}),

		"misses.bind.": prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dnsmasq_misses",
			Help: "DNS cache misses: queries which had to be forwarded",
		}),

		"hits.bind.": prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dnsmasq_hits",
			Help: "DNS queries answered locally (cache hits)",
		}),

		"auth.bind.": prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dnsmasq_auth",
			Help: "DNS queries for authoritative zones",
		}),
	}

	leases = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "dnsmasq_leases",
		Help: "Number of DHCP leases handed out",
	})
	leaseExpiry = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "dnsmasq_lease_expiry",
			Help: "Time of lease expiry, in epoch time (seconds since 1970)",
		},
		[]string{
			"mac_address",
			"ip_address",
			"computer_name",
			"client_id",
		},
	)
)

func init() {
	for _, g := range floatMetrics {
		prometheus.MustRegister(g)
	}
	prometheus.MustRegister(leases)
	prometheus.MustRegister(leaseExpiry)
}

// From https://manpages.debian.org/stretch/dnsmasq-base/dnsmasq.8.en.html:
// The cache statistics are also available in the DNS as answers to queries of
// class CHAOS and type TXT in domain bind. The domain names are cachesize.bind,
// insertions.bind, evictions.bind, misses.bind, hits.bind, auth.bind and
// servers.bind. An example command to query this, using the dig utility would
// be:
//     dig +short chaos txt cachesize.bind

type server struct {
	promHandler http.Handler
	dnsClient   *dns.Client
	dnsmasqAddr string
	leasesPath  string
}

func (s *server) metrics(w http.ResponseWriter, r *http.Request) {
	var eg errgroup.Group

	eg.Go(func() error {
		msg := &dns.Msg{
			MsgHdr: dns.MsgHdr{
				Id:               dns.Id(),
				RecursionDesired: true,
			},
			Question: []dns.Question{
				dns.Question{"cachesize.bind.", dns.TypeTXT, dns.ClassCHAOS},
				dns.Question{"insertions.bind.", dns.TypeTXT, dns.ClassCHAOS},
				dns.Question{"evictions.bind.", dns.TypeTXT, dns.ClassCHAOS},
				dns.Question{"misses.bind.", dns.TypeTXT, dns.ClassCHAOS},
				dns.Question{"hits.bind.", dns.TypeTXT, dns.ClassCHAOS},
				dns.Question{"auth.bind.", dns.TypeTXT, dns.ClassCHAOS},
				dns.Question{"servers.bind.", dns.TypeTXT, dns.ClassCHAOS},
			},
		}
		in, _, err := s.dnsClient.Exchange(msg, s.dnsmasqAddr)
		if err != nil {
			return err
		}
		for _, a := range in.Answer {
			txt, ok := a.(*dns.TXT)
			if !ok {
				continue
			}
			switch txt.Hdr.Name {
			case "servers.bind.":
				// TODO: parse <server> <successes> <errors>, also with multiple upstreams
			default:
				g, ok := floatMetrics[txt.Hdr.Name]
				if !ok {
					continue // ignore unexpected answer from dnsmasq
				}
				if got, want := len(txt.Txt), 1; got != want {
					return fmt.Errorf("stats DNS record %q: unexpected number of replies: got %d, want %d", txt.Hdr.Name, got, want)
				}
				f, err := strconv.ParseFloat(txt.Txt[0], 64)
				if err != nil {
					return err
				}
				g.Set(f)
			}
		}
		return nil
	})

	eg.Go(func() error {
		f, err := os.Open(s.leasesPath)
		if err != nil {
			log.Warnln("could not open leases file:", err)
			return err
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		var lines float64
		leaseExpiry.Reset()
		for scanner.Scan() {
			lines++

			// http://lists.thekelleys.org.uk/pipermail/dnsmasq-discuss/2016q2/010595.html
			// http://thekelleys.org.uk/gitweb/?p=dnsmasq.git;a=blob;f=src/lease.c;hb=v2.79#l243
			// https://serverfault.com/a/786141/58240
			// https://github.com/Illizian/dnsmasq-leases
			parts := strings.Fields(scanner.Text())
			if parts[0] == "duid" {
				// TODO DHCPv6 support (once we hit "duid", all following records are DHCPv6 in a slightly different format)
				// duid SERVER-DUID\n
				// EXPIRY IAID IPv6 HOST CLIENT-DUID
				// ...
				break
			}
			if len(parts) < 5 {
				// TODO decide what to do for malformed/incomplete records
				continue
			}
			expiry, err := strconv.ParseFloat(parts[0], 64)
			if err != nil {
				expiry = -1
			}
			leaseExpiry.With(prometheus.Labels{
				"mac_address":   parts[1],
				"ip_address":    parts[2],
				"computer_name": parts[3],
				"client_id":     parts[4],
			}).Set(expiry)
		}
		if err := scanner.Err(); err != nil {
			return err
		}
		leases.Set(lines)
		return nil
	})

	if err := eg.Wait(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.promHandler.ServeHTTP(w, r)
}

func main() {
	flag.Parse()
	s := &server{
		promHandler: promhttp.Handler(),
		dnsClient: &dns.Client{
			SingleInflight: true,
		},
		dnsmasqAddr: *dnsmasqAddr,
		leasesPath:  *leasesPath,
	}
	http.HandleFunc(*metricsPath, s.metrics)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
			<head><title>Dnsmasq Exporter</title></head>
			<body>
			<h1>Dnsmasq Exporter</h1>
			<p><a href="` + *metricsPath + `">Metrics</a></p>
			</body></html>`))
	})
	log.Infoln("Listening on", *listen)
	log.Infoln("Serving metrics under", *metricsPath)
	log.Fatal(http.ListenAndServe(*listen, nil))
}

// The MIT License (MIT)
//
// Copyright (c) 2018-2019 Cranky Kernel
//
// Permission is hereby granted, free of charge, to any person
// obtaining a copy of this software and associated documentation
// files (the "Software"), to deal in the Software without
// restriction, including without limitation the rights to use, copy,
// modify, merge, publish, distribute, sublicense, and/or sell copies
// of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be
// included in all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND,
// EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF
// MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND
// NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS
// BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN
// ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN
// CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/gobuffalo/packr"
	"github.com/gorilla/mux"
	"gitlab.com/crankykernel/cryptoxscanner/binance"
	"gitlab.com/crankykernel/cryptoxscanner/log"
	"gitlab.com/crankykernel/cryptoxscanner/version"
	"math/rand"
	"net/http"
	_ "net/http/pprof"
	"time"
)

var salt []byte

func init() {
	rand.Seed(time.Now().UnixNano())
	salt = make([]byte, 256)
	rand.Read(salt)
}

type Options struct {
	Port uint16
}

var static packr.Box

func ServerMain(options Options) {

	// Start the Binance runner. This is a little bit of a message as the
	// socket can subscribe to specific symbol feeds directly. This should be
	// abstracted with some sort of broker.
	binanceFeed := NewBinanceRunner()
	binanceWebSocketHandler := NewBroadcastWebSocketHandler()
	binanceFeed.websocket = binanceWebSocketHandler
	binanceWebSocketHandler.Feed = binanceFeed
	go binanceFeed.Run()

	router := mux.NewRouter()

	router.HandleFunc("/ws/binance/live", binanceWebSocketHandler.Handle)
	router.HandleFunc("/ws/binance/monitor", binanceWebSocketHandler.Handle)
	router.HandleFunc("/ws/binance/symbol", binanceWebSocketHandler.Handle)

	router.PathPrefix("/api/1/binance/proxy").Handler(binance.NewApiProxy())

	router.HandleFunc("/api/1/ping", pingHandler)
	router.HandleFunc("/api/1/status/websockets", webSocketsStatusHandler)

	router.Handle("/api/1/binance/volume", NewVolumeHandler(binanceFeed))

	static := packr.NewBox("../../webapp/dist")
	staticServer := http.FileServer(static)

	router.PathPrefix("/").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !static.Has(r.URL.Path) {
			r.URL.Path = "/"
		}
		staticServer.ServeHTTP(w, r)
	})

	go func() {
		err := http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", options.Port+1), nil)
		if err != nil {
			log.Printf("error: failed to start debug server: %v\n", err)
		}
	}()
	log.Printf("Starting server on port %d.", options.Port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", options.Port), router))
}

type VolumeHandler struct {
	binanceRunner *BinanceRunner
}

func NewVolumeHandler(binanceRunner *BinanceRunner) *VolumeHandler {
	return &VolumeHandler{
		binanceRunner: binanceRunner,
	}
}

func (h *VolumeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	lastTracker := h.binanceRunner.GetCache()
	data := map[string]interface{}{}
	for symbol := range lastTracker.Trackers {
		tracker := lastTracker.Trackers[symbol]
		ticker := map[string]interface{}{}
		ticker["nvh"] = lastTracker.Trackers[symbol].Histogram.NetVolume
		ticker["vol"] = tracker.LastTick().TotalQuoteVolume
		ticker["priceChange1h"] = tracker.Metrics[60].PriceChangePercent
		ticker["nv60"] = tracker.Metrics[60].NetVolume
		ticker["v60"] = tracker.Metrics[60].TotalVolume
		data[symbol] = ticker
	}
	response := map[string]interface{}{
		"data": data,
	}
	encoder := json.NewEncoder(w)
	encoder.Encode(response)
}

func buildUpdateMessage(tracker *TickerTracker) map[string]interface{} {
	last := tracker.LastTick()
	key := tracker.Symbol

	message := map[string]interface{}{
		"symbol": key,
		"close":  last.CurrentDayClose,
		"bid":    last.Bid,
		"ask":    last.Ask,
		"high":   last.HighPrice,
		"low":    last.LowPrice,
		"volume": last.TotalQuoteVolume,

		"price_change_pct": map[string]float64{
			"1m":  tracker.Metrics[1].PriceChangePercent,
			"5m":  tracker.Metrics[5].PriceChangePercent,
			"10m": tracker.Metrics[10].PriceChangePercent,
			"15m": tracker.Metrics[15].PriceChangePercent,
			"1h":  tracker.Metrics[60].PriceChangePercent,
			"24h": tracker.LastTick().PriceChangePercent,
		},

		"volume_change_pct": map[string]float64{
			"1m":  tracker.Metrics[1].VolumeChangePercent,
			"2m":  tracker.Metrics[2].VolumeChangePercent,
			"3m":  tracker.Metrics[3].VolumeChangePercent,
			"4m":  tracker.Metrics[4].VolumeChangePercent,
			"5m":  tracker.Metrics[5].VolumeChangePercent,
			"10m": tracker.Metrics[10].VolumeChangePercent,
			"15m": tracker.Metrics[15].VolumeChangePercent,
			"1h":  tracker.Metrics[60].VolumeChangePercent,
		},

		"timestamp": last.Timestamp(),
	}

	for _, bucket := range Buckets {
		metrics := tracker.Metrics[bucket]

		message[fmt.Sprintf("l_%d", bucket)] = metrics.Low
		message[fmt.Sprintf("h_%d", bucket)] = metrics.High

		message[fmt.Sprintf("r_%d", bucket)] = metrics.Range
		message[fmt.Sprintf("rp_%d", bucket)] = metrics.RangePercent
	}

	message["r_24"] = tracker.H24Metrics.Range
	message["rp_24"] = tracker.H24Metrics.RangePercent

	return message
}

func pingHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("content-type", "application/json")
	encoder := json.NewEncoder(w)
	if err := encoder.Encode(map[string]interface{}{
		"version":     version.BuildNumberAsInt(),
		"buildNumber": version.BuildNumberAsInt(),
	}); err != nil {
		log.WithError(err).WithField("handler", "ping").
			Errorf("Failed to encode response to JSON")
	}
}

func webSocketsStatusHandler(w http.ResponseWriter, r *http.Request) {
	wsConnectionTracker.Lock.RLock()
	defer wsConnectionTracker.Lock.RUnlock()

	paths := map[string]int{}

	for path := range wsConnectionTracker.Paths {
		count := len(wsConnectionTracker.Paths[path])
		if count > 0 {
			paths[path] += count
		}
	}

	clients := make(map[string][]string)

	for client := range wsConnectionTracker.Clients {

		// Instead of using the actual remote address we use a hash of it
		// as we may be running without password protection and don't want
		// to expose users IP addresses.
		hash := sha256.New()
		hash.Write([]byte(client.GetRemoteHost()))
		hash.Write(salt)
		remoteAddr := hex.EncodeToString(hash.Sum(nil))[0:8]

		for path := range wsConnectionTracker.Clients[client] {
			clients[remoteAddr] = append(
				clients[remoteAddr], path)
		}
	}

	encoder := json.NewEncoder(w)
	if err := encoder.Encode(map[string]interface{}{
		"paths":   paths,
		"clients": clients,
	}); err != nil {
		log.WithError(err).WithField("handler", "ws-status").
			Errorf("Failed to encode response to JSON")
	}
}

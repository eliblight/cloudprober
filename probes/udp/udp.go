// Copyright 2017 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/*
Package udp implements a UDP prober. It sends UDP queries to a list of
targets and reports statistics on queries sent, queries received, and latency
experienced.

Queries to each target are sent in parallel.
*/
package udp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/google/cloudprober/logger"
	"github.com/google/cloudprober/message"
	"github.com/google/cloudprober/metrics"
	"github.com/google/cloudprober/probes/options"
	"github.com/google/cloudprober/probes/probeutils"
	udpsrv "github.com/google/cloudprober/servers/udp"
	"github.com/google/cloudprober/sysvars"
)

const (
	maxMsgSize = 65536
)

// Probe holds aggregate information about all probe runs, per-target.
type Probe struct {
	name string
	opts *options.Options
	src  string
	c    *ProbeConf
	l    *logger.Logger

	// List of UDP connections to use.
	connList []*net.UDPConn
	numConn  int32
	runID    uint64

	// List of targets for a probe iteration.
	targets []string
	// map target name to flow state.
	fsm *message.FlowStateMap

	// Results by target. Uses mutex as send, recv threads modify it concurrently.
	res map[string]*probeRunResult
	mu  sync.Mutex
}

// probeRunResult captures the results of a single probe run. The way we work with
// stats makes sure that probeRunResult and its fields are not accessed concurrently
// (see documentation with statsKeeper below). That's the reason we use metrics.Int
// types instead of metrics.AtomicInt.
type probeRunResult struct {
	target   string
	total    metrics.Int
	success  metrics.Int
	latency  metrics.Float
	timeouts metrics.Int
	delayed  metrics.Int
}

// Target returns the p.target.
func (prr probeRunResult) Target() string {
	return prr.target
}

// Metrics converts probeRunResult into metrics.EventMetrics object
func (prr probeRunResult) Metrics() *metrics.EventMetrics {
	return metrics.NewEventMetrics(time.Now()).
		AddMetric("total", &prr.total).
		AddMetric("success", &prr.success).
		AddMetric("latency", &prr.latency).
		AddMetric("timeouts", &prr.timeouts).
		AddMetric("delayed", &prr.delayed)
}

// Init initializes the probe with the given params.
func (p *Probe) Init(name string, opts *options.Options) error {
	c, ok := opts.ProbeConf.(*ProbeConf)
	if !ok {
		return errors.New("not a UDP config")
	}
	p.name = name
	p.opts = opts
	if p.l = opts.Logger; p.l == nil {
		p.l = &logger.Logger{}
	}
	p.src = sysvars.Vars()["hostname"]
	p.c = c
	p.fsm = message.NewFlowStateMap()
	p.res = make(map[string]*probeRunResult)

	// For one-way connections, we use a pool of sockets.
	wantConn := p.c.GetNumTxPorts()
	triesRemaining := wantConn * 2
	p.numConn = 0
	p.connList = make([]*net.UDPConn, wantConn)
	for p.numConn < wantConn && triesRemaining > 0 {
		triesRemaining--
		udpConn, err := udpsrv.Listen(0, p.l)
		if err != nil {
			p.l.Warningf("Opening UDP socket failed: %v", err)
			continue
		}
		p.l.Infof("UDP socket id %d, addr %v", p.numConn, udpConn.LocalAddr())
		p.connList[p.numConn] = udpConn
		p.numConn++
	}
	if p.numConn < wantConn {
		for _, c := range p.connList {
			c.Close()
		}
		return fmt.Errorf("UDP socket creation failed: got %d connections, want %d", p.numConn, wantConn)
	}
	return nil
}

// initProbeRunResults empties the current probe results objects, gets a list of
// targets and builds a new result object for each target.
func (p *Probe) initProbeRunResults() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k := range p.res {
		delete(p.res, k)
	}
	for _, target := range p.targets {
		p.res[target] = &probeRunResult{
			target: target,
		}
	}
}

// flushProbeRunResults outputs results for the probe run to the resultsChan.
func (p *Probe) flushProbeRunResults(resultsChan chan<- probeutils.ProbeResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, result := range p.res {
		resultsChan <- result
	}
}

// updateSentCount is a helper function to increment total count for a target.
func (p *Probe) updateSentCount(target string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	res, ok := p.res[target]
	if !ok {
		return
	}
	res.total.Inc()
}

// updateProbeResults takes a message.Result object and updates probe results.
// NOTE: if latency > timeout, only the "delayed" counter (and not latency)
// will be incremented.
func (p *Probe) updateProbeResults(msg *message.Message, rxTS time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	res, ok := p.res[msg.Dst()]
	if !ok {
		return
	}

	latency := rxTS.Sub(msg.SrcTS())
	if latency < 0 {
		p.l.Errorf("Got negative time delta %v for %s->%s seq %d", latency, msg.Src(), msg.Dst(), msg.Seq())
		return
	}
	if latency > p.opts.Timeout {
		res.delayed.Inc()
		return
	}
	res.success.Inc()
	res.latency.AddFloat64(latency.Seconds() / p.opts.LatencyUnit.Seconds())
}

// send attempts to send data over UDP.
func send(conn *net.UDPConn, raddr *net.UDPAddr, sendData []byte, timeout time.Duration) error {
	conn.SetWriteDeadline(time.Now().Add(timeout))
	_, err := conn.WriteToUDP(sendData, raddr)
	return err
}

// Return true if the underlying error indicates a udp.Client timeout.
// In our case, we're using the ReadTimeout- time until response is read.
func isClientTimeout(err error) bool {
	e, ok := err.(*net.OpError)
	return ok && e != nil && e.Timeout()
}

// recvLoop receives all packets over a UDP socket and updates
// flowStates accordingly.
func (p *Probe) recvLoop(ctx context.Context, conn *net.UDPConn) {
	b := make([]byte, maxMsgSize)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		conn.SetReadDeadline(time.Now().Add(p.opts.Timeout))
		msgLen, raddr, err := conn.ReadFromUDP(b)
		if err != nil {
			if !isClientTimeout(err) {
				p.l.Errorf("Receive error on %s (from %v): %v", conn.LocalAddr(), raddr, err)
			}
			continue
		}

		rxTS := time.Now()
		msg, err := message.NewMessage(b[:msgLen])
		if err != nil {
			p.l.Errorf("Incoming message error from %s: %v", raddr, err)
			continue
		}
		p.updateProbeResults(msg, rxTS)
	}
}

// runProbe performs a single probe run. The main thread launches one goroutine
// per target to probe. It manages a sync.WaitGroup and Wait's until all probes
// have finished, then exits the runProbe method.
//
// Each per-target goroutine sends a UDP message and on success waits for
// "timeout" duration before exiting. "recvLoop" function is expected to
// capture the responses before "timeout" and the main loop will flush the
// results.
func (p *Probe) runProbe() {
	maxLen := int(p.c.GetMaxLength())
	wg := sync.WaitGroup{}
	dstPort := int(p.c.GetPort())
	ipVer := int(p.c.GetIpVersion())

	for i, target := range p.targets {
		wg.Add(1)

		// Launch a separate goroutine for each target. Wait for p.opts.Timeout before returning.
		go func(target string, connID uint64) {
			defer wg.Done()

			ip, err := p.opts.Targets.Resolve(target, ipVer)
			if err != nil {
				p.l.Errorf("Unable to resolve %s: %v", target, err)
				return
			}
			raddr := &net.UDPAddr{
				IP:   ip,
				Port: dstPort,
			}

			flowState := p.fsm.FlowState(p.src, target)
			msg, seq, err := flowState.CreateMessage(p.src, target, time.Now(), maxLen)
			conn := p.connList[connID%(uint64(p.numConn))]
			if err = send(conn, raddr, msg, p.opts.Timeout); err != nil {
				p.l.Errorf("Unable to send to %s(%v): %v", target, raddr, err)
				flowState.WithdrawMessage(seq)
				return
			}
			p.updateSentCount(target)
			<-time.After(p.opts.Timeout)
		}(target, p.runID+uint64(i))
	}

	// Wait until all probes are done.
	wg.Wait()
	p.runID++
}

// Start starts and runs the probe indefinitely.
func (p *Probe) Start(ctx context.Context, dataChan chan *metrics.EventMetrics) {
	resultsChan := make(chan probeutils.ProbeResult, len(p.targets))
	targetsFunc := func() []string {
		return p.targets
	}
	statsExportIntvl := time.Duration(p.c.GetStatsExportIntervalMsec()) * time.Millisecond
	go probeutils.StatsKeeper(ctx, "udp", p.name, statsExportIntvl, targetsFunc, resultsChan, dataChan, p.l)

	for _, conn := range p.connList {
		go p.recvLoop(ctx, conn)
	}

	p.targets = p.opts.Targets.List()
	p.initProbeRunResults()

	// Create a ticker more frequent than stats_export_interval. This will allow
	// for aggregation of "delayed" packets without impacting probe results.
	var ticker *time.Ticker
	timeBuffer := time.Second * 5
	if statsExportIntvl-timeBuffer > 0 {
		ticker = time.NewTicker(statsExportIntvl - timeBuffer)
	} else {
		ticker = time.NewTicker(p.opts.Interval)
	}

	for range time.Tick(p.opts.Interval) {
		// Don't run another probe if context is canceled already.
		select {
		case <-ctx.Done():
			return
		default:
		}

		p.runProbe()

		// Use a ticker slower than stats export interval to output.
		select {
		case <-ticker.C:
			p.flushProbeRunResults(resultsChan)
			p.targets = p.opts.Targets.List()
			p.initProbeRunResults()
		default:
		}
	}
}

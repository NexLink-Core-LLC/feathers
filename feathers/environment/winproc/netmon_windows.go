//go:build windows

package winproc

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	"github.com/0xrawsec/golang-etw/etw"
	"github.com/apex/log"
)

// dbgOnce logs the property schema of the very first kernel-network event we
// capture, exactly once. The provider's field names ("PID"/"size") are only
// documented, not runtime-confirmed on every host — this one log line proves
// them (or tells us the real names) after a deploy, with zero ongoing cost.
var dbgOnce sync.Once

// kernelNetworkProviderGUID is the GUID of the Microsoft-Windows-Kernel-Network
// manifest provider. It emits per-connection TCP and UDP send/recv events for
// every process on the system, tagged with the owning PID and a byte count —
// the same data source Resource Monitor's "Network" tab uses. Crucially it
// covers UDP as well as TCP (The Isle's game traffic is UDP), which a
// TCP-only counter would miss entirely.
const kernelNetworkProviderGUID = "{7DD42A49-5329-4832-8DFD-43D979153A88}"

// etwSessionName is the name of the private real-time ETW session Wings runs.
// It is distinct from the NT Kernel Logger so it never collides with other
// tracing on the host.
const etwSessionName = "WingsKernelNetwork"

// Microsoft-Windows-Kernel-Network event IDs. This provider splits datagram
// send/recv by transport (TCP vs UDP) and by address family (IPv4 vs IPv6);
// we sum all of them per direction. Verified against the provider manifest
// (KNetEvt task IDs) — send opcodes map to TxBytes, recv opcodes to RxBytes.
//
//	TCP send : 10 (IPv4), 26 (IPv6)  -> Tx
//	TCP recv : 11 (IPv4), 27 (IPv6)  -> Rx
//	UDP send : 42 (IPv4), 58 (IPv6)  -> Tx
//	UDP recv : 43 (IPv4), 59 (IPv6)  -> Rx
var (
	sendEventIDs = map[uint16]struct{}{10: {}, 26: {}, 42: {}, 58: {}}
	recvEventIDs = map[uint16]struct{}{11: {}, 27: {}, 43: {}, 59: {}}
)

// counter accumulates cumulative bytes for a single PID. Values are cumulative
// totals (never reset), matching Docker's rx_bytes/tx_bytes semantics so Panel
// graphs render deltas correctly.
type counter struct {
	rx uint64
	tx uint64
}

// netMonitor is the package-level singleton network monitor. It runs a single
// real-time ETW session for the whole daemon and accumulates per-PID byte
// counters, but only for PIDs that have been explicitly registered (the PIDs of
// the servers Wings is tracking) so the map cannot grow with every system PID.
//
// All ETW failures are non-fatal by design: if the session never starts, or an
// event can't be parsed, the monitor degrades to reporting zero — exactly the
// pre-existing behaviour — and never panics or crashes Wings.
type netMonitor struct {
	startOnce sync.Once

	mu         sync.RWMutex
	counters   map[uint32]*counter // per-registered-PID cumulative bytes
	registered map[uint32]struct{} // PIDs we care about; gate for accumulation
	started    bool                // true once the session started successfully
}

var monitor = &netMonitor{
	counters:   make(map[uint32]*counter),
	registered: make(map[uint32]struct{}),
}

// warnOnce ensures a monitor failure is logged at most once for the lifetime of
// the process, so a host without the required privilege doesn't spam the log on
// every 2s poll.
var warnOnce sync.Once

func warnNetmon(err error, msg string) {
	warnOnce.Do(func() {
		log.WithField("subsystem", "winproc-netmon").
			WithField("error", err).
			Warn("winproc: per-process network stats unavailable, reporting 0: " + msg)
	})
}

// ensureStarted lazily starts the ETW session exactly once. It is safe to call
// on every poll tick — after the first call it is effectively a no-op. Any
// failure is logged once and leaves the monitor in the "return 0" state.
func (m *netMonitor) ensureStarted() {
	m.startOnce.Do(func() {
		// Recover so a panic inside session setup can never take Wings down.
		defer func() {
			if r := recover(); r != nil {
				warnNetmon(nil, "panic during ETW session start")
			}
		}()

		session := etw.NewRealTimeSession(etwSessionName)

		provider, err := etw.ParseProvider(kernelNetworkProviderGUID)
		if err != nil {
			// Fall back to a bare provider from just the GUID if the host's
			// provider enumeration didn't resolve it by name.
			provider = etw.Provider{GUID: kernelNetworkProviderGUID, EnableLevel: 0xff}
		}

		if err := session.EnableProvider(provider); err != nil {
			warnNetmon(err, "failed to enable Kernel-Network provider (needs SeDebug/admin)")
			return
		}

		consumer := etw.NewRealTimeConsumer(context.Background())
		consumer.FromSessions(session)

		if err := consumer.Start(); err != nil {
			warnNetmon(err, "failed to start ETW consumer")
			_ = session.Stop()
			return
		}

		m.mu.Lock()
		m.started = true
		m.mu.Unlock()

		// Drain parsed events in a dedicated goroutine, guarded by recover() so a
		// bad event can never crash the daemon.
		go func() {
			defer func() {
				if r := recover(); r != nil {
					warnNetmon(nil, "panic in ETW event loop")
				}
			}()
			for e := range consumer.Events {
				m.handleEvent(e)
			}
		}()
	})
}

// handleEvent accumulates a single parsed ETW event into the per-PID counters,
// but only if that PID is currently registered. Events we can't parse are
// dropped (logged once), never fatal.
func (m *netMonitor) handleEvent(e *etw.Event) {
	if e == nil {
		return
	}

	id := e.System.EventID
	_, isSend := sendEventIDs[id]
	_, isRecv := recvEventIDs[id]
	if !isSend && !isRecv {
		return
	}

	// One-shot: dump the parsed property schema so a deploy confirms the real
	// field names for PID/size. Fires once, on the first network event.
	dbgOnce.Do(func() {
		log.WithField("subsystem", "winproc-netmon").
			WithField("event_id", id).
			WithField("data", fmt.Sprintf("%v", e.EventData)).
			Info("winproc: first kernel-network event captured (property schema)")
	})

	pid, ok := eventPID(e)
	if !ok {
		return
	}

	// Cheap gate: ignore every PID we aren't tracking so the map stays bounded.
	m.mu.RLock()
	_, tracked := m.registered[pid]
	m.mu.RUnlock()
	if !tracked {
		return
	}

	size, ok := eventSize(e)
	if !ok {
		return
	}

	m.mu.Lock()
	c := m.counters[pid]
	if c == nil {
		c = &counter{}
		m.counters[pid] = c
	}
	if isSend {
		c.tx += size
	} else {
		c.rx += size
	}
	m.mu.Unlock()
}

// eventPID extracts the owning process id from a Kernel-Network event. The
// provider carries the connection's PID as a "PID" property (parsed by TDH as a
// decimal string or a number depending on host); we accept either. Note this is
// the connection owner, NOT e.System.Execution.ProcessID (the event emitter).
func eventPID(e *etw.Event) (uint32, bool) {
	return eventUint32(e, "PID")
}

// eventSize extracts the transferred byte count ("size" property) from a
// Kernel-Network event.
func eventSize(e *etw.Event) (uint64, bool) {
	if v, ok := e.GetProperty("size"); ok {
		if n, ok := toUint64(v); ok {
			return n, true
		}
	}
	return 0, false
}

// eventUint32 reads a named property and coerces it to a uint32.
func eventUint32(e *etw.Event, name string) (uint32, bool) {
	if v, ok := e.GetProperty(name); ok {
		if n, ok := toUint64(v); ok {
			return uint32(n), true
		}
	}
	return 0, false
}

// toUint64 coerces a TDH-parsed property value (string or numeric) to uint64.
// TDH usually renders integer properties as decimal strings, but we tolerate
// the numeric case too so a library change won't silently break parsing.
func toUint64(v interface{}) (uint64, bool) {
	switch t := v.(type) {
	case string:
		n, err := strconv.ParseUint(t, 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	case uint64:
		return t, true
	case uint32:
		return uint64(t), true
	case uint16:
		return uint64(t), true
	case int64:
		if t < 0 {
			return 0, false
		}
		return uint64(t), true
	case int:
		if t < 0 {
			return 0, false
		}
		return uint64(t), true
	case float64:
		if t < 0 {
			return 0, false
		}
		return uint64(t), true
	default:
		return 0, false
	}
}

// registerPIDs marks the given PIDs as tracked so their network events are
// accumulated. Called each poll by stats.go with the server's current process
// tree. PIDs previously registered but no longer present are dropped along with
// their counters to keep the maps bounded across process restarts.
func (m *netMonitor) registerPIDs(pids []uint32) {
	if len(pids) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	next := make(map[uint32]struct{}, len(pids))
	for _, p := range pids {
		next[p] = struct{}{}
	}
	m.registered = next

	// Evict counters for PIDs that are no longer tracked.
	for pid := range m.counters {
		if _, ok := next[pid]; !ok {
			delete(m.counters, pid)
		}
	}
}

// networkBytesFor returns the summed cumulative rx/tx bytes across the given
// PIDs (the server's whole process tree). If the monitor never started, or none
// of the PIDs have seen traffic yet, it returns 0,0 — the pre-existing
// behaviour, and never an error.
func (m *netMonitor) networkBytesFor(pids []uint32) (rx, tx uint64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, p := range pids {
		if c := m.counters[p]; c != nil {
			rx += c.rx
			tx += c.tx
		}
	}
	return rx, tx
}

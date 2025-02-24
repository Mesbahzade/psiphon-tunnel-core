/*
 * Copyright (c) 2016, Psiphon Inc.
 * All rights reserved.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Psiphon-Labs/goarista/monotime"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/accesscontrol"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/crypto/ssh"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/fragmentor"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/marionette"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/obfuscator"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/osl"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/protocol"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/quic"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/tactics"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/tapdance"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/tun"
	"github.com/marusama/semaphore"
	cache "github.com/patrickmn/go-cache"
)

const (
	SSH_AUTH_LOG_PERIOD                   = 30 * time.Minute
	SSH_HANDSHAKE_TIMEOUT                 = 30 * time.Second
	SSH_BEGIN_HANDSHAKE_TIMEOUT           = 1 * time.Second
	SSH_CONNECTION_READ_DEADLINE          = 5 * time.Minute
	SSH_TCP_PORT_FORWARD_COPY_BUFFER_SIZE = 8192
	SSH_TCP_PORT_FORWARD_QUEUE_SIZE       = 1024
	SSH_KEEP_ALIVE_PAYLOAD_MIN_BYTES      = 0
	SSH_KEEP_ALIVE_PAYLOAD_MAX_BYTES      = 256
	SSH_SEND_OSL_INITIAL_RETRY_DELAY      = 30 * time.Second
	SSH_SEND_OSL_RETRY_FACTOR             = 2
	OSL_SESSION_CACHE_TTL                 = 5 * time.Minute
	MAX_AUTHORIZATIONS                    = 16
	PRE_HANDSHAKE_RANDOM_STREAM_MAX_COUNT = 1
	RANDOM_STREAM_MAX_BYTES               = 10485760
)

// TunnelServer is the main server that accepts Psiphon client
// connections, via various obfuscation protocols, and provides
// port forwarding (TCP and UDP) services to the Psiphon client.
// At its core, TunnelServer is an SSH server. SSH is the base
// protocol that provides port forward multiplexing, and transport
// security. Layered on top of SSH, optionally, is Obfuscated SSH
// and meek protocols, which provide further circumvention
// capabilities.
type TunnelServer struct {
	runWaitGroup      *sync.WaitGroup
	listenerError     chan error
	shutdownBroadcast <-chan struct{}
	sshServer         *sshServer
}

// NewTunnelServer initializes a new tunnel server.
func NewTunnelServer(
	support *SupportServices,
	shutdownBroadcast <-chan struct{}) (*TunnelServer, error) {

	sshServer, err := newSSHServer(support, shutdownBroadcast)
	if err != nil {
		return nil, common.ContextError(err)
	}

	return &TunnelServer{
		runWaitGroup:      new(sync.WaitGroup),
		listenerError:     make(chan error),
		shutdownBroadcast: shutdownBroadcast,
		sshServer:         sshServer,
	}, nil
}

// Run runs the tunnel server; this function blocks while running a selection of
// listeners that handle connection using various obfuscation protocols.
//
// Run listens on each designated tunnel port and spawns new goroutines to handle
// each client connection. It halts when shutdownBroadcast is signaled. A list of active
// clients is maintained, and when halting all clients are cleanly shutdown.
//
// Each client goroutine handles its own obfuscation (optional), SSH handshake, SSH
// authentication, and then looping on client new channel requests. "direct-tcpip"
// channels, dynamic port fowards, are supported. When the UDPInterceptUdpgwServerAddress
// config parameter is configured, UDP port forwards over a TCP stream, following
// the udpgw protocol, are handled.
//
// A new goroutine is spawned to handle each port forward for each client. Each port
// forward tracks its bytes transferred. Overall per-client stats for connection duration,
// GeoIP, number of port forwards, and bytes transferred are tracked and logged when the
// client shuts down.
//
// Note: client handler goroutines may still be shutting down after Run() returns. See
// comment in sshClient.stop(). TODO: fully synchronized shutdown.
func (server *TunnelServer) Run() error {

	type sshListener struct {
		net.Listener
		localAddress   string
		tunnelProtocol string
	}

	// TODO: should TunnelServer hold its own support pointer?
	support := server.sshServer.support

	// First bind all listeners; once all are successful,
	// start accepting connections on each.

	var listeners []*sshListener

	for tunnelProtocol, listenPort := range support.Config.TunnelProtocolPorts {

		localAddress := fmt.Sprintf(
			"%s:%d", support.Config.ServerIPAddress, listenPort)

		var listener net.Listener
		var err error

		if protocol.TunnelProtocolUsesQUIC(tunnelProtocol) {

			listener, err = quic.Listen(
				CommonLogger(log),
				localAddress,
				support.Config.ObfuscatedSSHKey)

		} else if protocol.TunnelProtocolUsesMarionette(tunnelProtocol) {

			listener, err = marionette.Listen(
				support.Config.ServerIPAddress,
				support.Config.MarionetteFormat)

		} else if protocol.TunnelProtocolUsesTapdance(tunnelProtocol) {

			listener, err = tapdance.Listen(localAddress)

		} else {

			listener, err = net.Listen("tcp", localAddress)
		}

		if err != nil {
			for _, existingListener := range listeners {
				existingListener.Listener.Close()
			}
			return common.ContextError(err)
		}

		tacticsListener := tactics.NewListener(
			listener,
			support.TacticsServer,
			tunnelProtocol,
			func(IPAddress string) common.GeoIPData {
				return common.GeoIPData(support.GeoIPService.Lookup(IPAddress))
			})

		log.WithContextFields(
			LogFields{
				"localAddress":   localAddress,
				"tunnelProtocol": tunnelProtocol,
			}).Info("listening")

		listeners = append(
			listeners,
			&sshListener{
				Listener:       tacticsListener,
				localAddress:   localAddress,
				tunnelProtocol: tunnelProtocol,
			})
	}

	for _, listener := range listeners {
		server.runWaitGroup.Add(1)
		go func(listener *sshListener) {
			defer server.runWaitGroup.Done()

			log.WithContextFields(
				LogFields{
					"localAddress":   listener.localAddress,
					"tunnelProtocol": listener.tunnelProtocol,
				}).Info("running")

			server.sshServer.runListener(
				listener.Listener,
				server.listenerError,
				listener.tunnelProtocol)

			log.WithContextFields(
				LogFields{
					"localAddress":   listener.localAddress,
					"tunnelProtocol": listener.tunnelProtocol,
				}).Info("stopped")

		}(listener)
	}

	var err error
	select {
	case <-server.shutdownBroadcast:
	case err = <-server.listenerError:
	}

	for _, listener := range listeners {
		listener.Close()
	}
	server.sshServer.stopClients()
	server.runWaitGroup.Wait()

	log.WithContext().Info("stopped")

	return err
}

// GetLoadStats returns load stats for the tunnel server. The stats are
// broken down by protocol ("SSH", "OSSH", etc.) and type. Types of stats
// include current connected client count, total number of current port
// forwards.
func (server *TunnelServer) GetLoadStats() (ProtocolStats, RegionStats) {
	return server.sshServer.getLoadStats()
}

// ResetAllClientTrafficRules resets all established client traffic rules
// to use the latest config and client properties. Any existing traffic
// rule state is lost, including throttling state.
func (server *TunnelServer) ResetAllClientTrafficRules() {
	server.sshServer.resetAllClientTrafficRules()
}

// ResetAllClientOSLConfigs resets all established client OSL state to use
// the latest OSL config. Any existing OSL state is lost, including partial
// progress towards SLOKs.
func (server *TunnelServer) ResetAllClientOSLConfigs() {
	server.sshServer.resetAllClientOSLConfigs()
}

// SetClientHandshakeState sets the handshake state -- that it completed and
// what parameters were passed -- in sshClient. This state is used for allowing
// port forwards and for future traffic rule selection. SetClientHandshakeState
// also triggers an immediate traffic rule re-selection, as the rules selected
// upon tunnel establishment may no longer apply now that handshake values are
// set.
//
// The authorizations received from the client handshake are verified and the
// resulting list of authorized access types are applied to the client's tunnel
// and traffic rules. A list of active authorization IDs and authorized access
// types is returned for responding to the client and logging.
func (server *TunnelServer) SetClientHandshakeState(
	sessionID string,
	state handshakeState,
	authorizations []string) ([]string, []string, error) {

	return server.sshServer.setClientHandshakeState(sessionID, state, authorizations)
}

// GetClientHandshaked indicates whether the client has completed a handshake
// and whether its traffic rules are immediately exhausted.
func (server *TunnelServer) GetClientHandshaked(
	sessionID string) (bool, bool, error) {

	return server.sshServer.getClientHandshaked(sessionID)
}

// UpdateClientAPIParameters updates the recorded handhake API parameters for
// the client corresponding to sessionID.
func (server *TunnelServer) UpdateClientAPIParameters(
	sessionID string,
	apiParams common.APIParameters) error {

	return server.sshServer.updateClientAPIParameters(sessionID, apiParams)
}

// ExpectClientDomainBytes indicates whether the client was configured to report
// domain bytes in its handshake response.
func (server *TunnelServer) ExpectClientDomainBytes(
	sessionID string) (bool, error) {

	return server.sshServer.expectClientDomainBytes(sessionID)
}

// SetEstablishTunnels sets whether new tunnels may be established or not.
// When not establishing, incoming connections are immediately closed.
func (server *TunnelServer) SetEstablishTunnels(establish bool) {
	server.sshServer.setEstablishTunnels(establish)
}

// GetEstablishTunnels returns whether new tunnels may be established or not.
func (server *TunnelServer) GetEstablishTunnels() bool {
	return server.sshServer.getEstablishTunnels()
}

type sshServer struct {
	// Note: 64-bit ints used with atomic operations are placed
	// at the start of struct to ensure 64-bit alignment.
	// (https://golang.org/pkg/sync/atomic/#pkg-note-BUG)
	lastAuthLog                  int64
	authFailedCount              int64
	support                      *SupportServices
	establishTunnels             int32
	concurrentSSHHandshakes      semaphore.Semaphore
	shutdownBroadcast            <-chan struct{}
	sshHostKey                   ssh.Signer
	clientsMutex                 sync.Mutex
	stoppingClients              bool
	acceptedClientCounts         map[string]map[string]int64
	clients                      map[string]*sshClient
	oslSessionCacheMutex         sync.Mutex
	oslSessionCache              *cache.Cache
	authorizationSessionIDsMutex sync.Mutex
	authorizationSessionIDs      map[string]string
}

func newSSHServer(
	support *SupportServices,
	shutdownBroadcast <-chan struct{}) (*sshServer, error) {

	privateKey, err := ssh.ParseRawPrivateKey([]byte(support.Config.SSHPrivateKey))
	if err != nil {
		return nil, common.ContextError(err)
	}

	// TODO: use cert (ssh.NewCertSigner) for anti-fingerprint?
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		return nil, common.ContextError(err)
	}

	var concurrentSSHHandshakes semaphore.Semaphore
	if support.Config.MaxConcurrentSSHHandshakes > 0 {
		concurrentSSHHandshakes = semaphore.New(support.Config.MaxConcurrentSSHHandshakes)
	}

	// The OSL session cache temporarily retains OSL seed state
	// progress for disconnected clients. This enables clients
	// that disconnect and immediately reconnect to the same
	// server to resume their OSL progress. Cached progress
	// is referenced by session ID and is retained for
	// OSL_SESSION_CACHE_TTL after disconnect.
	//
	// Note: session IDs are assumed to be unpredictable. If a
	// rogue client could guess the session ID of another client,
	// it could resume its OSL progress and, if the OSL config
	// were known, infer some activity.
	oslSessionCache := cache.New(OSL_SESSION_CACHE_TTL, 1*time.Minute)

	return &sshServer{
		support:                 support,
		establishTunnels:        1,
		concurrentSSHHandshakes: concurrentSSHHandshakes,
		shutdownBroadcast:       shutdownBroadcast,
		sshHostKey:              signer,
		acceptedClientCounts:    make(map[string]map[string]int64),
		clients:                 make(map[string]*sshClient),
		oslSessionCache:         oslSessionCache,
		authorizationSessionIDs: make(map[string]string),
	}, nil
}

func (sshServer *sshServer) setEstablishTunnels(establish bool) {

	// Do nothing when the setting is already correct. This avoids
	// spurious log messages when setEstablishTunnels is called
	// periodically with the same setting.
	if establish == sshServer.getEstablishTunnels() {
		return
	}

	establishFlag := int32(1)
	if !establish {
		establishFlag = 0
	}
	atomic.StoreInt32(&sshServer.establishTunnels, establishFlag)

	log.WithContextFields(
		LogFields{"establish": establish}).Info("establishing tunnels")
}

func (sshServer *sshServer) getEstablishTunnels() bool {
	return atomic.LoadInt32(&sshServer.establishTunnels) == 1
}

// runListener is intended to run an a goroutine; it blocks
// running a particular listener. If an unrecoverable error
// occurs, it will send the error to the listenerError channel.
func (sshServer *sshServer) runListener(
	listener net.Listener,
	listenerError chan<- error,
	listenerTunnelProtocol string) {

	runningProtocols := make([]string, 0)
	for tunnelProtocol := range sshServer.support.Config.TunnelProtocolPorts {
		runningProtocols = append(runningProtocols, tunnelProtocol)
	}

	handleClient := func(clientTunnelProtocol string, clientConn net.Conn) {

		// Note: establish tunnel limiter cannot simply stop TCP
		// listeners in all cases (e.g., meek) since SSH tunnel can
		// span multiple TCP connections.

		if !sshServer.getEstablishTunnels() {
			log.WithContext().Debug("not establishing tunnels")
			clientConn.Close()
			return
		}

		// The tunnelProtocol passed to handleClient is used for stats,
		// throttling, etc. When the tunnel protocol can be determined
		// unambiguously from the listening port, use that protocol and
		// don't use any client-declared value. Only use the client's
		// value, if present, in special cases where the listening port
		// cannot distinguish the protocol.
		tunnelProtocol := listenerTunnelProtocol
		if clientTunnelProtocol != "" &&
			protocol.UseClientTunnelProtocol(
				clientTunnelProtocol, runningProtocols) {
			tunnelProtocol = clientTunnelProtocol
		}

		// process each client connection concurrently
		go sshServer.handleClient(tunnelProtocol, clientConn)
	}

	// Note: when exiting due to a unrecoverable error, be sure
	// to try to send the error to listenerError so that the outer
	// TunnelServer.Run will properly shut down instead of remaining
	// running.

	if protocol.TunnelProtocolUsesMeekHTTP(listenerTunnelProtocol) ||
		protocol.TunnelProtocolUsesMeekHTTPS(listenerTunnelProtocol) {

		meekServer, err := NewMeekServer(
			sshServer.support,
			listener,
			protocol.TunnelProtocolUsesMeekHTTPS(listenerTunnelProtocol),
			protocol.TunnelProtocolUsesFrontedMeek(listenerTunnelProtocol),
			protocol.TunnelProtocolUsesObfuscatedSessionTickets(listenerTunnelProtocol),
			handleClient,
			sshServer.shutdownBroadcast)

		if err == nil {
			err = meekServer.Run()
		}

		if err != nil {
			select {
			case listenerError <- common.ContextError(err):
			default:
			}
			return
		}

	} else {

		for {
			conn, err := listener.Accept()

			select {
			case <-sshServer.shutdownBroadcast:
				if err == nil {
					conn.Close()
				}
				return
			default:
			}

			if err != nil {
				if e, ok := err.(net.Error); ok && e.Temporary() {
					log.WithContextFields(LogFields{"error": err}).Error("accept failed")
					// Temporary error, keep running
					continue
				}

				select {
				case listenerError <- common.ContextError(err):
				default:
				}
				return
			}

			handleClient("", conn)
		}
	}
}

// An accepted client has completed a direct TCP or meek connection and has a net.Conn. Registration
// is for tracking the number of connections.
func (sshServer *sshServer) registerAcceptedClient(tunnelProtocol, region string) {

	sshServer.clientsMutex.Lock()
	defer sshServer.clientsMutex.Unlock()

	if sshServer.acceptedClientCounts[tunnelProtocol] == nil {
		sshServer.acceptedClientCounts[tunnelProtocol] = make(map[string]int64)
	}

	sshServer.acceptedClientCounts[tunnelProtocol][region] += 1
}

func (sshServer *sshServer) unregisterAcceptedClient(tunnelProtocol, region string) {

	sshServer.clientsMutex.Lock()
	defer sshServer.clientsMutex.Unlock()

	sshServer.acceptedClientCounts[tunnelProtocol][region] -= 1
}

// An established client has completed its SSH handshake and has a ssh.Conn. Registration is
// for tracking the number of fully established clients and for maintaining a list of running
// clients (for stopping at shutdown time).
func (sshServer *sshServer) registerEstablishedClient(client *sshClient) bool {

	sshServer.clientsMutex.Lock()

	if sshServer.stoppingClients {
		sshServer.clientsMutex.Unlock()
		return false
	}

	// In the case of a duplicate client sessionID, the previous client is closed.
	// - Well-behaved clients generate a random sessionID that should be unique (won't
	//   accidentally conflict) and hard to guess (can't be targeted by a malicious
	//   client).
	// - Clients reuse the same sessionID when a tunnel is unexpectedly disconnected
	//   and reestablished. In this case, when the same server is selected, this logic
	//   will be hit; closing the old, dangling client is desirable.
	// - Multi-tunnel clients should not normally use one server for multiple tunnels.
	existingClient := sshServer.clients[client.sessionID]

	sshServer.clients[client.sessionID] = client

	sshServer.clientsMutex.Unlock()

	// Call stop() outside the mutex to avoid deadlock.
	if existingClient != nil {
		existingClient.stop()

		// Since existingClient.run() isn't guaranteed to have terminated at
		// this point, synchronously release authorizations for the previous
		// client here. This ensures that the authorization IDs are not in
		// use when the reconnecting client submits its authorizations.
		existingClient.cleanupAuthorizations()

		log.WithContext().Debug(
			"stopped existing client with duplicate session ID")
	}

	return true
}

func (sshServer *sshServer) unregisterEstablishedClient(client *sshClient) {

	sshServer.clientsMutex.Lock()

	registeredClient := sshServer.clients[client.sessionID]

	// registeredClient will differ from client when client
	// is the existingClient terminated in registerEstablishedClient.
	// In that case, registeredClient remains connected, and
	// the sshServer.clients entry should be retained.
	if registeredClient == client {
		delete(sshServer.clients, client.sessionID)
	}

	sshServer.clientsMutex.Unlock()

	// Call stop() outside the mutex to avoid deadlock.
	client.stop()
}

type ProtocolStats map[string]map[string]int64
type RegionStats map[string]map[string]map[string]int64

func (sshServer *sshServer) getLoadStats() (ProtocolStats, RegionStats) {

	sshServer.clientsMutex.Lock()
	defer sshServer.clientsMutex.Unlock()

	// Explicitly populate with zeros to ensure 0 counts in log messages
	zeroStats := func() map[string]int64 {
		stats := make(map[string]int64)
		stats["accepted_clients"] = 0
		stats["established_clients"] = 0
		stats["dialing_tcp_port_forwards"] = 0
		stats["tcp_port_forwards"] = 0
		stats["total_tcp_port_forwards"] = 0
		stats["udp_port_forwards"] = 0
		stats["total_udp_port_forwards"] = 0
		stats["tcp_port_forward_dialed_count"] = 0
		stats["tcp_port_forward_dialed_duration"] = 0
		stats["tcp_port_forward_failed_count"] = 0
		stats["tcp_port_forward_failed_duration"] = 0
		stats["tcp_port_forward_rejected_dialing_limit_count"] = 0
		return stats
	}

	zeroProtocolStats := func() map[string]map[string]int64 {
		stats := make(map[string]map[string]int64)
		stats["ALL"] = zeroStats()
		for tunnelProtocol := range sshServer.support.Config.TunnelProtocolPorts {
			stats[tunnelProtocol] = zeroStats()
		}
		return stats
	}

	// [<protocol or ALL>][<stat name>] -> count
	protocolStats := zeroProtocolStats()

	// [<region][<protocol or ALL>][<stat name>] -> count
	regionStats := make(RegionStats)

	// Note: as currently tracked/counted, each established client is also an accepted client

	for tunnelProtocol, regionAcceptedClientCounts := range sshServer.acceptedClientCounts {
		for region, acceptedClientCount := range regionAcceptedClientCounts {

			if acceptedClientCount > 0 {
				if regionStats[region] == nil {
					regionStats[region] = zeroProtocolStats()
				}

				protocolStats["ALL"]["accepted_clients"] += acceptedClientCount
				protocolStats[tunnelProtocol]["accepted_clients"] += acceptedClientCount

				regionStats[region]["ALL"]["accepted_clients"] += acceptedClientCount
				regionStats[region][tunnelProtocol]["accepted_clients"] += acceptedClientCount
			}
		}
	}

	for _, client := range sshServer.clients {

		client.Lock()

		tunnelProtocol := client.tunnelProtocol
		region := client.geoIPData.Country

		if regionStats[region] == nil {
			regionStats[region] = zeroProtocolStats()
		}

		stats := []map[string]int64{
			protocolStats["ALL"],
			protocolStats[tunnelProtocol],
			regionStats[region]["ALL"],
			regionStats[region][tunnelProtocol]}

		for _, stat := range stats {

			stat["established_clients"] += 1

			// Note: can't sum trafficState.peakConcurrentPortForwardCount to get a global peak

			stat["dialing_tcp_port_forwards"] += client.tcpTrafficState.concurrentDialingPortForwardCount
			stat["tcp_port_forwards"] += client.tcpTrafficState.concurrentPortForwardCount
			stat["total_tcp_port_forwards"] += client.tcpTrafficState.totalPortForwardCount
			// client.udpTrafficState.concurrentDialingPortForwardCount isn't meaningful
			stat["udp_port_forwards"] += client.udpTrafficState.concurrentPortForwardCount
			stat["total_udp_port_forwards"] += client.udpTrafficState.totalPortForwardCount

			stat["tcp_port_forward_dialed_count"] += client.qualityMetrics.tcpPortForwardDialedCount
			stat["tcp_port_forward_dialed_duration"] +=
				int64(client.qualityMetrics.tcpPortForwardDialedDuration / time.Millisecond)
			stat["tcp_port_forward_failed_count"] += client.qualityMetrics.tcpPortForwardFailedCount
			stat["tcp_port_forward_failed_duration"] +=
				int64(client.qualityMetrics.tcpPortForwardFailedDuration / time.Millisecond)
			stat["tcp_port_forward_rejected_dialing_limit_count"] +=
				client.qualityMetrics.tcpPortForwardRejectedDialingLimitCount
		}

		client.qualityMetrics.tcpPortForwardDialedCount = 0
		client.qualityMetrics.tcpPortForwardDialedDuration = 0
		client.qualityMetrics.tcpPortForwardFailedCount = 0
		client.qualityMetrics.tcpPortForwardFailedDuration = 0
		client.qualityMetrics.tcpPortForwardRejectedDialingLimitCount = 0

		client.Unlock()
	}

	return protocolStats, regionStats
}

func (sshServer *sshServer) resetAllClientTrafficRules() {

	sshServer.clientsMutex.Lock()
	clients := make(map[string]*sshClient)
	for sessionID, client := range sshServer.clients {
		clients[sessionID] = client
	}
	sshServer.clientsMutex.Unlock()

	for _, client := range clients {
		client.setTrafficRules()
	}
}

func (sshServer *sshServer) resetAllClientOSLConfigs() {

	// Flush cached seed state. This has the same effect
	// and same limitations as calling setOSLConfig for
	// currently connected clients -- all progress is lost.
	sshServer.oslSessionCacheMutex.Lock()
	sshServer.oslSessionCache.Flush()
	sshServer.oslSessionCacheMutex.Unlock()

	sshServer.clientsMutex.Lock()
	clients := make(map[string]*sshClient)
	for sessionID, client := range sshServer.clients {
		clients[sessionID] = client
	}
	sshServer.clientsMutex.Unlock()

	for _, client := range clients {
		client.setOSLConfig()
	}
}

func (sshServer *sshServer) setClientHandshakeState(
	sessionID string,
	state handshakeState,
	authorizations []string) ([]string, []string, error) {

	sshServer.clientsMutex.Lock()
	client := sshServer.clients[sessionID]
	sshServer.clientsMutex.Unlock()

	if client == nil {
		return nil, nil, common.ContextError(errors.New("unknown session ID"))
	}

	activeAuthorizationIDs, authorizedAccessTypes, err := client.setHandshakeState(
		state, authorizations)
	if err != nil {
		return nil, nil, common.ContextError(err)
	}

	return activeAuthorizationIDs, authorizedAccessTypes, nil
}

func (sshServer *sshServer) getClientHandshaked(
	sessionID string) (bool, bool, error) {

	sshServer.clientsMutex.Lock()
	client := sshServer.clients[sessionID]
	sshServer.clientsMutex.Unlock()

	if client == nil {
		return false, false, common.ContextError(errors.New("unknown session ID"))
	}

	completed, exhausted := client.getHandshaked()

	return completed, exhausted, nil
}

func (sshServer *sshServer) updateClientAPIParameters(
	sessionID string,
	apiParams common.APIParameters) error {

	sshServer.clientsMutex.Lock()
	client := sshServer.clients[sessionID]
	sshServer.clientsMutex.Unlock()

	if client == nil {
		return common.ContextError(errors.New("unknown session ID"))
	}

	client.updateAPIParameters(apiParams)

	return nil
}

func (sshServer *sshServer) revokeClientAuthorizations(sessionID string) {
	sshServer.clientsMutex.Lock()
	client := sshServer.clients[sessionID]
	sshServer.clientsMutex.Unlock()

	if client == nil {
		return
	}

	// sshClient.handshakeState.authorizedAccessTypes is not cleared. Clearing
	// authorizedAccessTypes may cause sshClient.logTunnel to fail to log
	// access types. As the revocation may be due to legitimate use of an
	// authorization in multiple sessions by a single client, useful metrics
	// would be lost.

	client.Lock()
	client.handshakeState.authorizationsRevoked = true
	client.Unlock()

	// Select and apply new traffic rules, as filtered by the client's new
	// authorization state.

	client.setTrafficRules()
}

func (sshServer *sshServer) expectClientDomainBytes(
	sessionID string) (bool, error) {

	sshServer.clientsMutex.Lock()
	client := sshServer.clients[sessionID]
	sshServer.clientsMutex.Unlock()

	if client == nil {
		return false, common.ContextError(errors.New("unknown session ID"))
	}

	return client.expectDomainBytes(), nil
}

func (sshServer *sshServer) stopClients() {

	sshServer.clientsMutex.Lock()
	sshServer.stoppingClients = true
	clients := sshServer.clients
	sshServer.clients = make(map[string]*sshClient)
	sshServer.clientsMutex.Unlock()

	for _, client := range clients {
		client.stop()
	}
}

func (sshServer *sshServer) handleClient(tunnelProtocol string, clientConn net.Conn) {

	// Calling clientConn.RemoteAddr at this point, before any Read calls,
	// satisfies the constraint documented in tapdance.Listen.

	geoIPData := sshServer.support.GeoIPService.Lookup(
		common.IPAddressFromAddr(clientConn.RemoteAddr()))

	sshServer.registerAcceptedClient(tunnelProtocol, geoIPData.Country)
	defer sshServer.unregisterAcceptedClient(tunnelProtocol, geoIPData.Country)

	// When configured, enforce a cap on the number of concurrent SSH
	// handshakes. This limits load spikes on busy servers when many clients
	// attempt to connect at once. Wait a short time, SSH_BEGIN_HANDSHAKE_TIMEOUT,
	// to acquire; waiting will avoid immediately creating more load on another
	// server in the network when the client tries a new candidate. Disconnect the
	// client when that wait time is exceeded.
	//
	// This mechanism limits memory allocations and CPU usage associated with the
	// SSH handshake. At this point, new direct TCP connections or new meek
	// connections, with associated resource usage, are already established. Those
	// connections are expected to be rate or load limited using other mechanisms.
	//
	// TODO:
	//
	// - deduct time spent acquiring the semaphore from SSH_HANDSHAKE_TIMEOUT in
	//   sshClient.run, since the client is also applying an SSH handshake timeout
	//   and won't exclude time spent waiting.
	// - each call to sshServer.handleClient (in sshServer.runListener) is invoked
	//   in its own goroutine, but shutdown doesn't synchronously await these
	//   goroutnes. Once this is synchronizes, the following context.WithTimeout
	//   should use an sshServer parent context to ensure blocking acquires
	//   interrupt immediately upon shutdown.

	var onSSHHandshakeFinished func()
	if sshServer.support.Config.MaxConcurrentSSHHandshakes > 0 {

		ctx, cancelFunc := context.WithTimeout(
			context.Background(),
			sshServer.support.Config.sshBeginHandshakeTimeout)
		defer cancelFunc()

		err := sshServer.concurrentSSHHandshakes.Acquire(ctx, 1)
		if err != nil {
			clientConn.Close()
			// This is a debug log as the only possible error is context timeout.
			log.WithContextFields(LogFields{"error": err}).Debug(
				"acquire SSH handshake semaphore failed")
			return
		}

		onSSHHandshakeFinished = func() {
			sshServer.concurrentSSHHandshakes.Release(1)
		}
	}

	sshClient := newSshClient(sshServer, tunnelProtocol, geoIPData)

	// sshClient.run _must_ call onSSHHandshakeFinished to release the semaphore:
	// in any error case; or, as soon as the SSH handshake phase has successfully
	// completed.

	sshClient.run(clientConn, onSSHHandshakeFinished)
}

func (sshServer *sshServer) monitorPortForwardDialError(err error) {

	// "err" is the error returned from a failed TCP or UDP port
	// forward dial. Certain system error codes indicate low resource
	// conditions: insufficient file descriptors, ephemeral ports, or
	// memory. For these cases, log an alert.

	// TODO: also temporarily suspend new clients

	// Note: don't log net.OpError.Error() as the full error string
	// may contain client destination addresses.

	opErr, ok := err.(*net.OpError)
	if ok {
		if opErr.Err == syscall.EADDRNOTAVAIL ||
			opErr.Err == syscall.EAGAIN ||
			opErr.Err == syscall.ENOMEM ||
			opErr.Err == syscall.EMFILE ||
			opErr.Err == syscall.ENFILE {

			log.WithContextFields(
				LogFields{"error": opErr.Err}).Error(
				"port forward dial failed due to unavailable resource")
		}
	}
}

type sshClient struct {
	sync.Mutex
	sshServer                            *sshServer
	tunnelProtocol                       string
	sshConn                              ssh.Conn
	activityConn                         *common.ActivityMonitoredConn
	throttledConn                        *common.ThrottledConn
	geoIPData                            GeoIPData
	sessionID                            string
	isFirstTunnelInSession               bool
	supportsServerRequests               bool
	handshakeState                       handshakeState
	udpChannel                           ssh.Channel
	packetTunnelChannel                  ssh.Channel
	trafficRules                         TrafficRules
	tcpTrafficState                      trafficState
	udpTrafficState                      trafficState
	qualityMetrics                       qualityMetrics
	tcpPortForwardLRU                    *common.LRUConns
	oslClientSeedState                   *osl.ClientSeedState
	signalIssueSLOKs                     chan struct{}
	runCtx                               context.Context
	stopRunning                          context.CancelFunc
	tcpPortForwardDialingAvailableSignal context.CancelFunc
	releaseAuthorizations                func()
	stopTimer                            *time.Timer
	preHandshakeRandomStreamMetrics      randomStreamMetrics
	postHandshakeRandomStreamMetrics     randomStreamMetrics
}

type trafficState struct {
	bytesUp                               int64
	bytesDown                             int64
	concurrentDialingPortForwardCount     int64
	peakConcurrentDialingPortForwardCount int64
	concurrentPortForwardCount            int64
	peakConcurrentPortForwardCount        int64
	totalPortForwardCount                 int64
	availablePortForwardCond              *sync.Cond
}

type randomStreamMetrics struct {
	count                 int
	upstreamBytes         int
	receivedUpstreamBytes int
	downstreamBytes       int
	sentDownstreamBytes   int
}

// qualityMetrics records upstream TCP dial attempts and
// elapsed time. Elapsed time includes the full TCP handshake
// and, in aggregate, is a measure of the quality of the
// upstream link. These stats are recorded by each sshClient
// and then reported and reset in sshServer.getLoadStats().
type qualityMetrics struct {
	tcpPortForwardDialedCount               int64
	tcpPortForwardDialedDuration            time.Duration
	tcpPortForwardFailedCount               int64
	tcpPortForwardFailedDuration            time.Duration
	tcpPortForwardRejectedDialingLimitCount int64
}

type handshakeState struct {
	completed             bool
	apiProtocol           string
	apiParams             common.APIParameters
	authorizedAccessTypes []string
	authorizationsRevoked bool
	expectDomainBytes     bool
}

func newSshClient(
	sshServer *sshServer, tunnelProtocol string, geoIPData GeoIPData) *sshClient {

	runCtx, stopRunning := context.WithCancel(context.Background())

	// isFirstTunnelInSession is defaulted to true so that the pre-handshake
	// traffic rules won't apply UnthrottleFirstTunnelOnly and negate any
	// unthrottled bytes during the initial protocol negotiation.

	client := &sshClient{
		sshServer:              sshServer,
		tunnelProtocol:         tunnelProtocol,
		geoIPData:              geoIPData,
		isFirstTunnelInSession: true,
		tcpPortForwardLRU:      common.NewLRUConns(),
		signalIssueSLOKs:       make(chan struct{}, 1),
		runCtx:                 runCtx,
		stopRunning:            stopRunning,
	}

	client.tcpTrafficState.availablePortForwardCond = sync.NewCond(new(sync.Mutex))
	client.udpTrafficState.availablePortForwardCond = sync.NewCond(new(sync.Mutex))

	return client
}

func (sshClient *sshClient) run(
	baseConn net.Conn, onSSHHandshakeFinished func()) {

	// onSSHHandshakeFinished must be called even if the SSH handshake is aborted.
	defer func() {
		if onSSHHandshakeFinished != nil {
			onSSHHandshakeFinished()
		}
	}()

	// Set initial traffic rules, pre-handshake, based on currently known info.
	sshClient.setTrafficRules()

	conn := baseConn

	// Wrap the base client connection with an ActivityMonitoredConn which will
	// terminate the connection if no data is received before the deadline. This
	// timeout is in effect for the entire duration of the SSH connection. Clients
	// must actively use the connection or send SSH keep alive requests to keep
	// the connection active. Writes are not considered reliable activity indicators
	// due to buffering.

	activityConn, err := common.NewActivityMonitoredConn(
		conn,
		SSH_CONNECTION_READ_DEADLINE,
		false,
		nil,
		nil)
	if err != nil {
		conn.Close()
		if !isExpectedTunnelIOError(err) {
			log.WithContextFields(LogFields{"error": err}).Error("NewActivityMonitoredConn failed")
		}
		return
	}
	conn = activityConn

	// Further wrap the connection in a rate limiting ThrottledConn.

	throttledConn := common.NewThrottledConn(conn, sshClient.rateLimits())
	conn = throttledConn

	// Run the initial [obfuscated] SSH handshake in a goroutine so we can both
	// respect shutdownBroadcast and implement a specific handshake timeout.
	// The timeout is to reclaim network resources in case the handshake takes
	// too long.

	type sshNewServerConnResult struct {
		obfuscatedSSHConn *obfuscator.ObfuscatedSSHConn
		sshConn           *ssh.ServerConn
		channels          <-chan ssh.NewChannel
		requests          <-chan *ssh.Request
		err               error
	}

	resultChannel := make(chan *sshNewServerConnResult, 2)

	var afterFunc *time.Timer
	if sshClient.sshServer.support.Config.sshHandshakeTimeout > 0 {
		afterFunc = time.AfterFunc(sshClient.sshServer.support.Config.sshHandshakeTimeout, func() {
			resultChannel <- &sshNewServerConnResult{err: errors.New("ssh handshake timeout")}
		})
	}

	go func(baseConn, conn net.Conn) {
		sshServerConfig := &ssh.ServerConfig{
			PasswordCallback: sshClient.passwordCallback,
			AuthLogCallback:  sshClient.authLogCallback,
			ServerVersion:    sshClient.sshServer.support.Config.SSHServerVersion,
		}
		sshServerConfig.AddHostKey(sshClient.sshServer.sshHostKey)

		var err error

		if protocol.TunnelProtocolUsesObfuscatedSSH(sshClient.tunnelProtocol) {
			// With Encrypt-then-MAC hash algorithms, packet length is
			// transmitted in plaintext, which aids in traffic analysis;
			// clients may still send Encrypt-then-MAC algorithms in their
			// KEX_INIT message, but do not select these algorithms.
			//
			// The exception is TUNNEL_PROTOCOL_SSH, which is intended to appear
			// like SSH on the wire.
			sshServerConfig.NoEncryptThenMACHash = true

		} else {
			// For TUNNEL_PROTOCOL_SSH only, randomize KEX.
			if sshClient.sshServer.support.Config.ObfuscatedSSHKey != "" {
				sshServerConfig.KEXPRNGSeed, err = protocol.DeriveSSHServerKEXPRNGSeed(
					sshClient.sshServer.support.Config.ObfuscatedSSHKey)
				if err != nil {
					err = common.ContextError(err)
				}
			}
		}

		result := &sshNewServerConnResult{}

		// Wrap the connection in an SSH deobfuscator when required.

		if err == nil && protocol.TunnelProtocolUsesObfuscatedSSH(sshClient.tunnelProtocol) {
			// Note: NewObfuscatedSSHConn blocks on network I/O
			// TODO: ensure this won't block shutdown
			result.obfuscatedSSHConn, err = obfuscator.NewObfuscatedSSHConn(
				obfuscator.OBFUSCATION_CONN_MODE_SERVER,
				conn,
				sshClient.sshServer.support.Config.ObfuscatedSSHKey,
				nil, nil, nil)
			if err != nil {
				err = common.ContextError(err)
			} else {
				conn = result.obfuscatedSSHConn
			}

			// Now seed fragmentor, when present, with seed derived from
			// initial obfuscator message. See tactics.Listener.Accept.
			// This must preceed ssh.NewServerConn to ensure fragmentor
			// is seeded before downstream bytes are written.
			if err == nil && sshClient.tunnelProtocol == protocol.TUNNEL_PROTOCOL_OBFUSCATED_SSH {
				if fragmentorConn, ok := baseConn.(*fragmentor.Conn); ok {
					fragmentorPRNG, err := result.obfuscatedSSHConn.GetDerivedPRNG("server-side-fragmentor")
					if err != nil {
						err = common.ContextError(err)
					} else {
						fragmentorConn.SetPRNG(fragmentorPRNG)
					}
				}
			}
		}

		if err == nil {
			result.sshConn, result.channels, result.requests, err =
				ssh.NewServerConn(conn, sshServerConfig)
			if err != nil {
				err = common.ContextError(err)
			}
		}

		result.err = err

		resultChannel <- result

	}(baseConn, conn)

	var result *sshNewServerConnResult
	select {
	case result = <-resultChannel:
	case <-sshClient.sshServer.shutdownBroadcast:
		// Close() will interrupt an ongoing handshake
		// TODO: wait for SSH handshake goroutines to exit before returning?
		conn.Close()
		return
	}

	if afterFunc != nil {
		afterFunc.Stop()
	}

	if result.err != nil {
		conn.Close()
		// This is a Debug log due to noise. The handshake often fails due to I/O
		// errors as clients frequently interrupt connections in progress when
		// client-side load balancing completes a connection to a different server.
		log.WithContextFields(LogFields{"error": result.err}).Debug("handshake failed")
		return
	}

	// The SSH handshake has finished successfully; notify now to allow other
	// blocked SSH handshakes to proceed.
	if onSSHHandshakeFinished != nil {
		onSSHHandshakeFinished()
	}
	onSSHHandshakeFinished = nil

	sshClient.Lock()
	sshClient.sshConn = result.sshConn
	sshClient.activityConn = activityConn
	sshClient.throttledConn = throttledConn
	sshClient.Unlock()

	if !sshClient.sshServer.registerEstablishedClient(sshClient) {
		conn.Close()
		log.WithContext().Warning("register failed")
		return
	}

	sshClient.runTunnel(result.channels, result.requests)

	// Note: sshServer.unregisterEstablishedClient calls sshClient.stop(),
	// which also closes underlying transport Conn.

	sshClient.sshServer.unregisterEstablishedClient(sshClient)

	// Some conns report additional metrics. Meek conns report resiliency
	// metrics and fragmentor.Conns report fragmentor configs.
	//
	// Limitation: for meek, GetMetrics from underlying fragmentor.Conn(s)
	// should be called in order to log fragmentor metrics for meek sessions.

	var additionalMetrics []LogFields
	if metricsSource, ok := baseConn.(common.MetricsSource); ok {
		additionalMetrics = append(
			additionalMetrics, LogFields(metricsSource.GetMetrics()))
	}
	if result.obfuscatedSSHConn != nil {
		additionalMetrics = append(
			additionalMetrics, LogFields(result.obfuscatedSSHConn.GetMetrics()))
	}

	sshClient.logTunnel(additionalMetrics)

	// Transfer OSL seed state -- the OSL progress -- from the closing
	// client to the session cache so the client can resume its progress
	// if it reconnects to this same server.
	// Note: following setOSLConfig order of locking.

	sshClient.Lock()
	if sshClient.oslClientSeedState != nil {
		sshClient.sshServer.oslSessionCacheMutex.Lock()
		sshClient.oslClientSeedState.Hibernate()
		sshClient.sshServer.oslSessionCache.Set(
			sshClient.sessionID, sshClient.oslClientSeedState, cache.DefaultExpiration)
		sshClient.sshServer.oslSessionCacheMutex.Unlock()
		sshClient.oslClientSeedState = nil
	}
	sshClient.Unlock()

	// Initiate cleanup of the GeoIP session cache. To allow for post-tunnel
	// final status requests, the lifetime of cached GeoIP records exceeds the
	// lifetime of the sshClient.
	sshClient.sshServer.support.GeoIPService.MarkSessionCacheToExpire(sshClient.sessionID)
}

func (sshClient *sshClient) passwordCallback(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {

	expectedSessionIDLength := 2 * protocol.PSIPHON_API_CLIENT_SESSION_ID_LENGTH
	expectedSSHPasswordLength := 2 * SSH_PASSWORD_BYTE_LENGTH

	var sshPasswordPayload protocol.SSHPasswordPayload
	err := json.Unmarshal(password, &sshPasswordPayload)
	if err != nil {

		// Backwards compatibility case: instead of a JSON payload, older clients
		// send the hex encoded session ID prepended to the SSH password.
		// Note: there's an even older case where clients don't send any session ID,
		// but that's no longer supported.
		if len(password) == expectedSessionIDLength+expectedSSHPasswordLength {
			sshPasswordPayload.SessionId = string(password[0:expectedSessionIDLength])
			sshPasswordPayload.SshPassword = string(password[expectedSessionIDLength:])
		} else {
			return nil, common.ContextError(fmt.Errorf("invalid password payload for %q", conn.User()))
		}
	}

	if !isHexDigits(sshClient.sshServer.support.Config, sshPasswordPayload.SessionId) ||
		len(sshPasswordPayload.SessionId) != expectedSessionIDLength {
		return nil, common.ContextError(fmt.Errorf("invalid session ID for %q", conn.User()))
	}

	userOk := (subtle.ConstantTimeCompare(
		[]byte(conn.User()), []byte(sshClient.sshServer.support.Config.SSHUserName)) == 1)

	passwordOk := (subtle.ConstantTimeCompare(
		[]byte(sshPasswordPayload.SshPassword), []byte(sshClient.sshServer.support.Config.SSHPassword)) == 1)

	if !userOk || !passwordOk {
		return nil, common.ContextError(fmt.Errorf("invalid password for %q", conn.User()))
	}

	sessionID := sshPasswordPayload.SessionId

	// The GeoIP session cache will be populated if there was a previous tunnel
	// with this session ID. This will be true up to GEOIP_SESSION_CACHE_TTL, which
	// is currently much longer than the OSL session cache, another option to use if
	// the GeoIP session cache is retired (the GeoIP session cache currently only
	// supports legacy use cases).
	isFirstTunnelInSession := !sshClient.sshServer.support.GeoIPService.InSessionCache(sessionID)

	supportsServerRequests := common.Contains(
		sshPasswordPayload.ClientCapabilities, protocol.CLIENT_CAPABILITY_SERVER_REQUESTS)

	sshClient.Lock()

	// After this point, these values are read-only as they are read
	// without obtaining sshClient.Lock.
	sshClient.sessionID = sessionID
	sshClient.isFirstTunnelInSession = isFirstTunnelInSession
	sshClient.supportsServerRequests = supportsServerRequests

	geoIPData := sshClient.geoIPData

	sshClient.Unlock()

	// Store the GeoIP data associated with the session ID. This makes
	// the GeoIP data available to the web server for web API requests.
	// A cache that's distinct from the sshClient record is used to allow
	// for or post-tunnel final status requests.
	// If the client is reconnecting with the same session ID, this call
	// will undo the expiry set by MarkSessionCacheToExpire.
	sshClient.sshServer.support.GeoIPService.SetSessionCache(sessionID, geoIPData)

	return nil, nil
}

func (sshClient *sshClient) authLogCallback(conn ssh.ConnMetadata, method string, err error) {

	if err != nil {

		if method == "none" && err.Error() == "no auth passed yet" {
			// In this case, the callback invocation is noise from auth negotiation
			return
		}

		// Note: here we previously logged messages for fail2ban to act on. This is no longer
		// done as the complexity outweighs the benefits.
		//
		// - The SSH credential is not secret -- it's in the server entry. Attackers targeting
		//   the server likely already have the credential. On the other hand, random scanning and
		//   brute forcing is mitigated with high entropy random passwords, rate limiting
		//   (implemented on the host via iptables), and limited capabilities (the SSH session can
		//   only port forward).
		//
		// - fail2ban coverage was inconsistent; in the case of an unfronted meek protocol through
		//   an upstream proxy, the remote address is the upstream proxy, which should not be blocked.
		//   The X-Forwarded-For header cant be used instead as it may be forged and used to get IPs
		//   deliberately blocked; and in any case fail2ban adds iptables rules which can only block
		//   by direct remote IP, not by original client IP. Fronted meek has the same iptables issue.
		//
		// Random scanning and brute forcing of port 22 will result in log noise. To mitigate this,
		// not every authentication failure is logged. A summary log is emitted periodically to
		// retain some record of this activity in case this is relevant to, e.g., a performance
		// investigation.

		atomic.AddInt64(&sshClient.sshServer.authFailedCount, 1)

		lastAuthLog := monotime.Time(atomic.LoadInt64(&sshClient.sshServer.lastAuthLog))
		if monotime.Since(lastAuthLog) > SSH_AUTH_LOG_PERIOD {
			now := int64(monotime.Now())
			if atomic.CompareAndSwapInt64(&sshClient.sshServer.lastAuthLog, int64(lastAuthLog), now) {
				count := atomic.SwapInt64(&sshClient.sshServer.authFailedCount, 0)
				log.WithContextFields(
					LogFields{"lastError": err, "failedCount": count}).Warning("authentication failures")
			}
		}

		log.WithContextFields(LogFields{"error": err, "method": method}).Debug("authentication failed")

	} else {

		log.WithContextFields(LogFields{"error": err, "method": method}).Debug("authentication success")
	}
}

// stop signals the ssh connection to shutdown. After sshConn() returns,
// the connection has terminated but sshClient.run() may still be
// running and in the process of exiting.
func (sshClient *sshClient) stop() {
	sshClient.sshConn.Close()
	sshClient.sshConn.Wait()
}

// runTunnel handles/dispatches new channels and new requests from the client.
// When the SSH client connection closes, both the channels and requests channels
// will close and runTunnel will exit.
func (sshClient *sshClient) runTunnel(
	channels <-chan ssh.NewChannel,
	requests <-chan *ssh.Request) {

	waitGroup := new(sync.WaitGroup)

	// Start client SSH API request handler

	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()
		sshClient.handleSSHRequests(requests)
	}()

	// Start OSL sender

	if sshClient.supportsServerRequests {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			sshClient.runOSLSender()
		}()
	}

	// Start the TCP port forward manager

	// The queue size is set to the traffic rules (MaxTCPPortForwardCount +
	// MaxTCPDialingPortForwardCount), which is a reasonable indication of resource
	// limits per client; when that value is not set, a default is used.
	// A limitation: this queue size is set once and doesn't change, for this client,
	// when traffic rules are reloaded.
	queueSize := sshClient.getTCPPortForwardQueueSize()
	if queueSize == 0 {
		queueSize = SSH_TCP_PORT_FORWARD_QUEUE_SIZE
	}
	newTCPPortForwards := make(chan *newTCPPortForward, queueSize)

	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()
		sshClient.handleTCPPortForwards(waitGroup, newTCPPortForwards)
	}()

	// Handle new channel (port forward) requests from the client.

	for newChannel := range channels {
		switch newChannel.ChannelType() {
		case protocol.RANDOM_STREAM_CHANNEL_TYPE:
			sshClient.handleNewRandomStreamChannel(waitGroup, newChannel)
		case protocol.PACKET_TUNNEL_CHANNEL_TYPE:
			sshClient.handleNewPacketTunnelChannel(waitGroup, newChannel)
		case "direct-tcpip":
			sshClient.handleNewTCPPortForwardChannel(waitGroup, newChannel, newTCPPortForwards)
		default:
			sshClient.rejectNewChannel(newChannel,
				fmt.Sprintf("unknown or unsupported channel type: %s", newChannel.ChannelType()))
		}
	}

	// The channel loop is interrupted by a client
	// disconnect or by calling sshClient.stop().

	// Stop the TCP port forward manager
	close(newTCPPortForwards)

	// Stop all other worker goroutines
	sshClient.stopRunning()

	if sshClient.sshServer.support.Config.RunPacketTunnel {
		// PacketTunnelServer.ClientDisconnected stops packet tunnel workers.
		sshClient.sshServer.support.PacketTunnelServer.ClientDisconnected(
			sshClient.sessionID)
	}

	waitGroup.Wait()

	sshClient.cleanupAuthorizations()
}

func (sshClient *sshClient) handleSSHRequests(requests <-chan *ssh.Request) {

	for request := range requests {

		// Requests are processed serially; API responses must be sent in request order.

		var responsePayload []byte
		var err error

		if request.Type == "keepalive@openssh.com" {

			// SSH keep alive round trips are used as speed test samples.
			responsePayload, err = tactics.MakeSpeedTestResponse(
				SSH_KEEP_ALIVE_PAYLOAD_MIN_BYTES, SSH_KEEP_ALIVE_PAYLOAD_MAX_BYTES)

		} else {

			// All other requests are assumed to be API requests.

			sshClient.Lock()
			authorizedAccessTypes := sshClient.handshakeState.authorizedAccessTypes
			sshClient.Unlock()

			// Note: unlock before use is only safe as long as referenced sshClient data,
			// such as slices in handshakeState, is read-only after initially set.

			responsePayload, err = sshAPIRequestHandler(
				sshClient.sshServer.support,
				sshClient.geoIPData,
				authorizedAccessTypes,
				request.Type,
				request.Payload)
		}

		if err == nil {
			err = request.Reply(true, responsePayload)
		} else {
			log.WithContextFields(LogFields{"error": err}).Warning("request failed")
			err = request.Reply(false, nil)
		}
		if err != nil {
			if !isExpectedTunnelIOError(err) {
				log.WithContextFields(LogFields{"error": err}).Warning("response failed")
			}
		}

	}

}

type newTCPPortForward struct {
	enqueueTime   monotime.Time
	hostToConnect string
	portToConnect int
	newChannel    ssh.NewChannel
}

func (sshClient *sshClient) handleTCPPortForwards(
	waitGroup *sync.WaitGroup,
	newTCPPortForwards chan *newTCPPortForward) {

	// Lifecycle of a TCP port forward:
	//
	// 1. A "direct-tcpip" SSH request is received from the client.
	//
	//    A new TCP port forward request is enqueued. The queue delivers TCP port
	//    forward requests to the TCP port forward manager, which enforces the TCP
	//    port forward dial limit.
	//
	//    Enqueuing new requests allows for reading further SSH requests from the
	//    client without blocking when the dial limit is hit; this is to permit new
	//    UDP/udpgw port forwards to be restablished without delay. The maximum size
	//    of the queue enforces a hard cap on resources consumed by a client in the
	//    pre-dial phase. When the queue is full, new TCP port forwards are
	//    immediately rejected.
	//
	// 2. The TCP port forward manager dequeues the request.
	//
	//    The manager calls dialingTCPPortForward(), which increments
	//    concurrentDialingPortForwardCount, and calls
	//    isTCPDialingPortForwardLimitExceeded() to check the concurrent dialing
	//    count.
	//
	//    The manager enforces the concurrent TCP dial limit: when at the limit, the
	//    manager blocks waiting for the number of dials to drop below the limit before
	//    dispatching the request to handleTCPPortForward(), which will run in its own
	//    goroutine and will dial and relay the port forward.
	//
	//    The block delays the current request and also halts dequeuing of subsequent
	//    requests and could ultimately cause requests to be immediately rejected if
	//    the queue fills. These actions are intended to apply back pressure when
	//    upstream network resources are impaired.
	//
	//    The time spent in the queue is deducted from the port forward's dial timeout.
	//    The time spent blocking while at the dial limit is similarly deducted from
	//    the dial timeout. If the dial timeout has expired before the dial begins, the
	//    port forward is rejected and a stat is recorded.
	//
	// 3. handleTCPPortForward() performs the port forward dial and relaying.
	//
	//     a. Dial the target, using the dial timeout remaining after queue and blocking
	//        time is deducted.
	//
	//     b. If the dial fails, call abortedTCPPortForward() to decrement
	//        concurrentDialingPortForwardCount, freeing up a dial slot.
	//
	//     c. If the dial succeeds, call establishedPortForward(), which decrements
	//        concurrentDialingPortForwardCount and increments concurrentPortForwardCount,
	//        the "established" port forward count.
	//
	//    d. Check isPortForwardLimitExceeded(), which enforces the configurable limit on
	//       concurrentPortForwardCount, the number of _established_ TCP port forwards.
	//       If the limit is exceeded, the LRU established TCP port forward is closed and
	//       the newly established TCP port forward proceeds. This LRU logic allows some
	//       dangling resource consumption (e.g., TIME_WAIT) while providing a better
	//       experience for clients.
	//
	//    e. Relay data.
	//
	//    f. Call closedPortForward() which decrements concurrentPortForwardCount and
	//       records bytes transferred.

	for newPortForward := range newTCPPortForwards {

		remainingDialTimeout :=
			time.Duration(sshClient.getDialTCPPortForwardTimeoutMilliseconds())*time.Millisecond -
				monotime.Since(newPortForward.enqueueTime)

		if remainingDialTimeout <= 0 {
			sshClient.updateQualityMetricsWithRejectedDialingLimit()
			sshClient.rejectNewChannel(
				newPortForward.newChannel, "TCP port forward timed out in queue")
			continue
		}

		// Reserve a TCP dialing slot.
		//
		// TOCTOU note: important to increment counts _before_ checking limits; otherwise,
		// the client could potentially consume excess resources by initiating many port
		// forwards concurrently.

		sshClient.dialingTCPPortForward()

		// When max dials are in progress, wait up to remainingDialTimeout for dialing
		// to become available. This blocks all dequeing.

		if sshClient.isTCPDialingPortForwardLimitExceeded() {
			blockStartTime := monotime.Now()
			ctx, cancelCtx := context.WithTimeout(sshClient.runCtx, remainingDialTimeout)
			sshClient.setTCPPortForwardDialingAvailableSignal(cancelCtx)
			<-ctx.Done()
			sshClient.setTCPPortForwardDialingAvailableSignal(nil)
			cancelCtx() // "must be called or the new context will remain live until its parent context is cancelled"
			remainingDialTimeout -= monotime.Since(blockStartTime)
		}

		if remainingDialTimeout <= 0 {

			// Release the dialing slot here since handleTCPChannel() won't be called.
			sshClient.abortedTCPPortForward()

			sshClient.updateQualityMetricsWithRejectedDialingLimit()
			sshClient.rejectNewChannel(
				newPortForward.newChannel, "TCP port forward timed out before dialing")
			continue
		}

		// Dial and relay the TCP port forward. handleTCPChannel is run in its own worker goroutine.
		// handleTCPChannel will release the dialing slot reserved by dialingTCPPortForward(); and
		// will deal with remainingDialTimeout <= 0.

		waitGroup.Add(1)
		go func(remainingDialTimeout time.Duration, newPortForward *newTCPPortForward) {
			defer waitGroup.Done()
			sshClient.handleTCPChannel(
				remainingDialTimeout,
				newPortForward.hostToConnect,
				newPortForward.portToConnect,
				newPortForward.newChannel)
		}(remainingDialTimeout, newPortForward)
	}
}

func (sshClient *sshClient) handleNewRandomStreamChannel(
	waitGroup *sync.WaitGroup, newChannel ssh.NewChannel) {

	// A random stream channel returns the requested number of bytes -- random
	// bytes -- to the client while also consuming and discarding bytes sent
	// by the client.
	//
	// One use case for the random stream channel is a liveness test that the
	// client performs to confirm that the tunnel is live. As the liveness
	// test is performed in the concurrent establishment phase, before
	// selecting a single candidate for handshake, the random stream channel
	// is available pre-handshake, albeit with additional restrictions.
	//
	// The random stream is subject to throttling in traffic rules; for
	// unthrottled liveness tests, set initial   Read/WriteUnthrottledBytes as
	// required. The random stream maximum count and response size cap
	// mitigate clients abusing the facility to waste server resources.
	//
	// Like all other channels, this channel type is handled asynchronously,
	// so it's possible to run at any point in the tunnel lifecycle.
	//
	// Up/downstream byte counts don't include SSH packet and request
	// marshalling overhead.

	var request protocol.RandomStreamRequest
	err := json.Unmarshal(newChannel.ExtraData(), &request)
	if err != nil {
		sshClient.rejectNewChannel(newChannel, fmt.Sprintf("invalid request: %s", err))
		return
	}

	if request.UpstreamBytes > RANDOM_STREAM_MAX_BYTES {
		sshClient.rejectNewChannel(newChannel,
			fmt.Sprintf("invalid upstream bytes: %d", request.UpstreamBytes))
		return
	}

	if request.DownstreamBytes > RANDOM_STREAM_MAX_BYTES {
		sshClient.rejectNewChannel(newChannel,
			fmt.Sprintf("invalid downstream bytes: %d", request.DownstreamBytes))
		return
	}

	var metrics *randomStreamMetrics

	sshClient.Lock()

	if !sshClient.handshakeState.completed {
		metrics = &sshClient.preHandshakeRandomStreamMetrics
	} else {
		metrics = &sshClient.postHandshakeRandomStreamMetrics
	}

	countOk := true
	if !sshClient.handshakeState.completed &&
		metrics.count >= PRE_HANDSHAKE_RANDOM_STREAM_MAX_COUNT {
		countOk = false
	} else {
		metrics.count++
	}

	sshClient.Unlock()

	if !countOk {
		sshClient.rejectNewChannel(newChannel, "max count exceeded")
		return
	}

	channel, requests, err := newChannel.Accept()
	if err != nil {
		if !isExpectedTunnelIOError(err) {
			log.WithContextFields(LogFields{"error": err}).Warning("accept new channel failed")
		}
		return
	}
	go ssh.DiscardRequests(requests)

	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()

		received := 0
		sent := 0

		if request.UpstreamBytes > 0 {
			n, err := io.CopyN(ioutil.Discard, channel, int64(request.UpstreamBytes))
			received = int(n)
			if err != nil {
				if !isExpectedTunnelIOError(err) {
					log.WithContextFields(LogFields{"error": err}).Warning("receive failed")
				}
				// Fall through and record any bytes received...
			}
		}

		if request.DownstreamBytes > 0 {
			n, err := io.CopyN(channel, rand.Reader, int64(request.DownstreamBytes))
			sent = int(n)
			if err != nil {
				if !isExpectedTunnelIOError(err) {
					log.WithContextFields(LogFields{"error": err}).Warning("send failed")
				}
			}
		}

		sshClient.Lock()
		metrics.upstreamBytes += request.UpstreamBytes
		metrics.receivedUpstreamBytes += received
		metrics.downstreamBytes += request.DownstreamBytes
		metrics.sentDownstreamBytes += sent
		sshClient.Unlock()

		channel.Close()
	}()
}

func (sshClient *sshClient) handleNewPacketTunnelChannel(
	waitGroup *sync.WaitGroup, newChannel ssh.NewChannel) {

	// packet tunnel channels are handled by the packet tunnel server
	// component. Each client may have at most one packet tunnel channel.

	if !sshClient.sshServer.support.Config.RunPacketTunnel {
		sshClient.rejectNewChannel(newChannel, "unsupported packet tunnel channel type")
		return
	}

	// Accept this channel immediately. This channel will replace any
	// previously existing packet tunnel channel for this client.

	packetTunnelChannel, requests, err := newChannel.Accept()
	if err != nil {
		if !isExpectedTunnelIOError(err) {
			log.WithContextFields(LogFields{"error": err}).Warning("accept new channel failed")
		}
		return
	}
	go ssh.DiscardRequests(requests)

	sshClient.setPacketTunnelChannel(packetTunnelChannel)

	// PacketTunnelServer will run the client's packet tunnel. If necessary, ClientConnected
	// will stop packet tunnel workers for any previous packet tunnel channel.

	checkAllowedTCPPortFunc := func(upstreamIPAddress net.IP, port int) bool {
		return sshClient.isPortForwardPermitted(portForwardTypeTCP, upstreamIPAddress, port)
	}

	checkAllowedUDPPortFunc := func(upstreamIPAddress net.IP, port int) bool {
		return sshClient.isPortForwardPermitted(portForwardTypeUDP, upstreamIPAddress, port)
	}

	flowActivityUpdaterMaker := func(
		upstreamHostname string, upstreamIPAddress net.IP) []tun.FlowActivityUpdater {

		var updaters []tun.FlowActivityUpdater
		oslUpdater := sshClient.newClientSeedPortForward(upstreamIPAddress)
		if oslUpdater != nil {
			updaters = append(updaters, oslUpdater)
		}
		return updaters
	}

	metricUpdater := func(
		TCPApplicationBytesDown, TCPApplicationBytesUp,
		UDPApplicationBytesDown, UDPApplicationBytesUp int64) {

		sshClient.Lock()
		sshClient.tcpTrafficState.bytesDown += TCPApplicationBytesDown
		sshClient.tcpTrafficState.bytesUp += TCPApplicationBytesUp
		sshClient.udpTrafficState.bytesDown += UDPApplicationBytesDown
		sshClient.udpTrafficState.bytesUp += UDPApplicationBytesUp
		sshClient.Unlock()
	}

	err = sshClient.sshServer.support.PacketTunnelServer.ClientConnected(
		sshClient.sessionID,
		packetTunnelChannel,
		checkAllowedTCPPortFunc,
		checkAllowedUDPPortFunc,
		flowActivityUpdaterMaker,
		metricUpdater)
	if err != nil {
		log.WithContextFields(LogFields{"error": err}).Warning("start packet tunnel client failed")
		sshClient.setPacketTunnelChannel(nil)
	}
}

func (sshClient *sshClient) handleNewTCPPortForwardChannel(
	waitGroup *sync.WaitGroup, newChannel ssh.NewChannel,
	newTCPPortForwards chan *newTCPPortForward) {

	// udpgw client connections are dispatched immediately (clients use this for
	// DNS, so it's essential to not block; and only one udpgw connection is
	// retained at a time).
	//
	// All other TCP port forwards are dispatched via the TCP port forward
	// manager queue.

	// http://tools.ietf.org/html/rfc4254#section-7.2
	var directTcpipExtraData struct {
		HostToConnect       string
		PortToConnect       uint32
		OriginatorIPAddress string
		OriginatorPort      uint32
	}

	err := ssh.Unmarshal(newChannel.ExtraData(), &directTcpipExtraData)
	if err != nil {
		sshClient.rejectNewChannel(newChannel, "invalid extra data")
		return
	}

	// Intercept TCP port forwards to a specified udpgw server and handle directly.
	// TODO: also support UDP explicitly, e.g. with a custom "direct-udp" channel type?
	isUDPChannel := sshClient.sshServer.support.Config.UDPInterceptUdpgwServerAddress != "" &&
		sshClient.sshServer.support.Config.UDPInterceptUdpgwServerAddress ==
			net.JoinHostPort(directTcpipExtraData.HostToConnect, strconv.Itoa(int(directTcpipExtraData.PortToConnect)))

	if isUDPChannel {

		// Dispatch immediately. handleUDPChannel runs the udpgw protocol in its
		// own worker goroutine.

		waitGroup.Add(1)
		go func(channel ssh.NewChannel) {
			defer waitGroup.Done()
			sshClient.handleUDPChannel(channel)
		}(newChannel)

	} else {

		// Dispatch via TCP port forward manager. When the queue is full, the channel
		// is immediately rejected.

		tcpPortForward := &newTCPPortForward{
			enqueueTime:   monotime.Now(),
			hostToConnect: directTcpipExtraData.HostToConnect,
			portToConnect: int(directTcpipExtraData.PortToConnect),
			newChannel:    newChannel,
		}

		select {
		case newTCPPortForwards <- tcpPortForward:
		default:
			sshClient.updateQualityMetricsWithRejectedDialingLimit()
			sshClient.rejectNewChannel(newChannel, "TCP port forward dial queue full")
		}
	}
}

func (sshClient *sshClient) cleanupAuthorizations() {
	sshClient.Lock()

	if sshClient.releaseAuthorizations != nil {
		sshClient.releaseAuthorizations()
	}

	if sshClient.stopTimer != nil {
		sshClient.stopTimer.Stop()
	}

	sshClient.Unlock()
}

// setPacketTunnelChannel sets the single packet tunnel channel
// for this sshClient. Any existing packet tunnel channel is
// closed.
func (sshClient *sshClient) setPacketTunnelChannel(channel ssh.Channel) {
	sshClient.Lock()
	if sshClient.packetTunnelChannel != nil {
		sshClient.packetTunnelChannel.Close()
	}
	sshClient.packetTunnelChannel = channel
	sshClient.Unlock()
}

// setUDPChannel sets the single UDP channel for this sshClient.
// Each sshClient may have only one concurrent UDP channel. Each
// UDP channel multiplexes many UDP port forwards via the udpgw
// protocol. Any existing UDP channel is closed.
func (sshClient *sshClient) setUDPChannel(channel ssh.Channel) {
	sshClient.Lock()
	if sshClient.udpChannel != nil {
		sshClient.udpChannel.Close()
	}
	sshClient.udpChannel = channel
	sshClient.Unlock()
}

var serverTunnelStatParams = append(
	[]requestParamSpec{
		{"last_connected", isLastConnected, requestParamOptional},
		{"establishment_duration", isIntString, requestParamOptional}},
	baseRequestParams...)

func (sshClient *sshClient) logTunnel(additionalMetrics []LogFields) {

	// Note: reporting duration based on last confirmed data transfer, which
	// is reads for sshClient.activityConn.GetActiveDuration(), and not
	// connection closing is important for protocols such as meek. For
	// meek, the connection remains open until the HTTP session expires,
	// which may be some time after the tunnel has closed. (The meek
	// protocol has no allowance for signalling payload EOF, and even if
	// it did the client may not have the opportunity to send a final
	// request with an EOF flag set.)

	sshClient.Lock()

	logFields := getRequestLogFields(
		"server_tunnel",
		sshClient.geoIPData,
		sshClient.handshakeState.authorizedAccessTypes,
		sshClient.handshakeState.apiParams,
		serverTunnelStatParams)

	// "relay_protocol" is sent with handshake API parameters. In pre-
	// handshake logTunnel cases, this value is not yet known. As
	// sshClient.tunnelProtocol is authoritative, set this value
	// unconditionally, overwriting any value from handshake.
	logFields["relay_protocol"] = sshClient.tunnelProtocol

	logFields["session_id"] = sshClient.sessionID
	logFields["handshake_completed"] = sshClient.handshakeState.completed
	logFields["start_time"] = sshClient.activityConn.GetStartTime()
	logFields["duration"] = sshClient.activityConn.GetActiveDuration() / time.Millisecond
	logFields["bytes_up_tcp"] = sshClient.tcpTrafficState.bytesUp
	logFields["bytes_down_tcp"] = sshClient.tcpTrafficState.bytesDown
	logFields["peak_concurrent_dialing_port_forward_count_tcp"] = sshClient.tcpTrafficState.peakConcurrentDialingPortForwardCount
	logFields["peak_concurrent_port_forward_count_tcp"] = sshClient.tcpTrafficState.peakConcurrentPortForwardCount
	logFields["total_port_forward_count_tcp"] = sshClient.tcpTrafficState.totalPortForwardCount
	logFields["bytes_up_udp"] = sshClient.udpTrafficState.bytesUp
	logFields["bytes_down_udp"] = sshClient.udpTrafficState.bytesDown
	// sshClient.udpTrafficState.peakConcurrentDialingPortForwardCount isn't meaningful
	logFields["peak_concurrent_port_forward_count_udp"] = sshClient.udpTrafficState.peakConcurrentPortForwardCount
	logFields["total_port_forward_count_udp"] = sshClient.udpTrafficState.totalPortForwardCount

	logFields["pre_handshake_random_stream_count"] = sshClient.preHandshakeRandomStreamMetrics.count
	logFields["pre_handshake_random_stream_upstream_bytes"] = sshClient.preHandshakeRandomStreamMetrics.upstreamBytes
	logFields["pre_handshake_random_stream_received_upstream_bytes"] = sshClient.preHandshakeRandomStreamMetrics.receivedUpstreamBytes
	logFields["pre_handshake_random_stream_downstream_bytes"] = sshClient.preHandshakeRandomStreamMetrics.downstreamBytes
	logFields["pre_handshake_random_stream_sent_downstream_bytes"] = sshClient.preHandshakeRandomStreamMetrics.sentDownstreamBytes
	logFields["random_stream_count"] = sshClient.postHandshakeRandomStreamMetrics.count
	logFields["random_stream_upstream_bytes"] = sshClient.postHandshakeRandomStreamMetrics.upstreamBytes
	logFields["random_stream_received_upstream_bytes"] = sshClient.postHandshakeRandomStreamMetrics.receivedUpstreamBytes
	logFields["random_stream_downstream_bytes"] = sshClient.postHandshakeRandomStreamMetrics.downstreamBytes
	logFields["random_stream_sent_downstream_bytes"] = sshClient.postHandshakeRandomStreamMetrics.sentDownstreamBytes

	// Pre-calculate a total-tunneled-bytes field. This total is used
	// extensively in analytics and is more performant when pre-calculated.
	logFields["bytes"] = sshClient.tcpTrafficState.bytesUp +
		sshClient.tcpTrafficState.bytesDown +
		sshClient.udpTrafficState.bytesUp +
		sshClient.udpTrafficState.bytesDown

	// Merge in additional metrics from the optional metrics source
	for _, metrics := range additionalMetrics {
		for name, value := range metrics {
			// Don't overwrite any basic fields
			if logFields[name] == nil {
				logFields[name] = value
			}
		}
	}

	sshClient.Unlock()

	// Note: unlock before use is only safe as long as referenced sshClient data,
	// such as slices in handshakeState, is read-only after initially set.

	log.LogRawFieldsWithTimestamp(logFields)
}

var blocklistHitsStatParams = []requestParamSpec{
	{"propagation_channel_id", isHexDigits, 0},
	{"sponsor_id", isHexDigits, 0},
	{"client_version", isIntString, requestParamLogStringAsInt},
	{"client_platform", isClientPlatform, 0},
	{"client_build_rev", isHexDigits, requestParamOptional},
	{"tunnel_whole_device", isBooleanFlag, requestParamOptional | requestParamLogFlagAsBool},
	{"device_region", isAnyString, requestParamOptional},
	{"egress_region", isRegionCode, requestParamOptional},
	{"session_id", isHexDigits, 0},
	{"last_connected", isLastConnected, requestParamOptional},
}

func (sshClient *sshClient) logBlocklistHits(remoteIP net.IP, tags []BlocklistTag) {

	sshClient.Lock()

	logFields := getRequestLogFields(
		"server_blocklist_hit",
		sshClient.geoIPData,
		sshClient.handshakeState.authorizedAccessTypes,
		sshClient.handshakeState.apiParams,
		blocklistHitsStatParams)

	logFields["session_id"] = sshClient.sessionID

	// Note: see comment in logTunnel regarding unlock and concurrent access.

	sshClient.Unlock()

	for _, tag := range tags {
		logFields["blocklist_ip_address"] = remoteIP.String()
		logFields["blocklist_source"] = tag.Source
		logFields["blocklist_subject"] = tag.Subject

		log.LogRawFieldsWithTimestamp(logFields)
	}
}

func (sshClient *sshClient) runOSLSender() {

	for {
		// Await a signal that there are SLOKs to send
		// TODO: use reflect.SelectCase, and optionally await timer here?
		select {
		case <-sshClient.signalIssueSLOKs:
		case <-sshClient.runCtx.Done():
			return
		}

		retryDelay := SSH_SEND_OSL_INITIAL_RETRY_DELAY
		for {
			err := sshClient.sendOSLRequest()
			if err == nil {
				break
			}
			if !isExpectedTunnelIOError(err) {
				log.WithContextFields(LogFields{"error": err}).Warning("sendOSLRequest failed")
			}

			// If the request failed, retry after a delay (with exponential backoff)
			// or when signaled that there are additional SLOKs to send
			retryTimer := time.NewTimer(retryDelay)
			select {
			case <-retryTimer.C:
			case <-sshClient.signalIssueSLOKs:
			case <-sshClient.runCtx.Done():
				retryTimer.Stop()
				return
			}
			retryTimer.Stop()
			retryDelay *= SSH_SEND_OSL_RETRY_FACTOR
		}
	}
}

// sendOSLRequest will invoke osl.GetSeedPayload to issue SLOKs and
// generate a payload, and send an OSL request to the client when
// there are new SLOKs in the payload.
func (sshClient *sshClient) sendOSLRequest() error {

	seedPayload := sshClient.getOSLSeedPayload()

	// Don't send when no SLOKs. This will happen when signalIssueSLOKs
	// is received but no new SLOKs are issued.
	if len(seedPayload.SLOKs) == 0 {
		return nil
	}

	oslRequest := protocol.OSLRequest{
		SeedPayload: seedPayload,
	}
	requestPayload, err := json.Marshal(oslRequest)
	if err != nil {
		return common.ContextError(err)
	}

	ok, _, err := sshClient.sshConn.SendRequest(
		protocol.PSIPHON_API_OSL_REQUEST_NAME,
		true,
		requestPayload)
	if err != nil {
		return common.ContextError(err)
	}
	if !ok {
		return common.ContextError(errors.New("client rejected request"))
	}

	sshClient.clearOSLSeedPayload()

	return nil
}

func (sshClient *sshClient) rejectNewChannel(newChannel ssh.NewChannel, logMessage string) {

	// We always return the reject reason "Prohibited":
	// - Traffic rules and connection limits may prohibit the connection.
	// - External firewall rules may prohibit the connection, and this is not currently
	//   distinguishable from other failure modes.
	// - We limit the failure information revealed to the client.
	reason := ssh.Prohibited

	// Note: Debug level, as logMessage may contain user traffic destination address information
	log.WithContextFields(
		LogFields{
			"channelType":  newChannel.ChannelType(),
			"logMessage":   logMessage,
			"rejectReason": reason.String(),
		}).Debug("reject new channel")

	// Note: logMessage is internal, for logging only; just the reject reason is sent to the client.
	newChannel.Reject(reason, reason.String())
}

// setHandshakeState records that a client has completed a handshake API request.
// Some parameters from the handshake request may be used in future traffic rule
// selection. Port forwards are disallowed until a handshake is complete. The
// handshake parameters are included in the session summary log recorded in
// sshClient.stop().
func (sshClient *sshClient) setHandshakeState(
	state handshakeState,
	authorizations []string) ([]string, []string, error) {

	sshClient.Lock()
	completed := sshClient.handshakeState.completed
	if !completed {
		sshClient.handshakeState = state
	}
	sshClient.Unlock()

	// Client must only perform one handshake
	if completed {
		return nil, nil, common.ContextError(errors.New("handshake already completed"))
	}

	// Verify the authorizations submitted by the client. Verified, active
	// (non-expired) access types will be available for traffic rules
	// filtering.
	//
	// When an authorization is active but expires while the client is
	// connected, the client is disconnected to ensure the access is reset.
	// This is implemented by setting a timer to perform the disconnect at the
	// expiry time of the soonest expiring authorization.
	//
	// sshServer.authorizationSessionIDs tracks the unique mapping of active
	// authorization IDs to client session IDs  and is used to detect and
	// prevent multiple malicious clients from reusing a single authorization
	// (within the scope of this server).

	// authorizationIDs and authorizedAccessTypes are returned to the client
	// and logged, respectively; initialize to empty lists so the
	// protocol/logs don't need to handle 'null' values.
	authorizationIDs := make([]string, 0)
	authorizedAccessTypes := make([]string, 0)
	var stopTime time.Time

	for i, authorization := range authorizations {

		// This sanity check mitigates malicious clients causing excess CPU use.
		if i >= MAX_AUTHORIZATIONS {
			log.WithContext().Warning("too many authorizations")
			break
		}

		verifiedAuthorization, err := accesscontrol.VerifyAuthorization(
			&sshClient.sshServer.support.Config.AccessControlVerificationKeyRing,
			authorization)

		if err != nil {
			log.WithContextFields(
				LogFields{"error": err}).Warning("verify authorization failed")
			continue
		}

		authorizationID := base64.StdEncoding.EncodeToString(verifiedAuthorization.ID)

		if common.Contains(authorizedAccessTypes, verifiedAuthorization.AccessType) {
			log.WithContextFields(
				LogFields{"accessType": verifiedAuthorization.AccessType}).Warning("duplicate authorization access type")
			continue
		}

		authorizationIDs = append(authorizationIDs, authorizationID)
		authorizedAccessTypes = append(authorizedAccessTypes, verifiedAuthorization.AccessType)

		if stopTime.IsZero() || stopTime.After(verifiedAuthorization.Expires) {
			stopTime = verifiedAuthorization.Expires
		}
	}

	// Associate all verified authorizationIDs with this client's session ID.
	// Handle cases where previous associations exist:
	//
	// - Multiple malicious clients reusing a single authorization. In this
	//   case, authorizations are revoked from the previous client.
	//
	// - The client reconnected with a new session ID due to user toggling.
	//   This case is expected due to server affinity. This cannot be
	//   distinguished from the previous case and the same action is taken;
	//   this will have no impact on a legitimate client as the previous
	//   session is dangling.
	//
	// - The client automatically reconnected with the same session ID. This
	//   case is not expected as sshServer.registerEstablishedClient
	//   synchronously calls sshClient.releaseAuthorizations; as a safe guard,
	//   this case is distinguished and no revocation action is taken.

	sshClient.sshServer.authorizationSessionIDsMutex.Lock()
	for _, authorizationID := range authorizationIDs {
		sessionID, ok := sshClient.sshServer.authorizationSessionIDs[authorizationID]
		if ok && sessionID != sshClient.sessionID {

			log.WithContextFields(
				LogFields{"authorizationID": authorizationID}).Warning("duplicate active authorization")

			// Invoke asynchronously to avoid deadlocks.
			// TODO: invoke only once for each distinct sessionID?
			go sshClient.sshServer.revokeClientAuthorizations(sessionID)
		}
		sshClient.sshServer.authorizationSessionIDs[authorizationID] = sshClient.sessionID
	}
	sshClient.sshServer.authorizationSessionIDsMutex.Unlock()

	if len(authorizationIDs) > 0 {

		sshClient.Lock()

		// Make the authorizedAccessTypes available for traffic rules filtering.

		sshClient.handshakeState.authorizedAccessTypes = authorizedAccessTypes

		// On exit, sshClient.runTunnel will call releaseAuthorizations, which
		// will release the authorization IDs so the client can reconnect and
		// present the same authorizations again. sshClient.runTunnel will
		// also cancel the stopTimer in case it has not yet fired.
		// Note: termination of the stopTimer goroutine is not synchronized.

		sshClient.releaseAuthorizations = func() {
			sshClient.sshServer.authorizationSessionIDsMutex.Lock()
			for _, authorizationID := range authorizationIDs {
				sessionID, ok := sshClient.sshServer.authorizationSessionIDs[authorizationID]
				if ok && sessionID == sshClient.sessionID {
					delete(sshClient.sshServer.authorizationSessionIDs, authorizationID)
				}
			}
			sshClient.sshServer.authorizationSessionIDsMutex.Unlock()
		}

		sshClient.stopTimer = time.AfterFunc(
			stopTime.Sub(time.Now()),
			func() {
				sshClient.stop()
			})

		sshClient.Unlock()
	}

	sshClient.setTrafficRules()
	sshClient.setOSLConfig()

	return authorizationIDs, authorizedAccessTypes, nil
}

// getHandshaked returns whether the client has completed a handshake API
// request and whether the traffic rules that were selected after the
// handshake immediately exhaust the client.
//
// When the client is immediately exhausted it will be closed; but this
// takes effect asynchronously. The "exhausted" return value is used to
// prevent API requests by clients that will close.
func (sshClient *sshClient) getHandshaked() (bool, bool) {
	sshClient.Lock()
	defer sshClient.Unlock()

	completed := sshClient.handshakeState.completed

	exhausted := false

	// Notes:
	// - "Immediately exhausted" is when CloseAfterExhausted is set and
	//   either ReadUnthrottledBytes or WriteUnthrottledBytes starts from
	//   0, so no bytes would be read or written. This check does not
	//   examine whether 0 bytes _remain_ in the ThrottledConn.
	// - This check is made against the current traffic rules, which
	//   could have changed in a hot reload since the handshake.

	if completed &&
		*sshClient.trafficRules.RateLimits.CloseAfterExhausted == true &&
		(*sshClient.trafficRules.RateLimits.ReadUnthrottledBytes == 0 ||
			*sshClient.trafficRules.RateLimits.WriteUnthrottledBytes == 0) {

		exhausted = true
	}

	return completed, exhausted
}

func (sshClient *sshClient) updateAPIParameters(
	apiParams common.APIParameters) {

	sshClient.Lock()
	defer sshClient.Unlock()

	// Only update after handshake has initialized API params.
	if !sshClient.handshakeState.completed {
		return
	}

	for name, value := range apiParams {
		sshClient.handshakeState.apiParams[name] = value
	}
}

func (sshClient *sshClient) expectDomainBytes() bool {
	sshClient.Lock()
	defer sshClient.Unlock()

	return sshClient.handshakeState.expectDomainBytes
}

// setTrafficRules resets the client's traffic rules based on the latest server config
// and client properties. As sshClient.trafficRules may be reset by a concurrent
// goroutine, trafficRules must only be accessed within the sshClient mutex.
func (sshClient *sshClient) setTrafficRules() {
	sshClient.Lock()
	defer sshClient.Unlock()

	sshClient.trafficRules = sshClient.sshServer.support.TrafficRulesSet.GetTrafficRules(
		sshClient.isFirstTunnelInSession,
		sshClient.tunnelProtocol,
		sshClient.geoIPData,
		sshClient.handshakeState)

	if sshClient.throttledConn != nil {
		// Any existing throttling state is reset.
		sshClient.throttledConn.SetLimits(
			sshClient.trafficRules.RateLimits.CommonRateLimits())
	}
}

// setOSLConfig resets the client's OSL seed state based on the latest OSL config
// As sshClient.oslClientSeedState may be reset by a concurrent goroutine,
// oslClientSeedState must only be accessed within the sshClient mutex.
func (sshClient *sshClient) setOSLConfig() {
	sshClient.Lock()
	defer sshClient.Unlock()

	propagationChannelID, err := getStringRequestParam(
		sshClient.handshakeState.apiParams, "propagation_channel_id")
	if err != nil {
		// This should not fail as long as client has sent valid handshake
		return
	}

	// Use a cached seed state if one is found for the client's
	// session ID. This enables resuming progress made in a previous
	// tunnel.
	// Note: go-cache is already concurency safe; the additional mutex
	// is necessary to guarantee that Get/Delete is atomic; although in
	// practice no two concurrent clients should ever supply the same
	// session ID.

	sshClient.sshServer.oslSessionCacheMutex.Lock()
	oslClientSeedState, found := sshClient.sshServer.oslSessionCache.Get(sshClient.sessionID)
	if found {
		sshClient.sshServer.oslSessionCache.Delete(sshClient.sessionID)
		sshClient.sshServer.oslSessionCacheMutex.Unlock()
		sshClient.oslClientSeedState = oslClientSeedState.(*osl.ClientSeedState)
		sshClient.oslClientSeedState.Resume(sshClient.signalIssueSLOKs)
		return
	}
	sshClient.sshServer.oslSessionCacheMutex.Unlock()

	// Two limitations when setOSLConfig() is invoked due to an
	// OSL config hot reload:
	//
	// 1. any partial progress towards SLOKs is lost.
	//
	// 2. all existing osl.ClientSeedPortForwards for existing
	//    port forwards will not send progress to the new client
	//    seed state.

	sshClient.oslClientSeedState = sshClient.sshServer.support.OSLConfig.NewClientSeedState(
		sshClient.geoIPData.Country,
		propagationChannelID,
		sshClient.signalIssueSLOKs)
}

// newClientSeedPortForward will return nil when no seeding is
// associated with the specified ipAddress.
func (sshClient *sshClient) newClientSeedPortForward(ipAddress net.IP) *osl.ClientSeedPortForward {
	sshClient.Lock()
	defer sshClient.Unlock()

	// Will not be initialized before handshake.
	if sshClient.oslClientSeedState == nil {
		return nil
	}

	return sshClient.oslClientSeedState.NewClientSeedPortForward(ipAddress)
}

// getOSLSeedPayload returns a payload containing all seeded SLOKs for
// this client's session.
func (sshClient *sshClient) getOSLSeedPayload() *osl.SeedPayload {
	sshClient.Lock()
	defer sshClient.Unlock()

	// Will not be initialized before handshake.
	if sshClient.oslClientSeedState == nil {
		return &osl.SeedPayload{SLOKs: make([]*osl.SLOK, 0)}
	}

	return sshClient.oslClientSeedState.GetSeedPayload()
}

func (sshClient *sshClient) clearOSLSeedPayload() {
	sshClient.Lock()
	defer sshClient.Unlock()

	sshClient.oslClientSeedState.ClearSeedPayload()
}

func (sshClient *sshClient) rateLimits() common.RateLimits {
	sshClient.Lock()
	defer sshClient.Unlock()

	return sshClient.trafficRules.RateLimits.CommonRateLimits()
}

func (sshClient *sshClient) idleTCPPortForwardTimeout() time.Duration {
	sshClient.Lock()
	defer sshClient.Unlock()

	return time.Duration(*sshClient.trafficRules.IdleTCPPortForwardTimeoutMilliseconds) * time.Millisecond
}

func (sshClient *sshClient) idleUDPPortForwardTimeout() time.Duration {
	sshClient.Lock()
	defer sshClient.Unlock()

	return time.Duration(*sshClient.trafficRules.IdleUDPPortForwardTimeoutMilliseconds) * time.Millisecond
}

func (sshClient *sshClient) setTCPPortForwardDialingAvailableSignal(signal context.CancelFunc) {
	sshClient.Lock()
	defer sshClient.Unlock()

	sshClient.tcpPortForwardDialingAvailableSignal = signal
}

const (
	portForwardTypeTCP = iota
	portForwardTypeUDP
)

func (sshClient *sshClient) isPortForwardPermitted(
	portForwardType int,
	remoteIP net.IP,
	port int) bool {

	// Disallow connection to loopback. This is a failsafe. The server
	// should be run on a host with correctly configured firewall rules.
	if remoteIP.IsLoopback() {
		return false
	}

	// Blocklist check.
	//
	// Limitation: isPortForwardPermitted is not called in transparent DNS
	// forwarding cases. As the destination IP address is rewritten in these
	// cases, a blocklist entry won't be dialed in any case. However, no logs
	// will be recorded.

	tags := sshClient.sshServer.support.Blocklist.Lookup(remoteIP)
	if len(tags) > 0 {
		sshClient.logBlocklistHits(remoteIP, tags)
		if sshClient.sshServer.support.Config.BlocklistActive {
			return false
		}
	}

	// Don't lock before calling logBlocklistHits.
	sshClient.Lock()
	defer sshClient.Unlock()

	// Client must complete handshake before port forwards are permitted.
	if !sshClient.handshakeState.completed {
		return false
	}

	// Traffic rules checks.

	var allowPorts []int
	if portForwardType == portForwardTypeTCP {
		allowPorts = sshClient.trafficRules.AllowTCPPorts
	} else {
		allowPorts = sshClient.trafficRules.AllowUDPPorts
	}

	if len(allowPorts) == 0 {
		return true
	}

	// TODO: faster lookup?
	if len(allowPorts) > 0 {
		for _, allowPort := range allowPorts {
			if port == allowPort {
				return true
			}
		}
	}

	for _, subnet := range sshClient.trafficRules.AllowSubnets {
		// Note: ignoring error as config has been validated
		_, network, _ := net.ParseCIDR(subnet)
		if network.Contains(remoteIP) {
			return true
		}
	}

	log.WithContextFields(
		LogFields{
			"type": portForwardType,
			"port": port,
		}).Debug("port forward denied by traffic rules")

	return false
}

func (sshClient *sshClient) isTCPDialingPortForwardLimitExceeded() bool {

	sshClient.Lock()
	defer sshClient.Unlock()

	state := &sshClient.tcpTrafficState
	max := *sshClient.trafficRules.MaxTCPDialingPortForwardCount

	if max > 0 && state.concurrentDialingPortForwardCount >= int64(max) {
		return true
	}
	return false
}

func (sshClient *sshClient) getTCPPortForwardQueueSize() int {

	sshClient.Lock()
	defer sshClient.Unlock()

	return *sshClient.trafficRules.MaxTCPPortForwardCount +
		*sshClient.trafficRules.MaxTCPDialingPortForwardCount
}

func (sshClient *sshClient) getDialTCPPortForwardTimeoutMilliseconds() int {

	sshClient.Lock()
	defer sshClient.Unlock()

	return *sshClient.trafficRules.DialTCPPortForwardTimeoutMilliseconds
}

func (sshClient *sshClient) dialingTCPPortForward() {

	sshClient.Lock()
	defer sshClient.Unlock()

	state := &sshClient.tcpTrafficState

	state.concurrentDialingPortForwardCount += 1
	if state.concurrentDialingPortForwardCount > state.peakConcurrentDialingPortForwardCount {
		state.peakConcurrentDialingPortForwardCount = state.concurrentDialingPortForwardCount
	}
}

func (sshClient *sshClient) abortedTCPPortForward() {

	sshClient.Lock()
	defer sshClient.Unlock()

	sshClient.tcpTrafficState.concurrentDialingPortForwardCount -= 1
}

func (sshClient *sshClient) allocatePortForward(portForwardType int) bool {

	sshClient.Lock()
	defer sshClient.Unlock()

	// Check if at port forward limit. The subsequent counter
	// changes must be atomic with the limit check to ensure
	// the counter never exceeds the limit in the case of
	// concurrent allocations.

	var max int
	var state *trafficState
	if portForwardType == portForwardTypeTCP {
		max = *sshClient.trafficRules.MaxTCPPortForwardCount
		state = &sshClient.tcpTrafficState
	} else {
		max = *sshClient.trafficRules.MaxUDPPortForwardCount
		state = &sshClient.udpTrafficState
	}

	if max > 0 && state.concurrentPortForwardCount >= int64(max) {
		return false
	}

	// Update port forward counters.

	if portForwardType == portForwardTypeTCP {

		// Assumes TCP port forwards called dialingTCPPortForward
		state.concurrentDialingPortForwardCount -= 1

		if sshClient.tcpPortForwardDialingAvailableSignal != nil {

			max := *sshClient.trafficRules.MaxTCPDialingPortForwardCount
			if max <= 0 || state.concurrentDialingPortForwardCount < int64(max) {
				sshClient.tcpPortForwardDialingAvailableSignal()
			}
		}
	}

	state.concurrentPortForwardCount += 1
	if state.concurrentPortForwardCount > state.peakConcurrentPortForwardCount {
		state.peakConcurrentPortForwardCount = state.concurrentPortForwardCount
	}
	state.totalPortForwardCount += 1

	return true
}

// establishedPortForward increments the concurrent port
// forward counter. closedPortForward decrements it, so it
// must always be called for each establishedPortForward
// call.
//
// When at the limit of established port forwards, the LRU
// existing port forward is closed to make way for the newly
// established one. There can be a minor delay as, in addition
// to calling Close() on the port forward net.Conn,
// establishedPortForward waits for the LRU's closedPortForward()
// call which will decrement the concurrent counter. This
// ensures all resources associated with the LRU (socket,
// goroutine) are released or will very soon be released before
// proceeding.
func (sshClient *sshClient) establishedPortForward(
	portForwardType int, portForwardLRU *common.LRUConns) {

	// Do not lock sshClient here.

	var state *trafficState
	if portForwardType == portForwardTypeTCP {
		state = &sshClient.tcpTrafficState
	} else {
		state = &sshClient.udpTrafficState
	}

	// When the maximum number of port forwards is already
	// established, close the LRU. CloseOldest will call
	// Close on the port forward net.Conn. Both TCP and
	// UDP port forwards have handler goroutines that may
	// be blocked calling Read on the net.Conn. Close will
	// eventually interrupt the Read and cause the handlers
	// to exit, but not immediately. So the following logic
	// waits for a LRU handler to be interrupted and signal
	// availability.
	//
	// Notes:
	//
	// - the port forward limit can change via a traffic
	//   rules hot reload; the condition variable handles
	//   this case whereas a channel-based semaphore would
	//   not.
	//
	// - if a number of goroutines exceeding the total limit
	//   arrive here all concurrently, some CloseOldest() calls
	//   will have no effect as there can be less existing port
	//   forwards than new ones. In this case, the new port
	//   forward will be delayed. This is highly unlikely in
	//   practise since UDP calls to establishedPortForward are
	//   serialized and TCP calls are limited by the dial
	//   queue/count.

	if !sshClient.allocatePortForward(portForwardType) {

		portForwardLRU.CloseOldest()
		log.WithContext().Debug("closed LRU port forward")

		state.availablePortForwardCond.L.Lock()
		for !sshClient.allocatePortForward(portForwardType) {
			state.availablePortForwardCond.Wait()
		}
		state.availablePortForwardCond.L.Unlock()
	}
}

func (sshClient *sshClient) closedPortForward(
	portForwardType int, bytesUp, bytesDown int64) {

	sshClient.Lock()

	var state *trafficState
	if portForwardType == portForwardTypeTCP {
		state = &sshClient.tcpTrafficState
	} else {
		state = &sshClient.udpTrafficState
	}

	state.concurrentPortForwardCount -= 1
	state.bytesUp += bytesUp
	state.bytesDown += bytesDown

	sshClient.Unlock()

	// Signal any goroutine waiting in establishedPortForward
	// that an established port forward slot is available.
	state.availablePortForwardCond.Signal()
}

func (sshClient *sshClient) updateQualityMetricsWithDialResult(
	tcpPortForwardDialSuccess bool, dialDuration time.Duration) {

	sshClient.Lock()
	defer sshClient.Unlock()

	if tcpPortForwardDialSuccess {
		sshClient.qualityMetrics.tcpPortForwardDialedCount += 1
		sshClient.qualityMetrics.tcpPortForwardDialedDuration += dialDuration

	} else {
		sshClient.qualityMetrics.tcpPortForwardFailedCount += 1
		sshClient.qualityMetrics.tcpPortForwardFailedDuration += dialDuration
	}
}

func (sshClient *sshClient) updateQualityMetricsWithRejectedDialingLimit() {

	sshClient.Lock()
	defer sshClient.Unlock()

	sshClient.qualityMetrics.tcpPortForwardRejectedDialingLimitCount += 1
}

func (sshClient *sshClient) handleTCPChannel(
	remainingDialTimeout time.Duration,
	hostToConnect string,
	portToConnect int,
	newChannel ssh.NewChannel) {

	// Assumptions:
	// - sshClient.dialingTCPPortForward() has been called
	// - remainingDialTimeout > 0

	established := false
	defer func() {
		if !established {
			sshClient.abortedTCPPortForward()
		}
	}()

	// Transparently redirect web API request connections.

	isWebServerPortForward := false
	config := sshClient.sshServer.support.Config
	if config.WebServerPortForwardAddress != "" {
		destination := net.JoinHostPort(hostToConnect, strconv.Itoa(portToConnect))
		if destination == config.WebServerPortForwardAddress {
			isWebServerPortForward = true
			if config.WebServerPortForwardRedirectAddress != "" {
				// Note: redirect format is validated when config is loaded
				host, portStr, _ := net.SplitHostPort(config.WebServerPortForwardRedirectAddress)
				port, _ := strconv.Atoi(portStr)
				hostToConnect = host
				portToConnect = port
			}
		}
	}

	// Dial the remote address.
	//
	// Hostname resolution is performed explicitly, as a separate step, as the target IP
	// address is used for traffic rules (AllowSubnets) and OSL seed progress.
	//
	// Contexts are used for cancellation (via sshClient.runCtx, which is cancelled
	// when the client is stopping) and timeouts.

	dialStartTime := monotime.Now()

	log.WithContextFields(LogFields{"hostToConnect": hostToConnect}).Debug("resolving")

	ctx, cancelCtx := context.WithTimeout(sshClient.runCtx, remainingDialTimeout)
	IPs, err := (&net.Resolver{}).LookupIPAddr(ctx, hostToConnect)
	cancelCtx() // "must be called or the new context will remain live until its parent context is cancelled"

	// TODO: shuffle list to try other IPs?
	// TODO: IPv6 support
	var IP net.IP
	for _, ip := range IPs {
		if ip.IP.To4() != nil {
			IP = ip.IP
			break
		}
	}
	if err == nil && IP == nil {
		err = errors.New("no IP address")
	}

	resolveElapsedTime := monotime.Since(dialStartTime)

	if err != nil {

		// Record a port forward failure
		sshClient.updateQualityMetricsWithDialResult(false, resolveElapsedTime)

		sshClient.rejectNewChannel(newChannel, fmt.Sprintf("LookupIP failed: %s", err))
		return
	}

	remainingDialTimeout -= resolveElapsedTime

	if remainingDialTimeout <= 0 {
		sshClient.rejectNewChannel(newChannel, "TCP port forward timed out resolving")
		return
	}

	// Enforce traffic rules, using the resolved IP address.

	if !isWebServerPortForward &&
		!sshClient.isPortForwardPermitted(
			portForwardTypeTCP,
			IP,
			portToConnect) {

		// Note: not recording a port forward failure in this case

		sshClient.rejectNewChannel(newChannel, "port forward not permitted")
		return
	}

	// TCP dial.

	remoteAddr := net.JoinHostPort(IP.String(), strconv.Itoa(portToConnect))

	log.WithContextFields(LogFields{"remoteAddr": remoteAddr}).Debug("dialing")

	ctx, cancelCtx = context.WithTimeout(sshClient.runCtx, remainingDialTimeout)
	fwdConn, err := (&net.Dialer{}).DialContext(ctx, "tcp", remoteAddr)
	cancelCtx() // "must be called or the new context will remain live until its parent context is cancelled"

	// Record port forward success or failure
	sshClient.updateQualityMetricsWithDialResult(err == nil, monotime.Since(dialStartTime))

	if err != nil {

		// Monitor for low resource error conditions
		sshClient.sshServer.monitorPortForwardDialError(err)

		sshClient.rejectNewChannel(newChannel, fmt.Sprintf("DialTimeout failed: %s", err))
		return
	}

	// The upstream TCP port forward connection has been established. Schedule
	// some cleanup and notify the SSH client that the channel is accepted.

	defer fwdConn.Close()

	fwdChannel, requests, err := newChannel.Accept()
	if err != nil {
		if !isExpectedTunnelIOError(err) {
			log.WithContextFields(LogFields{"error": err}).Warning("accept new channel failed")
		}
		return
	}
	go ssh.DiscardRequests(requests)
	defer fwdChannel.Close()

	// Release the dialing slot and acquire an established slot.
	//
	// establishedPortForward increments the concurrent TCP port
	// forward counter and closes the LRU existing TCP port forward
	// when already at the limit.
	//
	// Known limitations:
	//
	// - Closed LRU TCP sockets will enter the TIME_WAIT state,
	//   continuing to consume some resources.

	sshClient.establishedPortForward(portForwardTypeTCP, sshClient.tcpPortForwardLRU)

	// "established = true" cancels the deferred abortedTCPPortForward()
	established = true

	// TODO: 64-bit alignment? https://golang.org/pkg/sync/atomic/#pkg-note-BUG
	var bytesUp, bytesDown int64
	defer func() {
		sshClient.closedPortForward(
			portForwardTypeTCP, atomic.LoadInt64(&bytesUp), atomic.LoadInt64(&bytesDown))
	}()

	lruEntry := sshClient.tcpPortForwardLRU.Add(fwdConn)
	defer lruEntry.Remove()

	// ActivityMonitoredConn monitors the TCP port forward I/O and updates
	// its LRU status. ActivityMonitoredConn also times out I/O on the port
	// forward if both reads and writes have been idle for the specified
	// duration.

	// Ensure nil interface if newClientSeedPortForward returns nil
	var updater common.ActivityUpdater
	seedUpdater := sshClient.newClientSeedPortForward(IP)
	if seedUpdater != nil {
		updater = seedUpdater
	}

	fwdConn, err = common.NewActivityMonitoredConn(
		fwdConn,
		sshClient.idleTCPPortForwardTimeout(),
		true,
		updater,
		lruEntry)
	if err != nil {
		log.WithContextFields(LogFields{"error": err}).Error("NewActivityMonitoredConn failed")
		return
	}

	// Relay channel to forwarded connection.

	log.WithContextFields(LogFields{"remoteAddr": remoteAddr}).Debug("relaying")

	// TODO: relay errors to fwdChannel.Stderr()?
	relayWaitGroup := new(sync.WaitGroup)
	relayWaitGroup.Add(1)
	go func() {
		defer relayWaitGroup.Done()
		// io.Copy allocates a 32K temporary buffer, and each port forward relay uses
		// two of these buffers; using io.CopyBuffer with a smaller buffer reduces the
		// overall memory footprint.
		bytes, err := io.CopyBuffer(
			fwdChannel, fwdConn, make([]byte, SSH_TCP_PORT_FORWARD_COPY_BUFFER_SIZE))
		atomic.AddInt64(&bytesDown, bytes)
		if err != nil && err != io.EOF {
			// Debug since errors such as "connection reset by peer" occur during normal operation
			log.WithContextFields(LogFields{"error": err}).Debug("downstream TCP relay failed")
		}
		// Interrupt upstream io.Copy when downstream is shutting down.
		// TODO: this is done to quickly cleanup the port forward when
		// fwdConn has a read timeout, but is it clean -- upstream may still
		// be flowing?
		fwdChannel.Close()
	}()
	bytes, err := io.CopyBuffer(
		fwdConn, fwdChannel, make([]byte, SSH_TCP_PORT_FORWARD_COPY_BUFFER_SIZE))
	atomic.AddInt64(&bytesUp, bytes)
	if err != nil && err != io.EOF {
		log.WithContextFields(LogFields{"error": err}).Debug("upstream TCP relay failed")
	}
	// Shutdown special case: fwdChannel will be closed and return EOF when
	// the SSH connection is closed, but we need to explicitly close fwdConn
	// to interrupt the downstream io.Copy, which may be blocked on a
	// fwdConn.Read().
	fwdConn.Close()

	relayWaitGroup.Wait()

	log.WithContextFields(
		LogFields{
			"remoteAddr": remoteAddr,
			"bytesUp":    atomic.LoadInt64(&bytesUp),
			"bytesDown":  atomic.LoadInt64(&bytesDown)}).Debug("exiting")
}

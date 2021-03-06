/*
 * Copyright (c) 2015, Psiphon Inc.
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

package psiphon

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/transferstats"
	"golang.org/x/crypto/ssh"
)

// Tunneler specifies the interface required by components that use a tunnel.
// Components which use this interface may be serviced by a single Tunnel instance,
// or a Controller which manages a pool of tunnels, or any other object which
// implements Tunneler.
// downstreamConn is an optional parameter which specifies a connection to be
// explictly closed when the Dialed connection is closed. For instance, this
// is used to close downstreamConn App<->LocalProxy connections when the related
// LocalProxy<->SshPortForward connections close.
type Tunneler interface {
	Dial(remoteAddr string, downstreamConn net.Conn) (conn net.Conn, err error)
	SignalComponentFailure()
}

// TunnerOwner specifies the interface required by Tunnel to notify its
// owner when it has failed. The owner may, as in the case of the Controller,
// remove the tunnel from its list of active tunnels.
type TunnelOwner interface {
	SignalTunnelFailure(tunnel *Tunnel)
}

const (
	TUNNEL_PROTOCOL_SSH            = "SSH"
	TUNNEL_PROTOCOL_OBFUSCATED_SSH = "OSSH"
	TUNNEL_PROTOCOL_UNFRONTED_MEEK = "UNFRONTED-MEEK-OSSH"
	TUNNEL_PROTOCOL_FRONTED_MEEK   = "FRONTED-MEEK-OSSH"
)

// This is a list of supported tunnel protocols, in default preference order
var SupportedTunnelProtocols = []string{
	TUNNEL_PROTOCOL_FRONTED_MEEK,
	TUNNEL_PROTOCOL_UNFRONTED_MEEK,
	TUNNEL_PROTOCOL_OBFUSCATED_SSH,
	TUNNEL_PROTOCOL_SSH,
}

// Tunnel is a connection to a Psiphon server. An established
// tunnel includes a network connection to the specified server
// and an SSH session built on top of that transport.
type Tunnel struct {
	mutex                    *sync.Mutex
	isClosed                 bool
	serverEntry              *ServerEntry
	session                  *Session
	protocol                 string
	conn                     Conn
	closedSignal             chan struct{}
	sshClient                *ssh.Client
	operateWaitGroup         *sync.WaitGroup
	shutdownOperateBroadcast chan struct{}
	portForwardFailures      chan int
	portForwardFailureTotal  int
}

// EstablishTunnel first makes a network transport connection to the
// Psiphon server and then establishes an SSH client session on top of
// that transport. The SSH server is authenticated using the public
// key in the server entry.
// Depending on the server's capabilities, the connection may use
// plain SSH over TCP, obfuscated SSH over TCP, or obfuscated SSH over
// HTTP (meek protocol).
// When requiredProtocol is not blank, that protocol is used. Otherwise,
// the first protocol in SupportedTunnelProtocols that's also in the
// server capabilities is used.
func EstablishTunnel(
	config *Config,
	sessionId string,
	pendingConns *Conns,
	serverEntry *ServerEntry,
	tunnelOwner TunnelOwner) (tunnel *Tunnel, err error) {

	selectedProtocol, err := selectProtocol(config, serverEntry)
	if err != nil {
		return nil, ContextError(err)
	}

	// Build transport layers and establish SSH connection
	conn, closedSignal, sshClient, err := dialSsh(
		config, pendingConns, serverEntry, selectedProtocol, sessionId)
	if err != nil {
		return nil, ContextError(err)
	}

	// Cleanup on error
	defer func() {
		if err != nil {
			conn.Close()
		}
	}()

	// The tunnel is now connected
	tunnel = &Tunnel{
		mutex:                    new(sync.Mutex),
		isClosed:                 false,
		serverEntry:              serverEntry,
		protocol:                 selectedProtocol,
		conn:                     conn,
		closedSignal:             closedSignal,
		sshClient:                sshClient,
		operateWaitGroup:         new(sync.WaitGroup),
		shutdownOperateBroadcast: make(chan struct{}),
		// portForwardFailures buffer size is large enough to receive the thresold number
		// of failure reports without blocking. Senders can drop failures without blocking.
		portForwardFailures: make(chan int, config.PortForwardFailureThreshold)}

	// Create a new Psiphon API session for this tunnel. This includes performing
	// a handshake request. If the handshake fails, this establishment fails.
	//
	// TODO: as long as the servers are not enforcing that a client perform a handshake,
	// proceed with this tunnel as long as at least one previous handhake succeeded?
	//
	if !config.DisableApi {
		NoticeInfo("starting session for %s", tunnel.serverEntry.IpAddress)
		tunnel.session, err = NewSession(config, tunnel, sessionId)
		if err != nil {
			return nil, ContextError(fmt.Errorf("error starting session for %s: %s", tunnel.serverEntry.IpAddress, err))
		}
	}

	// Now that network operations are complete, cancel interruptibility
	pendingConns.Remove(conn)

	// Promote this successful tunnel to first rank so it's one
	// of the first candidates next time establish runs.
	PromoteServerEntry(tunnel.serverEntry.IpAddress)

	// Spawn the operateTunnel goroutine, which monitors the tunnel and handles periodic stats updates.
	tunnel.operateWaitGroup.Add(1)
	go tunnel.operateTunnel(config, tunnelOwner)

	return tunnel, nil
}

// Close stops operating the tunnel and closes the underlying connection.
// Supports multiple and/or concurrent calls to Close().
func (tunnel *Tunnel) Close() {
	tunnel.mutex.Lock()
	if !tunnel.isClosed {
		close(tunnel.shutdownOperateBroadcast)
		tunnel.operateWaitGroup.Wait()
		tunnel.conn.Close()
	}
	tunnel.isClosed = true
	tunnel.mutex.Unlock()
}

// Dial establishes a port forward connection through the tunnel
func (tunnel *Tunnel) Dial(remoteAddr string, downstreamConn net.Conn) (conn net.Conn, err error) {
	tunnel.mutex.Lock()
	isClosed := tunnel.isClosed
	tunnel.mutex.Unlock()
	if isClosed {
		return nil, errors.New("tunnel is closed")
	}

	sshPortForwardConn, err := tunnel.sshClient.Dial("tcp", remoteAddr)
	if err != nil {
		// TODO: conditional on type of error or error message?
		select {
		case tunnel.portForwardFailures <- 1:
		default:
		}
		return nil, ContextError(err)
	}

	conn = &TunneledConn{
		Conn:           sshPortForwardConn,
		tunnel:         tunnel,
		downstreamConn: downstreamConn}

	// Tunnel does not have a session when DisableApi is set
	if tunnel.session != nil {
		conn = transferstats.NewConn(
			conn, tunnel.session.StatsServerID(), tunnel.session.StatsRegexps())
	}

	return conn, nil
}

// SignalComponentFailure notifies the tunnel that an associated component has failed.
// This will terminate the tunnel.
func (tunnel *Tunnel) SignalComponentFailure() {
	NoticeAlert("tunnel received component failure signal")
	tunnel.Close()
}

// TunneledConn implements net.Conn and wraps a port foward connection.
// It is used to hook into Read and Write to observe I/O errors and
// report these errors back to the tunnel monitor as port forward failures.
// TunneledConn optionally tracks a peer connection to be explictly closed
// when the TunneledConn is closed.
type TunneledConn struct {
	net.Conn
	tunnel         *Tunnel
	downstreamConn net.Conn
}

func (conn *TunneledConn) Read(buffer []byte) (n int, err error) {
	n, err = conn.Conn.Read(buffer)
	if err != nil && err != io.EOF {
		// Report 1 new failure. Won't block; assumes the receiver
		// has a sufficient buffer for the threshold number of reports.
		// TODO: conditional on type of error or error message?
		select {
		case conn.tunnel.portForwardFailures <- 1:
		default:
		}
	}
	return
}

func (conn *TunneledConn) Write(buffer []byte) (n int, err error) {
	n, err = conn.Conn.Write(buffer)
	if err != nil && err != io.EOF {
		// Same as TunneledConn.Read()
		select {
		case conn.tunnel.portForwardFailures <- 1:
		default:
		}
	}
	return
}

func (conn *TunneledConn) Close() error {
	if conn.downstreamConn != nil {
		err := conn.downstreamConn.Close()
		if err != nil {
			NoticeAlert("downstreamConn.Close() error: %s", ContextError(err))
		}
	}
	return conn.Conn.Close()
}

// selectProtocol is a helper that picks the tunnel protocol
func selectProtocol(config *Config, serverEntry *ServerEntry) (selectedProtocol string, err error) {
	// TODO: properly handle protocols (e.g. FRONTED-MEEK-OSSH) vs. capabilities (e.g., {FRONTED-MEEK, OSSH})
	// for now, the code is simply assuming that MEEK capabilities imply OSSH capability.
	if config.TunnelProtocol != "" {
		requiredCapability := strings.TrimSuffix(config.TunnelProtocol, "-OSSH")
		if !Contains(serverEntry.Capabilities, requiredCapability) {
			return "", ContextError(fmt.Errorf("server does not have required capability"))
		}
		selectedProtocol = config.TunnelProtocol
	} else {
		// Order of SupportedTunnelProtocols is default preference order
		for _, protocol := range SupportedTunnelProtocols {
			requiredCapability := strings.TrimSuffix(protocol, "-OSSH")
			if Contains(serverEntry.Capabilities, requiredCapability) {
				selectedProtocol = protocol
				break
			}
		}
		if selectedProtocol == "" {
			return "", ContextError(fmt.Errorf("server does not have any supported capabilities"))
		}
	}
	return selectedProtocol, nil
}

// dialSsh is a helper that builds the transport layers and establishes the SSH connection
func dialSsh(
	config *Config,
	pendingConns *Conns,
	serverEntry *ServerEntry,
	selectedProtocol,
	sessionId string) (conn Conn, closedSignal chan struct{}, sshClient *ssh.Client, err error) {

	// The meek protocols tunnel obfuscated SSH. Obfuscated SSH is layered on top of SSH.
	// So depending on which protocol is used, multiple layers are initialized.
	port := 0
	useMeek := false
	useFronting := false
	useObfuscatedSsh := false
	switch selectedProtocol {
	case TUNNEL_PROTOCOL_FRONTED_MEEK:
		useMeek = true
		useFronting = true
		useObfuscatedSsh = true
	case TUNNEL_PROTOCOL_UNFRONTED_MEEK:
		useMeek = true
		useObfuscatedSsh = true
		port = serverEntry.SshObfuscatedPort
	case TUNNEL_PROTOCOL_OBFUSCATED_SSH:
		useObfuscatedSsh = true
		port = serverEntry.SshObfuscatedPort
	case TUNNEL_PROTOCOL_SSH:
		port = serverEntry.SshPort
	}

	frontingDomain := ""
	if useFronting {
		frontingDomain = serverEntry.MeekFrontingDomain
	}
	NoticeConnectingServer(
		serverEntry.IpAddress,
		serverEntry.Region,
		selectedProtocol,
		frontingDomain)

	// Create the base transport: meek or direct connection
	dialConfig := &DialConfig{
		UpstreamHttpProxyAddress: config.UpstreamHttpProxyAddress,
		ConnectTimeout:           TUNNEL_CONNECT_TIMEOUT,
		ReadTimeout:              TUNNEL_READ_TIMEOUT,
		WriteTimeout:             TUNNEL_WRITE_TIMEOUT,
		PendingConns:             pendingConns,
		BindToDeviceProvider:     config.BindToDeviceProvider,
		BindToDeviceDnsServer:    config.BindToDeviceDnsServer,
	}
	if useMeek {
		conn, err = DialMeek(serverEntry, sessionId, useFronting, dialConfig)
		if err != nil {
			return nil, nil, nil, ContextError(err)
		}
	} else {
		conn, err = DialTCP(fmt.Sprintf("%s:%d", serverEntry.IpAddress, port), dialConfig)
		if err != nil {
			return nil, nil, nil, ContextError(err)
		}
	}

	cleanupConn := conn
	defer func() {
		// Cleanup on error
		if err != nil {
			cleanupConn.Close()
		}
	}()

	// Create signal which is triggered when the underlying network connection is closed,
	// this is used in operateTunnel to detect an unexpected disconnect. SetClosedSignal
	// is called here, well before operateTunnel, so that we don't need to handle the
	// "already closed" with a tunnelOwner.SignalTunnelFailure() in operateTunnel (this
	// was previously the order of events, which caused the establish process to sometimes
	// run briefly when not needed).
	closedSignal = make(chan struct{}, 1)
	if !conn.SetClosedSignal(closedSignal) {
		// Conn is already closed. This is not unexpected -- for example,
		// when establish is interrupted.
		// TODO: make this not log an error when called from establishTunnelWorker?
		return nil, nil, nil, ContextError(errors.New("conn already closed"))
	}

	// Add obfuscated SSH layer
	var sshConn net.Conn
	sshConn = conn
	if useObfuscatedSsh {
		sshConn, err = NewObfuscatedSshConn(conn, serverEntry.SshObfuscatedKey)
		if err != nil {
			return nil, nil, nil, ContextError(err)
		}
	}

	// Now establish the SSH session over the sshConn transport
	expectedPublicKey, err := base64.StdEncoding.DecodeString(serverEntry.SshHostKey)
	if err != nil {
		return nil, nil, nil, ContextError(err)
	}
	sshCertChecker := &ssh.CertChecker{
		HostKeyFallback: func(addr string, remote net.Addr, publicKey ssh.PublicKey) error {
			if !bytes.Equal(expectedPublicKey, publicKey.Marshal()) {
				return ContextError(errors.New("unexpected host public key"))
			}
			return nil
		},
	}
	sshPasswordPayload, err := json.Marshal(
		struct {
			SessionId   string `json:"SessionId"`
			SshPassword string `json:"SshPassword"`
		}{sessionId, serverEntry.SshPassword})
	if err != nil {
		return nil, nil, nil, ContextError(err)
	}
	sshClientConfig := &ssh.ClientConfig{
		User: serverEntry.SshUsername,
		Auth: []ssh.AuthMethod{
			ssh.Password(string(sshPasswordPayload)),
		},
		HostKeyCallback: sshCertChecker.CheckHostKey,
	}
	// The folowing is adapted from ssh.Dial(), here using a custom conn
	// The sshAddress is passed through to host key verification callbacks; we don't use it.
	sshAddress := ""
	sshClientConn, sshChans, sshReqs, err := ssh.NewClientConn(sshConn, sshAddress, sshClientConfig)
	if err != nil {
		return nil, nil, nil, ContextError(err)
	}
	sshClient = ssh.NewClient(sshClientConn, sshChans, sshReqs)

	return conn, closedSignal, sshClient, nil
}

// operateTunnel periodically sends status requests (traffic stats updates updates)
// to the Psiphon API; and monitors the tunnel for failures:
//
// 1. Overall tunnel failure: the tunnel sends a signal to the ClosedSignal
// channel on keep-alive failure and other transport I/O errors. In case
// of such a failure, the tunnel is marked as failed.
//
// 2. Tunnel port forward failures: the tunnel connection may stay up but
// the client may still fail to establish port forwards due to server load
// and other conditions. After a threshold number of such failures, the
// overall tunnel is marked as failed.
//
// TODO: currently, any connect (dial), read, or write error associated with
// a port forward is counted as a failure. It may be important to differentiate
// between failures due to Psiphon server conditions and failures due to the
// origin/target server (in the latter case, the tunnel is healthy). Here are
// some typical error messages to consider matching against (or ignoring):
//
// - "ssh: rejected: administratively prohibited (open failed)"
//   (this error message is reported in both actual and false cases: when a server
//    is overloaded and has no free ephemeral ports; and when the user mistypes
//    a domain in a browser address bar and name resolution fails)
// - "ssh: rejected: connect failed (Connection timed out)"
// - "write tcp ... broken pipe"
// - "read tcp ... connection reset by peer"
// - "ssh: unexpected packet in response to channel open: <nil>"
//
func (tunnel *Tunnel) operateTunnel(config *Config, tunnelOwner TunnelOwner) {
	defer tunnel.operateWaitGroup.Done()

	// The next status request and ssh keep alive times are picked at random,
	// from a range, to make the resulting traffic less fingerprintable,
	// especially when then tunnel is otherwise idle.
	// Note: not using Tickers since these are not fixed time periods.

	nextStatusRequestPeriod := func() time.Duration {
		return MakeRandomPeriod(
			PSIPHON_API_STATUS_REQUEST_PERIOD_MIN,
			PSIPHON_API_STATUS_REQUEST_PERIOD_MAX)
	}
	nextSshKeepAlivePeriod := func() time.Duration {
		return MakeRandomPeriod(
			TUNNEL_SSH_KEEP_ALIVE_PERIOD_MIN,
			TUNNEL_SSH_KEEP_ALIVE_PERIOD_MAX)
	}

	statsTimer := time.NewTimer(nextStatusRequestPeriod())
	defer statsTimer.Stop()

	sshKeepAliveTimer := time.NewTimer(nextSshKeepAlivePeriod())
	defer sshKeepAliveTimer.Stop()

	var err error
	for err == nil {
		select {
		case <-statsTimer.C:
			sendStats(tunnel)
			statsTimer.Reset(nextStatusRequestPeriod())

		case <-sshKeepAliveTimer.C:
			// Random padding to frustrate fingerprinting
			_, _, err := tunnel.sshClient.SendRequest(
				"keepalive@openssh.com", true,
				MakeSecureRandomPadding(0, TUNNEL_SSH_KEEP_ALIVE_PAYLOAD_MAX_BYTES))
			err = fmt.Errorf("ssh keep alive failed: %s", err)
			sshKeepAliveTimer.Reset(nextSshKeepAlivePeriod())

		case failures := <-tunnel.portForwardFailures:
			// Note: no mutex on portForwardFailureTotal; only referenced here
			tunnel.portForwardFailureTotal += failures
			NoticeInfo("port forward failures for %s: %d",
				tunnel.serverEntry.IpAddress, tunnel.portForwardFailureTotal)
			if tunnel.portForwardFailureTotal > config.PortForwardFailureThreshold {
				err = errors.New("tunnel exceeded port forward failure threshold")
			}

		case <-tunnel.closedSignal:
			err = errors.New("tunnel closed unexpectedly")

		case <-tunnel.shutdownOperateBroadcast:
			// Attempt to send any remaining stats
			sendStats(tunnel)
			NoticeInfo("shutdown operate tunnel")
			return
		}
	}

	if err != nil {
		NoticeAlert("operate tunnel error for %s: %s", tunnel.serverEntry.IpAddress, err)
		tunnelOwner.SignalTunnelFailure(tunnel)
	}
}

// sendStats is a helper for sending session stats to the server.
func sendStats(tunnel *Tunnel) {

	// Tunnel does not have a session when DisableApi is set
	if tunnel.session == nil {
		return
	}

	payload := transferstats.GetForServer(tunnel.serverEntry.IpAddress)
	if payload != nil {
		err := tunnel.session.DoStatusRequest(payload)
		if err != nil {
			NoticeAlert("DoStatusRequest failed for %s: %s", tunnel.serverEntry.IpAddress, err)
			transferstats.PutBack(tunnel.serverEntry.IpAddress, payload)
		}
	}
}

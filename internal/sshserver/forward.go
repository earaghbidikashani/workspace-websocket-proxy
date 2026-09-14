/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

package sshserver

import (
	"io"
	"net"
	"strconv"
	"sync"

	"github.com/gliderlabs/ssh"
	"github.com/go-logr/logr"
	gossh "golang.org/x/crypto/ssh"
)

const forwardedTCPChannelType = "forwarded-tcpip"

// remoteForwardSuccess carries the bound port back to the client, which matters
// when the client asked for port 0.
type remoteForwardSuccess struct {
	BindPort uint32
}

// remoteForwardChannelData is the forwarded-tcpip channel payload from RFC 4254
// section 7.2.
type remoteForwardChannelData struct {
	DestAddr   string
	DestPort   uint32
	OriginAddr string
	OriginPort uint32
}

// loopbackForwardHandler services tcpip-forward and cancel-tcpip-forward, the
// requests behind ssh -R.
//
// It replaces the library's own handler because two different addresses are
// involved and the library derives both from one field. The listener must bind
// loopback, so that a forward cannot be reached from outside the pod, while the
// forwarded-tcpip channels must report the address the client asked for: RFC 4254
// section 7.2 specifies the address from the request, and clients match incoming
// channels against exactly what they registered. Reporting the rewritten address
// instead makes a client reject every forwarded connection, which presents as the
// forward accepting connections and then resetting them.
type loopbackForwardHandler struct {
	logger logr.Logger

	mu sync.Mutex
	// forwards is keyed by the address the client will send to cancel: the host
	// it requested, with the port actually allocated.
	forwards map[string]net.Listener
}

func newLoopbackForwardHandler(logger logr.Logger) *loopbackForwardHandler {
	return &loopbackForwardHandler{
		logger:   logger,
		forwards: map[string]net.Listener{},
	}
}

// HandleSSHRequest dispatches the two global requests that manage reverse
// forwards.
func (h *loopbackForwardHandler) HandleSSHRequest(
	ctx ssh.Context,
	srv *ssh.Server,
	req *gossh.Request,
) (bool, []byte) {
	var payload remoteForwardRequest
	if err := gossh.Unmarshal(req.Payload, &payload); err != nil {
		h.logger.Info("Rejected malformed reverse forward request", "type", req.Type)
		return false, nil
	}

	switch req.Type {
	case requestTypeForward:
		return h.startForward(ctx, srv, payload)
	case requestTypeCancelForward:
		return h.cancelForward(payload)
	default:
		return false, nil
	}
}

// startForward opens the listener for a reverse forward and relays inbound
// connections to the client, returning the port it bound, which is what the
// client needs when it requested port 0.
func (h *loopbackForwardHandler) startForward(
	ctx ssh.Context,
	srv *ssh.Server,
	payload remoteForwardRequest,
) (bool, []byte) {
	listener, boundPort, ok := h.openForward(ctx, srv, payload)
	if !ok {
		return false, []byte("port forwarding is disabled")
	}

	key := forwardKey(payload.BindAddr, boundPort)

	conn, ok := ctx.Value(ssh.ContextKeyConn).(*gossh.ServerConn)
	if !ok {
		h.closeForward(key)
		return false, nil
	}

	go func() {
		<-ctx.Done()
		h.closeForward(key)
	}()

	go h.acceptLoop(conn, listener, payload.BindAddr, boundPort, key)

	return true, gossh.Marshal(&remoteForwardSuccess{BindPort: uint32(boundPort)})
}

// openForward applies the loopback policy, binds the listener and records it
// under the address the client requested. It is separate from the request
// plumbing so the policy can be exercised without a live SSH connection.
func (h *loopbackForwardHandler) openForward(
	ctx ssh.Context,
	srv *ssh.Server,
	payload remoteForwardRequest,
) (listener net.Listener, boundPort int, ok bool) {
	bindHost := payload.BindAddr
	if anyBindAddresses[bindHost] {
		h.logger.V(1).Info("Binding a wildcard reverse forward to loopback",
			"requested", payload.BindAddr, "bindPort", payload.BindPort)
		bindHost = loopbackIPv4
	}

	// The callback sees the address that will actually be bound, so a request for
	// a specific routable address is refused rather than quietly moved.
	if srv.ReversePortForwardingCallback == nil ||
		!srv.ReversePortForwardingCallback(ctx, bindHost, payload.BindPort) {
		return nil, 0, false
	}

	listener, err := net.Listen("tcp", net.JoinHostPort(bindHost, strconv.Itoa(int(payload.BindPort))))
	if err != nil {
		h.logger.Info("Failed to open a reverse forward listener",
			"bindHost", bindHost, "bindPort", payload.BindPort, "error", err.Error())
		return nil, 0, false
	}

	boundPort = listener.Addr().(*net.TCPAddr).Port

	h.mu.Lock()
	h.forwards[forwardKey(payload.BindAddr, boundPort)] = listener
	h.mu.Unlock()

	h.logger.V(1).Info("Opened a reverse forward",
		"requested", payload.BindAddr, "boundTo", listener.Addr().String())

	return listener, boundPort, true
}

// cancelForward closes a forward the client no longer wants. The client cancels
// with the host it originally requested, which is why the table is keyed that
// way rather than by the address that was bound.
func (h *loopbackForwardHandler) cancelForward(payload remoteForwardRequest) (bool, []byte) {
	key := forwardKey(payload.BindAddr, int(payload.BindPort))
	if !h.closeForward(key) {
		h.logger.V(1).Info("Ignoring a cancel for an unknown reverse forward", "forward", key)
		return false, nil
	}

	h.logger.V(1).Info("Closed a reverse forward", "forward", key)
	return true, nil
}

// acceptLoop relays each inbound connection to the client as its own channel.
func (h *loopbackForwardHandler) acceptLoop(
	conn *gossh.ServerConn,
	listener net.Listener,
	requestedAddr string,
	boundPort int,
	key string,
) {
	defer func() {
		h.mu.Lock()
		delete(h.forwards, key)
		h.mu.Unlock()
	}()

	for {
		inbound, err := listener.Accept()
		if err != nil {
			return
		}

		originAddr, originPort := splitAddr(inbound.RemoteAddr().String())
		channelData := gossh.Marshal(&remoteForwardChannelData{
			// The address the client asked for, not the one bound. Clients match
			// on this and reject anything else.
			DestAddr:   requestedAddr,
			DestPort:   uint32(boundPort),
			OriginAddr: originAddr,
			OriginPort: uint32(originPort),
		})

		go h.relay(conn, inbound, channelData)
	}
}

// relay opens the forwarded-tcpip channel for one inbound connection and copies
// in both directions until either side finishes.
func (h *loopbackForwardHandler) relay(
	conn *gossh.ServerConn,
	inbound net.Conn,
	channelData []byte,
) {
	channel, reqs, err := conn.OpenChannel(forwardedTCPChannelType, channelData)
	if err != nil {
		h.logger.V(1).Info("Client refused a forwarded connection", "error", err.Error())
		_ = inbound.Close()
		return
	}

	go gossh.DiscardRequests(reqs)

	go func() {
		defer func() { _ = channel.Close() }()
		defer func() { _ = inbound.Close() }()
		_, _ = io.Copy(channel, inbound)
	}()
	go func() {
		defer func() { _ = channel.Close() }()
		defer func() { _ = inbound.Close() }()
		_, _ = io.Copy(inbound, channel)
	}()
}

// closeForward closes a forward's listener, reporting whether it was there to
// close. Closing the listener is what ends its accept loop.
func (h *loopbackForwardHandler) closeForward(key string) bool {
	h.mu.Lock()
	listener, ok := h.forwards[key]
	if ok {
		delete(h.forwards, key)
	}
	h.mu.Unlock()

	if !ok {
		return false
	}
	_ = listener.Close()
	return true
}

func forwardKey(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func splitAddr(addr string) (host string, port int) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, 0
	}
	port, _ = strconv.Atoi(portStr)
	return host, port
}

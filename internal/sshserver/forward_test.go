/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

package sshserver

import (
	"net"
	"strconv"
	"testing"

	"github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// testForwardHandler returns a handler and the gliderlabs server whose callback
// it consults, wired to the same loopback policy the real server uses.
func testForwardHandler(t *testing.T) (*loopbackForwardHandler, *ssh.Server) {
	t.Helper()

	server, err := New(testConfig(t), testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := newLoopbackForwardHandler(testLogger())
	t.Cleanup(func() {
		handler.mu.Lock()
		defer handler.mu.Unlock()
		for _, listener := range handler.forwards {
			_ = listener.Close()
		}
	})

	return handler, server.ssh
}

// listenerFor looks a forward up the way a cancel request would.
func (h *loopbackForwardHandler) listenerFor(requestedHost string, port int) net.Listener {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.forwards[forwardKey(requestedHost, port)]
}

func request(bindAddr string, bindPort uint32) remoteForwardRequest {
	return remoteForwardRequest{BindAddr: bindAddr, BindPort: bindPort}
}

// A wildcard request must produce a listener on loopback only. Left alone it
// would reach net.Listen as ":port" and be reachable from outside the pod.
func TestForwardHandlerBindsWildcardRequestsToLoopback(t *testing.T) {
	for _, requested := range []string{"", bindAnyWildcard, bindAnyIPv4, bindAnyIPv6} {
		t.Run("requested "+strconv.Quote(requested), func(t *testing.T) {
			handler, srv := testForwardHandler(t)

			listener, port, ok := handler.openForward(nil, srv, request(requested, 0))
			if !ok {
				t.Fatal("expected a wildcard request to be accepted and moved to loopback")
			}
			if port == 0 {
				t.Fatal("expected a concrete allocated port")
			}

			host, _, err := net.SplitHostPort(listener.Addr().String())
			if err != nil {
				t.Fatalf("unexpected listener address %q: %v", listener.Addr(), err)
			}
			if !isLoopback(host) {
				t.Errorf("listener bound to %q, which is not loopback", host)
			}

			// Keyed on what the client asked for, since that is what it cancels with.
			if handler.listenerFor(requested, port) == nil {
				t.Errorf("expected the forward to be keyed on the requested address %q", requested)
			}
		})
	}
}

func TestForwardHandlerAcceptsExplicitLoopback(t *testing.T) {
	handler, srv := testForwardHandler(t)

	_, port, ok := handler.openForward(nil, srv, request(loopbackIPv4, 0))
	if !ok {
		t.Fatal("expected an explicit loopback request to be accepted")
	}
	if handler.listenerFor(loopbackIPv4, port) == nil {
		t.Error("expected the forward to be recorded")
	}
}

// A specific routable address is refused rather than quietly moved to loopback,
// because silently binding somewhere the client did not ask for would be worse
// than a clear refusal.
func TestForwardHandlerRejectsRoutableBind(t *testing.T) {
	handler, srv := testForwardHandler(t)

	if _, _, ok := handler.openForward(nil, srv, request("10.0.0.5", 0)); ok {
		t.Error("expected a request for a routable address to be refused")
	}
}

func TestForwardHandlerRefusesWhenCallbackIsUnset(t *testing.T) {
	handler, _ := testForwardHandler(t)

	if _, _, ok := handler.openForward(nil, &ssh.Server{}, request(loopbackIPv4, 0)); ok {
		t.Error("expected a server with no callback to refuse forwarding")
	}
}

func TestForwardHandlerRejectsMalformedPayload(t *testing.T) {
	handler, srv := testForwardHandler(t)

	req := &gossh.Request{Type: requestTypeForward, Payload: []byte{0xff}}
	if ok, _ := handler.HandleSSHRequest(nil, srv, req); ok {
		t.Error("expected a malformed request to be refused")
	}
}

func TestForwardHandlerIgnoresUnknownRequestType(t *testing.T) {
	handler, srv := testForwardHandler(t)

	req := &gossh.Request{
		Type:    "streamlocal-forward@openssh.com",
		Payload: gossh.Marshal(&remoteForwardRequest{BindAddr: loopbackIPv4}),
	}
	if ok, _ := handler.HandleSSHRequest(nil, srv, req); ok {
		t.Error("expected an unhandled request type to be refused")
	}
}

// The client cancels with the address it originally requested, so the table has
// to be keyed that way rather than by the address that was bound.
func TestForwardHandlerCancelsUsingTheRequestedAddress(t *testing.T) {
	handler, srv := testForwardHandler(t)

	_, port, ok := handler.openForward(nil, srv, request(bindAnyIPv4, 0))
	if !ok {
		t.Fatal("expected the forward to be accepted")
	}

	// A cancel naming the bound address rather than the requested one must not
	// match, otherwise the key is wrong in the other direction.
	if ok, _ := handler.cancelForward(request(loopbackIPv4, uint32(port))); ok {
		t.Error("expected a cancel for the bound address not to match the requested key")
	}

	if ok, _ := handler.cancelForward(request(bindAnyIPv4, uint32(port))); !ok {
		t.Fatal("expected a cancel naming the requested address to succeed")
	}
	if handler.listenerFor(bindAnyIPv4, port) != nil {
		t.Error("expected the forward to be removed after a cancel")
	}
}

func TestAcceptLoopRemovesItsOwnForward(t *testing.T) {
	handler, srv := testForwardHandler(t)

	listener, port, ok := handler.openForward(nil, srv, request(loopbackIPv4, 0))
	if !ok {
		t.Fatal("expected the forward to be accepted")
	}
	key := forwardKey(loopbackIPv4, port)

	_ = listener.Close()
	handler.acceptLoop(nil, listener, loopbackIPv4, port, key)

	if handler.listenerFor(loopbackIPv4, port) != nil {
		t.Error("expected the accept loop to remove its own forward")
	}
}

func TestAcceptLoopDoesNotEvictAReplacementForward(t *testing.T) {
	handler, srv := testForwardHandler(t)

	first, port, ok := handler.openForward(nil, srv, request(loopbackIPv4, 0))
	if !ok {
		t.Fatal("expected the first forward to be accepted")
	}
	key := forwardKey(loopbackIPv4, port)

	if ok, _ := handler.cancelForward(request(loopbackIPv4, uint32(port))); !ok {
		t.Fatal("expected the cancel to succeed")
	}

	second, reboundPort, ok := handler.openForward(nil, srv, request(loopbackIPv4, uint32(port)))
	if !ok {
		t.Fatal("expected a second forward on the same port to be accepted")
	}
	if reboundPort != port {
		t.Fatalf("expected the same port, got %d and %d", port, reboundPort)
	}
	defer func() { _ = second.Close() }()

	handler.acceptLoop(nil, first, loopbackIPv4, port, key)

	if got := handler.listenerFor(loopbackIPv4, port); got != second {
		t.Fatalf("the first accept loop evicted the replacement forward: forwards[%s] = %v", key, got)
	}
	if ok, _ := handler.cancelForward(request(loopbackIPv4, uint32(port))); !ok {
		t.Error("expected the replacement forward to remain cancellable")
	}
}

func TestForwardHandlerIgnoresUnknownCancel(t *testing.T) {
	handler, _ := testForwardHandler(t)

	if ok, _ := handler.cancelForward(request(loopbackIPv4, 65000)); ok {
		t.Error("expected a cancel for an unknown forward to be refused")
	}
}

// Copyright (c) 2017,2018 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package zedcloud

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
)

const (
	maxRetryAttempts = 50
)

// WSTunnelClient represents a persistent tunnel that can cycle through many websockets.
// The conn field points to the latest websocket,
// but it's important to realize that there may be goroutines handling older
// websockets that are not fully closed yet running at any point in time
type WSTunnelClient struct {
	TunnelServerName string            // hostname[:port] string representation of remote tunnel server
	Tunnel           string            // websocket server to connect to (ws[s]://hostname[:port])
	DestURL          string            // formatted websocket endpoint URL
	LocalRelayServer string            // local server to send received requests to
	Timeout          time.Duration     // timeout on websocket
	Connected        bool              // true when we have an active connection to remote server
	Dialer           *websocket.Dialer // dialer connection initialized & tested for success
	exitChan         chan struct{}     // channel to tell the tunnel goroutines to end
	conn             *WSConnection     // reference to remote websocket connection
	retryOnFailCount int               // no of times the ws connection attempts have continuously failed
	requestSentChan  chan struct{}     // channel to inform that a new request was written to local relay
}

// WSConnection represents a single websocket connection
type WSConnection struct {
	ws              *websocket.Conn // websocket connection
	tun             *WSTunnelClient // link back to tunnel
	localConnection net.Conn        // connection to local relay
}

var wsWriterMutex sync.Mutex // mutex to allow a single goroutine to send a response at a time
var connMutex sync.Mutex     // mutex to allow a single goroutine to check and re-initialize connection if required

// InitializeTunnelClient returns a websocket tunnel client configured with the
// requested remote and local servers.
func InitializeTunnelClient(serverName string, localRelay string) *WSTunnelClient {
	tunnelClient := WSTunnelClient{
		TunnelServerName: serverName,
		Tunnel:           "wss://" + serverName,
		LocalRelayServer: localRelay,
		Timeout:          30 * time.Second,
	}

	return &tunnelClient
}

// Start triggers workflow to establish the websocket
// session with remote tunnel server
func (t *WSTunnelClient) Start() {
	go func() {
		t.startSession()
		<-make(chan struct{}, 0)
	}()
}

// TestConnection validates the configured parameters for correctness
// and further attempts an actual connection request to confirm
// if the client can successfully connect to remote backend server.
func (t *WSTunnelClient) TestConnection(proxyURL *url.URL, localAddr net.IP) error {

	if t.Tunnel == "" {
		return fmt.Errorf("Must specify tunnel server ws://hostname:port")
	}
	if !strings.HasPrefix(t.Tunnel, "ws://") && !strings.HasPrefix(t.Tunnel, "wss://") {
		return fmt.Errorf("Remote tunnel must begin with ws:// or wss://")
	}
	t.Tunnel = strings.TrimSuffix(t.Tunnel, "/")

	if t.LocalRelayServer == "" {
		return fmt.Errorf("Must specify local relay server hostOrIP:port")
	}
	if strings.HasPrefix(t.LocalRelayServer, "http://") && strings.HasPrefix(t.LocalRelayServer, "https://") {
		return fmt.Errorf("Local server relay must not begin with http:// or https://")
	}
	t.LocalRelayServer = strings.TrimSuffix(t.LocalRelayServer, "/")

	log.Debugf("Testing connection to %s on local address: %v, proxy: %v", t.Tunnel, localAddr, proxyURL)

	tlsConfig, err := GetTlsConfig(t.TunnelServerName, nil)
	if err != nil {
		log.Fatal(err)
	}
	dialer := &websocket.Dialer{
		ReadBufferSize:  100 * 1024,
		WriteBufferSize: 100 * 1024,
		TLSClientConfig: tlsConfig,
		NetDial: func(network, addr string) (net.Conn, error) {
			localTCPAddr := net.TCPAddr{IP: localAddr}
			netDialer := &net.Dialer{LocalAddr: &localTCPAddr}
			return netDialer.DialContext(context.Background(), network, addr)
		},
	}
	if proxyURL != nil {
		dialer.Proxy = http.ProxyURL(proxyURL)
	}

	pingURL := fmt.Sprintf("%s/api/v1/edgedevice/connection/ping", t.Tunnel)
	log.Debugf("Testing connection to ping url: %s", pingURL)
	_, resp, err := dialer.Dial(pingURL, nil)

	log.Debugf("Read ping response status code: %v for ping url: %s", resp.StatusCode, pingURL)

	if resp.StatusCode == http.StatusOK {
		url := fmt.Sprintf("%s/api/v1/edgedevice/connection/tunnel", t.Tunnel)
		t.DestURL = url
		t.Dialer = dialer
		log.Infof("Connection test succeeded for url: %s on local address: %v, proxy: %v", url, localAddr, proxyURL)
		return nil
	}
	return err
}

// startSession connects to configured backend on a
// secure websocket and waits for commands from the backend
// to forward to local relay.
func (t *WSTunnelClient) startSession() error {

	// signal that tells tunnel client to exit instead of reopening
	// a fresh connection.
	t.exitChan = make(chan struct{}, 1)
	t.requestSentChan = make(chan struct{}, 1)

	t.retryOnFailCount = 0

	// Keep opening websocket connections to tunnel requests
	go func() {
		log.Debug("Looping through websocket connection requests")
		for {
			if t.retryOnFailCount == maxRetryAttempts {
				log.Errorf("Shutting down tunnel client after %d failed attempts.", maxRetryAttempts)
				break
			}
			// Retry timer of 30 seconds between attempts.
			timer := time.NewTimer(30 * time.Second)

			log.Debugf("Attempting WS connection to url: %s", t.DestURL)

			ws, resp, err := t.Dialer.Dial(t.DestURL, nil)
			if err != nil {
				extra := ""
				if resp != nil {
					extra = resp.Status
					buf := make([]byte, 80)
					resp.Body.Read(buf)
					if len(buf) > 0 {
						extra = extra + " -- " + string(buf)
					}
					resp.Body.Close()
					log.Errorf("Error opening connection: %v, response: %v", err.Error(), resp)
				}
				t.retryOnFailCount++
			} else {
				t.conn = &WSConnection{ws: ws, tun: t}
				// Safety setting
				ws.SetReadLimit(100 * 1024 * 1024)
				// Request Loop
				t.Connected = true
				t.retryOnFailCount = 0
				t.conn.handleRequests()
				t.Connected = false
			}
			// check whether we need to exit
			select {
			case <-t.exitChan:
				break
			default: // non-blocking receive
			}

			// ensure we don't open connections too rapidly,
			<-timer.C
		}
	}()

	return nil
}

// Stop tunnel client
func (t *WSTunnelClient) Stop() {
	log.Info("Shutting down WS tunnel client and exiting.")
	t.exitChan <- struct{}{}
}

// handleRequests reads a request from the socket, then forks
// a goroutine to relay the request locally and optionally
// return the result if any.
func (wsc *WSConnection) handleRequests() {
	go wsc.pinger()
	go wsc.processResponses()
	for {
		wsc.ws.SetReadDeadline(time.Time{}) // separate ping-pong routine does timeout
		messageType, reader, err := wsc.ws.NextReader()
		if err != nil {
			log.Debugf("WS ReadMessage Error: %s", err.Error())
			break
		}
		if messageType != websocket.BinaryMessage {
			log.Debugf("WS ReadMessage Invalid message type: %d", messageType)
			break
		}
		// give the sender a minute to produce the request
		wsc.ws.SetReadDeadline(time.Now().Add(time.Minute))
		// read request id
		var id int16
		_, err = fmt.Fscanf(io.LimitReader(reader, 4), "%04x", &id)
		if err != nil {
			log.Debugf("WS cannot read request ID Error: %s", err.Error())
			break
		}
		// read the whole message, this is bounded (to something large) by the
		// SetReadLimit on the websocket. We have to do this because we want to handle
		// the request in a goroutine (see "go process..Request" calls below) and the
		// websocket doesn't allow us to have multiple goroutines reading...
		request, err := ioutil.ReadAll(reader)
		if err != nil {
			log.Debugf("[id=%d] WS cannot read request message Error: %s", id, err.Error())
			break
		}
		log.Debugf("[id=%d] WS processing request payload: %v", id, string(request))

		// Finish off while we read the next request
		if len(request) > 0 {
			if err := wsc.processRequest(id, request); err != nil {
				log.Error(err)
			}
		} else {
			log.Debugf("[id=%d] Encountered WS request to process with no payload", id)
		}

	}
	// delay a few seconds to allow for writes to drain and then force-close the socket
	go func() {
		log.Info("Closing websocket connection")
		time.Sleep(5 * time.Second)
		wsc.ws.Close()
	}()
}

// Pinger that keeps connections alive and terminates them if they seem stuck
func (wsc *WSConnection) pinger() {
	defer func() {
		// panics may occur in WriteControl (in unit tests at least) for closed
		// websocket connections
		if x := recover(); x != nil {
			log.Errorf("Panic in pinger: %s", x)
		}
	}()
	log.Infof("pinger starting for websocket connection to: %s", wsc.tun.DestURL)
	tunTimeout := wsc.tun.Timeout

	// timeout handler sends a close message, waits a few seconds, then kills the socket
	timeout := func() {
		if wsc.ws == nil {
			return
		}
		wsc.ws.WriteControl(websocket.CloseMessage, nil, time.Now().Add(1*time.Second))
		log.Infof("ping timeout, closing websocket connection to: %s", wsc.tun.DestURL)
		time.Sleep(15 * time.Second)
		if wsc.ws != nil {
			wsc.ws.Close()
		}
	}
	// timeout timer
	timer := time.AfterFunc(tunTimeout, timeout)
	// pong handler resets last pong time
	ph := func(message string) error {
		timer.Reset(tunTimeout)
		return nil
	}
	wsc.ws.SetPongHandler(ph)
	// ping loop, ends when socket is closed...
	for {
		if wsc.ws == nil {
			log.Errorf("WS not found for destination: %s", wsc.tun.DestURL)
			break
		}
		err := wsc.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(tunTimeout/3))
		if err != nil {
			log.Errorf("WS WriteControl Error: %s", err.Error())
			break
		}
		time.Sleep(tunTimeout / 3)
	}
	log.Infof("pinger ending (WS errored or closed) for destination: %s", wsc.tun.DestURL)
	wsc.ws.Close()
}

// processRequest forwards the received message to local relay
// server and starts a separate go-routine to check for and return
// any responses that are optionally received.
func (wsc *WSConnection) processRequest(id int16, req []byte) (err error) {

	host := wsc.tun.LocalRelayServer
	if err := wsc.refreshLocalConnection(host, false); err != nil {
		return err
	}
	log.Debugf("[id=%d] Forwarding request: %v to local connection: %s", id, string(req), host)
	for tries := 1; tries <= 3; tries++ {
		_, err := wsc.localConnection.Write(req)
		if err == nil {
			log.Debugf("[id=%d] Completed writing request: \"%s\" to local connection",
				id, string(req))
			break
		} else {
			log.Debugf("[id=%d] Error encountered while writing request to local connection : %s",
				id, err.Error())
			if err := wsc.refreshLocalConnection(host, true); err != nil {
				return err
			}
		}
	}
	wsc.tun.requestSentChan <- struct{}{}
	return nil
}

// refreshLocalConnection checks if the cached connection is still
// valid or else creates & caches a new one. The forceCreate flag
// can be used to forcily update the cached local connection.
func (wsc *WSConnection) refreshLocalConnection(host string, forceCreate bool) (err error) {

	connMutex.Lock()
	defer connMutex.Unlock()

	if wsc.localConnection != nil && !forceCreate {
		c := wsc.localConnection
		one := []byte{}
		c.SetReadDeadline(time.Now())
		_, err := c.Read(one)
		if err != nil {
			log.Errorf("Error encountered while testing local connection: %s", err.Error())
			if err == io.EOF ||
				err == io.ErrClosedPipe ||
				err == io.ErrUnexpectedEOF {
				log.Debug("Lost local server connection, reconnecting...")
				if err := wsc.dialLocalConnection(); err != nil {
					return err
				}
			}
		}
	} else {
		if err := wsc.dialLocalConnection(); err != nil {
			return err
		}
	}
	return nil
}

// dialLocalConnection creates a new connection to local relay server.
func (wsc *WSConnection) dialLocalConnection() (err error) {

	host := wsc.tun.LocalRelayServer
	if host == "" {
		log.Error("Local server not found for WS connection")
		return
	}

	log.Debugf("Initializing local server connection: %s", host)
	localConnection, err := net.Dial("tcp", host)
	if err != nil {
		log.Errorf("Could not connect to local server: %s, error: %s", host, err.Error())
		return err
	}
	wsc.localConnection = localConnection
	log.Debugf("Successfully connected to local server: %s", host)
	return nil
}

// processResponses loops through waiting for responses from local relay
// connection and forwards any received messages to the websocket.
func (wsc *WSConnection) processResponses() {

	host := wsc.tun.LocalRelayServer
	log.Infof("Processing responses from local relay: %s", host)

	var id int64
	for {
		select {
		case <-wsc.tun.requestSentChan:

			if err := wsc.refreshLocalConnection(host, false); err != nil {
				log.Errorf("Error encountered while refreshing local connection: %s", err.Error())
				break
			}
			wsc.localConnection.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			responseBuffer := make([]byte, 524288)
			responseBuffer, _ = ioutil.ReadAll(wsc.localConnection)
			num := len(responseBuffer)
			if num > 0 {
				response := responseBuffer[:num]
				log.Debugf("[id=%d] Read local connection payload: \"%s\"", id, string(response))

				wsc.writeResponseMessage(id, bytes.NewBuffer(response))
				id++
			}
		default:
		}

		// check whether we need to exit
		select {
		case <-wsc.tun.exitChan:
			break
		default: // non-blocking receive
		}
	}
}

// writeResponseMessage forwards the response message on the websocket.
func (wsc *WSConnection) writeResponseMessage(id int64, resp *bytes.Buffer) {
	// Get writer's lock
	wsWriterMutex.Lock()
	defer wsWriterMutex.Unlock()
	// Write response into the tunnel
	wsc.ws.SetWriteDeadline(time.Now().Add(time.Minute))
	writer, err := wsc.ws.NextWriter(websocket.BinaryMessage)
	// got an error, reply with a "hey, retry" to the request handler
	if err != nil {
		log.Errorf("[id=%d] WS could not find writer: %s", id, err.Error())
		wsc.ws.Close()
		return
	}

	// write the request Id
	_, err = fmt.Fprintf(writer, "%04x", id)
	if err != nil {
		wsc.ws.Close()
		return
	}

	// write the response itself
	num, err := io.Copy(writer, resp)
	if err != nil {
		log.Errorf("WS cannot write response: %s", err.Error())
		wsc.ws.Close()
		return
	}
	log.Debugf("[id=%d] Completed writing response of length: %d", id, num)

	// done
	err = writer.Close()
	if err != nil {
		wsc.ws.Close()
		return
	}
}

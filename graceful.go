package graceful

import (
	"crypto/tls"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/netutil"
)

// Server wraps an http.Server with graceful connection handling.
// It may be used directly in the same way as http.Server, or may
// be constructed with the global functions in this package.
//
// Example:
//	srv := &graceful.Server{
//		Timeout: 5 * time.Second,
//		Server: &http.Server{Addr: ":1234", Handler: handler},
//	}
//	srv.ListenAndServe()
type Server struct {
	*http.Server

	// Timeout is the duration to allow outstanding requests to survive
	// before forcefully terminating them.
	Timeout time.Duration

	// Limit the number of outstanding requests
	ListenLimit int

	// TCPKeepAlive sets the TCP keep-alive timeouts on accepted
	// connections. It prunes dead TCP connections ( e.g. closing
	// laptop mid-download)
	TCPKeepAlive time.Duration

	// ConnState specifies an optional callback function that is
	// called when a client connection changes state. This is a proxy
	// to the underlying http.Server's ConnState, and the original
	// must not be set directly.
	ConnState func(net.Conn, http.ConnState)

	// BeforeShutdown is an optional callback function that is called
	// before the listener is closed.
	BeforeShutdown func()

	// ShutdownInitiated is an optional callback function that is called
	// when shutdown is initiated. It can be used to notify the client
	// side of long lived connections (e.g. websockets) to reconnect.
	ShutdownInitiated func()

	// NoSignalHandling prevents graceful from automatically shutting down
	// on SIGINT and SIGTERM. If set to true, you must shut down the server
	// manually with Stop().
	NoSignalHandling bool

	// Logger used to notify of errors on startup and on stop.
	Logger *log.Logger

	// Interrupted is true if the server is handling a SIGINT or SIGTERM
	// signal and is thus shutting down.
	Interrupted bool

	// interrupt signals the listener to stop serving connections,
	// and the server to shut down.
	interrupt chan os.Signal

	// stopLock is used to protect against concurrent calls to Stop
	stopLock sync.Mutex

	// stopChan is the channel on which callers may block while waiting for
	// the server to stop.
	stopChan chan struct{}

	// chanLock is used to protect access to the various channel constructors.
	chanLock sync.RWMutex

	// connections holds all connections managed by graceful
	connections map[net.Conn]struct{}
}

// Run serves the http.Handler with graceful shutdown enabled.
//
// timeout is the duration to wait until killing active requests and stopping the server.
// If timeout is 0, the server never times out. It waits for all active requests to finish.
func Run(addr string, timeout time.Duration, n http.Handler) {
	srv := &Server{
		Timeout:      timeout,
		TCPKeepAlive: 3 * time.Minute,
		Server:       &http.Server{Addr: addr, Handler: n},
		Logger:       DefaultLogger(),
	}

	if err := srv.ListenAndServe(); err != nil {
		if opErr, ok := err.(*net.OpError); !ok || (ok && opErr.Op != "accept") {
			srv.Logger.Fatal(err)
		}
	}

}

// RunWithErr is an alternative version of Run function which can return error.
//
// Unlike Run this version will not exit the program if an error is encountered but will
// return it instead.
func RunWithErr(addr string, timeout time.Duration, n http.Handler) error {
	srv := &Server{
		Timeout:      timeout,
		TCPKeepAlive: 3 * time.Minute,
		Server:       &http.Server{Addr: addr, Handler: n},
		Logger:       DefaultLogger(),
	}

	return srv.ListenAndServe()
}

// ListenAndServe is equivalent to http.Server.ListenAndServe with graceful shutdown enabled.
//
// timeout is the duration to wait until killing active requests and stopping the server.
// If timeout is 0, the server never times out. It waits for all active requests to finish.
func ListenAndServe(server *http.Server, timeout time.Duration) error {
	srv := &Server{Timeout: timeout, Server: server, Logger: DefaultLogger()}
	return srv.ListenAndServe()
}

// ListenAndServe is equivalent to http.Server.ListenAndServe with graceful shutdown enabled.
func (srv *Server) ListenAndServe() error {
	// Create the listener so we can control their lifetime
	addr := srv.Addr
	if addr == "" {
		addr = ":http"
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	return srv.Serve(l)
}

// ListenAndServeTLS is equivalent to http.Server.ListenAndServeTLS with graceful shutdown enabled.
//
// timeout is the duration to wait until killing active requests and stopping the server.
// If timeout is 0, the server never times out. It waits for all active requests to finish.
func ListenAndServeTLS(server *http.Server, certFile, keyFile string, timeout time.Duration) error {
	srv := &Server{Timeout: timeout, Server: server, Logger: DefaultLogger()}
	return srv.ListenAndServeTLS(certFile, keyFile)
}

// ListenTLS is a convenience method that creates an https listener using the
// provided cert and key files. Use this method if you need access to the
// listener object directly. When ready, pass it to the Serve method.
func (srv *Server) ListenTLS(certFile, keyFile string) (net.Listener, error) {
	// Create the listener ourselves so we can control its lifetime
	addr := srv.Addr
	if addr == "" {
		addr = ":https"
	}

	config := &tls.Config{}
	if srv.TLSConfig != nil {
		*config = *srv.TLSConfig
	}
	if config.NextProtos == nil {
		config.NextProtos = []string{"http/1.1"}
	}

	var err error
	config.Certificates = make([]tls.Certificate, 1)
	config.Certificates[0], err = tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}

	conn, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	tlsListener := tls.NewListener(conn, config)
	return tlsListener, nil
}

// ListenAndServeTLS is equivalent to http.Server.ListenAndServeTLS with graceful shutdown enabled.
func (srv *Server) ListenAndServeTLS(certFile, keyFile string) error {
	l, err := srv.ListenTLS(certFile, keyFile)
	if err != nil {
		return err
	}

	return srv.Serve(l)
}

// ListenAndServeTLSConfig can be used with an existing TLS config and is equivalent to
// http.Server.ListenAndServeTLS with graceful shutdown enabled,
func (srv *Server) ListenAndServeTLSConfig(config *tls.Config) error {
	addr := srv.Addr
	if addr == "" {
		addr = ":https"
	}

	conn, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	tlsListener := tls.NewListener(conn, config)
	return srv.Serve(tlsListener)
}

// Serve is equivalent to http.Server.Serve with graceful shutdown enabled.
//
// timeout is the duration to wait until killing active requests and stopping the server.
// If timeout is 0, the server never times out. It waits for all active requests to finish.
func Serve(server *http.Server, l net.Listener, timeout time.Duration) error {
	srv := &Server{Timeout: timeout, Server: server, Logger: DefaultLogger()}
	return srv.Serve(l)
}

// Serve is equivalent to http.Server.Serve with graceful shutdown enabled.
func (srv *Server) Serve(listener net.Listener) error {

	if srv.ListenLimit != 0 {
		listener = netutil.LimitListener(listener, srv.ListenLimit)
	}

	if srv.TCPKeepAlive != 0 {
		listener = tcpKeepAliveListener{listener.(*net.TCPListener), srv.TCPKeepAlive}
	}

	// Track connection state
	add := make(chan net.Conn)
	remove := make(chan net.Conn)

	srv.Server.ConnState = func(conn net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			add <- conn
		case http.StateClosed, http.StateHijacked:
			remove <- conn
		}
		if srv.ConnState != nil {
			srv.ConnState(conn, state)
		}
	}

	// Manage open connections
	shutdown := make(chan chan struct{})
	kill := make(chan struct{})
	go srv.manageConnections(add, remove, shutdown, kill)

	interrupt := srv.interruptChan()
	// Set up the interrupt handler
	if !srv.NoSignalHandling {
		signal.Notify(interrupt, syscall.SIGINT, syscall.SIGTERM)
	}
	quitting := make(chan struct{})
	go srv.handleInterrupt(interrupt, quitting, listener)

	// Serve with graceful listener.
	// Execution blocks here until listener.Close() is called, above.
	err := srv.Server.Serve(listener)
	if err != nil {
		// If the underlying listening is closed, Serve returns an error
		// complaining about listening on a closed socket. This is expected, so
		// let's ignore the error if we are the ones who explicitly closed the
		// socket.
		select {
		case <-quitting:
			err = nil
		default:
		}
	}

	srv.shutdown(shutdown, kill)

	return err
}

// Stop instructs the type to halt operations and close
// the stop channel when it is finished.
//
// timeout is grace period for which to wait before shutting
// down the server. The timeout value passed here will override the
// timeout given when constructing the server, as this is an explicit
// command to stop the server.
func (srv *Server) Stop(timeout time.Duration) {
	srv.stopLock.Lock()
	defer srv.stopLock.Unlock()

	srv.Timeout = timeout
	interrupt := srv.interruptChan()
	interrupt <- syscall.SIGINT
}

// StopChan gets the stop channel which will block until
// stopping has completed, at which point it is closed.
// Callers should never close the stop channel.
func (srv *Server) StopChan() <-chan struct{} {
	srv.chanLock.Lock()
	defer srv.chanLock.Unlock()

	if srv.stopChan == nil {
		srv.stopChan = make(chan struct{})
	}
	return srv.stopChan
}

// DefaultLogger returns the logger used by Run, RunWithErr, ListenAndServe, ListenAndServeTLS and Serve.
// The logger outputs to STDERR by default.
func DefaultLogger() *log.Logger {
	return log.New(os.Stderr, "[graceful] ", 0)
}

func (srv *Server) manageConnections(add, remove chan net.Conn, shutdown chan chan struct{}, kill chan struct{}) {
	var done chan struct{}
	srv.connections = map[net.Conn]struct{}{}
	for {
		select {
		case conn := <-add:
			srv.connections[conn] = struct{}{}
		case conn := <-remove:
			delete(srv.connections, conn)
			if done != nil && len(srv.connections) == 0 {
				done <- struct{}{}
				return
			}
		case done = <-shutdown:
			if len(srv.connections) == 0 {
				done <- struct{}{}
				return
			}
		case <-kill:
			for k := range srv.connections {
				if err := k.Close(); err != nil {
					srv.log("[ERROR] %s", err)
				}
			}
			return
		}
	}
}

func (srv *Server) interruptChan() chan os.Signal {
	srv.chanLock.Lock()
	defer srv.chanLock.Unlock()

	if srv.interrupt == nil {
		srv.interrupt = make(chan os.Signal, 1)
	}

	return srv.interrupt
}

func (srv *Server) handleInterrupt(interrupt chan os.Signal, quitting chan struct{}, listener net.Listener) {
	for _ = range interrupt {
		if srv.Interrupted {
			srv.log("already shutting down")
			continue
		}
		srv.log("shutdown initiated")
		srv.Interrupted = true
		if srv.BeforeShutdown != nil {
			srv.BeforeShutdown()
		}

		close(quitting)
		srv.SetKeepAlivesEnabled(false)
		if err := listener.Close(); err != nil {
			srv.log("[ERROR] %s", err)
		}

		if srv.ShutdownInitiated != nil {
			srv.ShutdownInitiated()
		}
	}
}

func (srv *Server) log(fmt string, v ...interface{}) {
	if srv.Logger != nil {
		srv.Logger.Printf(fmt, v...)
	}
}

func (srv *Server) shutdown(shutdown chan chan struct{}, kill chan struct{}) {
	// Request done notification
	done := make(chan struct{})
	shutdown <- done

	if srv.Timeout > 0 {
		select {
		case <-done:
		case <-time.After(srv.Timeout):
			close(kill)
		}
	} else {
		<-done
	}
	// Close the stopChan to wake up any blocked goroutines.
	srv.chanLock.Lock()
	if srv.stopChan != nil {
		close(srv.stopChan)
	}
	srv.chanLock.Unlock()
}

// tcpKeepAliveListener sets TCP keep-alive timeouts on accepted
// connections. It's used by ListenAndServe and ListenAndServeTLS so
// dead TCP connections (e.g. closing laptop mid-download) eventually
// go away.
type tcpKeepAliveListener struct {
	*net.TCPListener
	keepAlivePeriod time.Duration
}

func (ln tcpKeepAliveListener) Accept() (c net.Conn, err error) {
	tc, err := ln.AcceptTCP()
	if err != nil {
		return
	}
	tc.SetKeepAlive(true)
	tc.SetKeepAlivePeriod(ln.keepAlivePeriod)
	return tc, nil
}

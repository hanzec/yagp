package nodebridge

import (
	"crypto/sha256"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"yagp/pkg/nodegrpc"
)

type tunnelProbeDomain struct {
	listener *net.UnixListener
	pending  sync.Map // [sha256.Size]byte -> *net.TCPListener
}

func listenTunnelProbeDomain(configFile string) (*tunnelProbeDomain, error) {
	path := filepath.Join(filepath.Dir(configFile), nodegrpc.TunnelProbeDomain)
	// Gost and the daemon share an installation directory. Only remove a socket left by a previous daemon.
	if info, err := os.Lstat(path); err == nil && info.Mode().Type() == os.ModeSocket {
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	listen, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		_ = listen.Close()
		return nil, err
	}
	return &tunnelProbeDomain{listener: listen}, nil
}

func (p *tunnelProbeDomain) serve() {
	for {
		conn, err := p.listener.AcceptUnix()
		if err != nil {
			return
		}
		go p.respond(conn)
	}
}

func (p *tunnelProbeDomain) respond(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(tunnelProbeTimeout))
	var token [sha256.Size]byte
	if _, err := io.ReadFull(conn, token[:]); err != nil {
		return
	}
	listener, found := p.pending.LoadAndDelete(token)
	if !found {
		return
	}
	_ = listener.(*net.TCPListener).Close()
	response := sha256.Sum256(token[:])
	_, _ = conn.Write(response[:])
}

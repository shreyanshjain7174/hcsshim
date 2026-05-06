//go:build linux
// +build linux

package stdio

import (
	"fmt"
	"io"
	"os"

	"github.com/Microsoft/hcsshim/internal/guest/transport"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type Conn interface {
	io.ReadWriteCloser
	CloseRead() error
	CloseWrite() error
}

// ConnectionSettings describe the stdin, stdout, stderr ports to connect the
// transport to. A nil port specifies no connection.
type ConnectionSettings struct {
	StdIn  *uint32
	StdOut *uint32
	StdErr *uint32
}

type logConnection struct {
	con  Conn
	port uint32
}

func (lc *logConnection) File() (*os.File, error) {
	filer, ok := lc.con.(interface{ File() (*os.File, error) })
	if !ok {
		return nil, fmt.Errorf("con does not support File")
	}
	return filer.File()
}

func (lc *logConnection) Read(b []byte) (int, error) {
	return lc.con.Read(b)
}

func (lc *logConnection) Write(b []byte) (int, error) {
	return lc.con.Write(b)
}

func (lc *logConnection) Close() error {
	logrus.WithFields(logrus.Fields{
		"port": lc.port,
	}).Debug("opengcs::logConnection::Close - closing connection")

	return lc.con.Close()
}

func (lc *logConnection) CloseRead() error {
	logrus.WithFields(logrus.Fields{
		"port": lc.port,
	}).Debug("opengcs::logConnection::Close - closing read connection")

	return lc.con.CloseRead()
}

func (lc *logConnection) CloseWrite() error {
	logrus.WithFields(logrus.Fields{
		"port": lc.port,
	}).Debug("opengcs::logConnection::Close - closing write connection")

	return lc.con.CloseWrite()
}

var _ = (Conn)(&logConnection{})

// Connect returns new transport.Connection instances, one for each stdio pipe
// to be used. If CreateStd*Pipe for a given pipe is false, the given Connection
// is set to nil. Each connection is wrapped in a ConnSlot so the underlying
// vsock can be replaced when the bridge reconnects after live migration.
func Connect(tport transport.Transport, settings ConnectionSettings) (_ *ConnectionSet, err error) {
	connSet := &ConnectionSet{}
	defer func() {
		if err != nil {
			connSet.Close()
		}
	}()
	if settings.StdIn != nil {
		port := *settings.StdIn
		logrus.WithField("port", port).Info("connecting to stdin port")
		c, err := tport.Dial(port)
		if err != nil {
			return nil, errors.Wrap(err, "failed creating stdin Connection")
		}
		connSet.In = NewConnSlot(&logConnection{con: c, port: port}, redialer(tport, port))
	}
	if settings.StdOut != nil {
		port := *settings.StdOut
		logrus.WithField("port", port).Info("connecting to stdout port")
		c, err := tport.Dial(port)
		if err != nil {
			return nil, errors.Wrap(err, "failed creating stdout Connection")
		}
		connSet.Out = NewConnSlot(&logConnection{con: c, port: port}, redialer(tport, port))
	}
	if settings.StdErr != nil {
		port := *settings.StdErr
		logrus.WithField("port", port).Info("connecting to stderr port")
		c, err := tport.Dial(port)
		if err != nil {
			return nil, errors.Wrap(err, "failed creating stderr Connection")
		}
		connSet.Err = NewConnSlot(&logConnection{con: c, port: port}, redialer(tport, port))
	}
	return connSet, nil
}

// redialer returns a callback that re-dials the given vsock port via the
// provided transport. Used by ConnSlot to recover from a bridge disconnect:
// after live migration the source-host listener is gone but the destination
// host has a fresh listener on the same port number.
func redialer(tport transport.Transport, port uint32) func() (transport.Connection, error) {
	return func() (transport.Connection, error) {
		logrus.WithField("port", port).Info("ConnSlot: redialing port")
		nc, err := tport.Dial(port)
		if err != nil {
			return nil, err
		}
		return &logConnection{con: nc, port: port}, nil
	}
}

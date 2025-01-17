package dns

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"os"
	"time"

	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/buf"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/common/task"

	"golang.org/x/net/dns/dnsmessage"
)

var _ Transport = (*TLSTransport)(nil)

type TLSTransport struct {
	myTransportAdapter
}

func NewTLSTransport(ctx context.Context, dialer N.Dialer, destination M.Socksaddr) *TLSTransport {
	return &TLSTransport{
		myTransportAdapter{
			ctx:         ctx,
			dialer:      dialer,
			destination: destination,
			done:        make(chan struct{}),
		},
	}
}

func (t *TLSTransport) offer(ctx context.Context) (*dnsConnection, error) {
	t.access.RLock()
	connection := t.connection
	t.access.RUnlock()
	if connection != nil {
		select {
		case <-connection.done:
		default:
			return connection, nil
		}
	}
	t.access.Lock()
	defer t.access.Unlock()
	connection = t.connection
	if connection != nil {
		select {
		case <-connection.done:
		default:
			return connection, nil
		}
	}
	tcpConn, err := t.dialer.DialContext(t.ctx, N.NetworkTCP, t.destination)
	if err != nil {
		return nil, err
	}
	tlsConn := tls.Client(tcpConn, &tls.Config{
		ServerName: t.destination.AddrString(),
	})
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = tlsConn.HandshakeContext(ctx)
	cancel()
	if err != nil {
		return nil, err
	}
	connection = &dnsConnection{
		Conn:      tlsConn,
		done:      make(chan struct{}),
		callbacks: make(map[uint16]chan *dnsmessage.Message),
	}
	t.connection = connection
	go t.newConnection(connection)
	return connection, nil
}

func (t *TLSTransport) newConnection(conn *dnsConnection) {
	defer close(conn.done)
	defer conn.Close()
	err := task.Any(t.ctx, func(ctx context.Context) error {
		return t.loopIn(conn)
	}, func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return nil
		case <-t.done:
			return os.ErrClosed
		}
	})
	conn.err = err
}

func (t *TLSTransport) loopIn(conn *dnsConnection) error {
	_buffer := buf.StackNewSize(1024)
	defer common.KeepAlive(_buffer)
	buffer := common.Dup(_buffer)
	defer buffer.Release()
	for {
		buffer.FullReset()
		_, err := buffer.ReadFullFrom(conn, 2)
		if err != nil {
			return err
		}
		length := binary.BigEndian.Uint16(buffer.Bytes())
		if length > 512 {
			return E.New("invalid length received: ", length)
		}
		buffer.FullReset()
		_, err = buffer.ReadFullFrom(conn, int(length))
		if err != nil {
			return err
		}
		var message dnsmessage.Message
		err = message.Unpack(buffer.Bytes())
		if err != nil {
			return err
		}
		conn.access.Lock()
		callback, loaded := conn.callbacks[message.ID]
		if loaded {
			delete(conn.callbacks, message.ID)
		}
		conn.access.Unlock()
		if !loaded {
			continue
		}
		callback <- &message
	}
}

func (t *TLSTransport) Exchange(ctx context.Context, message *dnsmessage.Message) (*dnsmessage.Message, error) {
	var connection *dnsConnection
	err := task.Run(ctx, func() error {
		var innerErr error
		connection, innerErr = t.offer(ctx)
		return innerErr
	})
	if err != nil {
		return nil, err
	}
	connection.access.Lock()
	connection.queryId++
	message.ID = connection.queryId
	callback := make(chan *dnsmessage.Message)
	connection.callbacks[message.ID] = callback
	connection.access.Unlock()
	_buffer := buf.StackNewSize(1024)
	defer common.KeepAlive(_buffer)
	buffer := common.Dup(_buffer)
	defer buffer.Release()
	length := buffer.Extend(2)
	rawMessage, err := message.AppendPack(buffer.Index(2))
	if err != nil {
		return nil, err
	}
	buffer.Truncate(2 + len(rawMessage))
	binary.BigEndian.PutUint16(length, uint16(len(rawMessage)))
	err = task.Run(ctx, func() error {
		return common.Error(connection.Write(buffer.Bytes()))
	})
	if err != nil {
		return nil, err
	}
	select {
	case response := <-callback:
		return response, nil
	case <-connection.done:
		return nil, connection.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

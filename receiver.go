package gosmpp

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linxGnu/gosmpp/pdu"
)

// ReceiveSettings is event listener for Receiver.
type ReceiveSettings struct {
	// OnPDU handles received PDU from SMSC.
	//
	// `Responded` flag indicates this pdu is responded automatically,
	// no manual respond needed.
	OnPDU func(p pdu.PDU, responded bool)

	// OnReceivingError notifies happened error while reading PDU
	// from SMSC.
	OnReceivingError func(error)

	// OnRebindingError notifies error while rebinding.
	OnRebindingError func(error)

	// OnClosed notifies `closed` event due to State.
	OnClosed func(State)

	response func(pdu.PDU)
}

type receiver struct {
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	settings ReceiveSettings
	conn     *Connection
	state    int32
}

// NewReceiver returns new Receiver, bound with inputStream stream.
func NewReceiver(conn *Connection, settings ReceiveSettings) Receiver {
	return newReceiver(conn, settings, true)
}

func newReceiver(conn *Connection, settings ReceiveSettings, startDaemon bool) *receiver {
	r := &receiver{
		settings: settings,
		conn:     conn,
	}
	r.ctx, r.cancel = context.WithCancel(context.Background())

	// start receiver daemon(s)
	if startDaemon {
		r.start()
	}

	return r
}

// SystemID returns tagged SystemID, returned from bind_resp from SMSC.
func (t *receiver) SystemID() string {
	return t.conn.systemID
}

// Close receiver, close connection and stop underlying daemons.
func (t *receiver) Close() (err error) {
	return t.close(ExplicitClosing)
}

func (t *receiver) close(state State) (err error) {
	if atomic.CompareAndSwapInt32(&t.state, 0, 1) {
		// cancel to notify stop
		t.cancel()

		// set read deadline for current blocking read
		_ = t.conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))

		// wait daemons
		t.wg.Wait()

		// close connection to notify daemons to stop
		if state != StoppingProcessOnly {
			err = t.conn.Close()
		}

		// notify receiver closed
		if t.settings.OnClosed != nil {
			t.settings.OnClosed(state)
		}
	}
	return
}

func (t *receiver) closing(state State) {
	go func() {
		_ = t.close(state)
	}()
}

func (t *receiver) start() {
	t.wg.Add(1)
	go func() {
		t.loop()
		t.wg.Done()
	}()
}

// check error and do closing if need
func (t *receiver) check(err error) (closing bool) {
	if err == nil {
		return
	}

	if t.settings.OnReceivingError != nil {
		t.settings.OnReceivingError(err)
	}

	closing = true
	return
}

// PDU loop processing
func (t *receiver) loop() {
	for {
		select {
		case <-t.ctx.Done():
			return
		default:
		}

		p, err := pdu.Parse(t.conn)

		if closeOnError := t.check(err); closeOnError || t.handleOrClose(p) {
			if closeOnError {
				t.closing(InvalidStreaming)
			}
			return
		}
	}
}

func (t *receiver) handleOrClose(p pdu.PDU) (closing bool) {
	if p != nil {
		switch pp := p.(type) {
		case *pdu.EnquireLink:
			if t.settings.response != nil {
				t.settings.response(pp.GetResponse())
			}

		case *pdu.Unbind:
			if t.settings.response != nil {
				t.settings.response(pp.GetResponse())

				// wait to send response before closing
				time.Sleep(50 * time.Millisecond)
			}

			closing = true
			t.closing(UnbindClosing)

		default:
			var responded bool
			if p.CanResponse() && t.settings.response != nil {
				t.settings.response(p.GetResponse())
				responded = true
			}

			if t.settings.OnPDU != nil {
				t.settings.OnPDU(p, responded)
			}
		}
	}
	return
}

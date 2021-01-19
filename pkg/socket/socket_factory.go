package socket

import (
	"context"
	"net"
	"os/exec"
	"sync"
	"time"

	"github.com/shirou/gopsutil/process"
	"github.com/spiral/errors"
	"github.com/spiral/goridge/v3/interfaces/relay"
	"github.com/spiral/goridge/v3/pkg/socket"
	"github.com/spiral/roadrunner/v2/interfaces/events"
	"github.com/spiral/roadrunner/v2/interfaces/worker"
	"github.com/spiral/roadrunner/v2/internal"
	workerImpl "github.com/spiral/roadrunner/v2/pkg/worker"

	"go.uber.org/multierr"
	"golang.org/x/sync/errgroup"
)

// Factory connects to external stack using socket server.
type Factory struct {
	// listens for incoming connections from underlying processes
	ls net.Listener

	// relay connection timeout
	tout time.Duration

	// sockets which are waiting for process association
	relays sync.Map

	ErrCh chan error
}

// todo: review

// NewSocketServer returns Factory attached to a given socket listener.
// tout specifies for how long factory should serve for incoming relay connection
func NewSocketServer(ls net.Listener, tout time.Duration) worker.Factory {
	f := &Factory{
		ls:     ls,
		tout:   tout,
		relays: sync.Map{},
		ErrCh:  make(chan error, 10),
	}

	// Be careful
	// https://github.com/go101/go101/wiki/About-memory-ordering-guarantees-made-by-atomic-operations-in-Go
	// https://github.com/golang/go/issues/5045
	go func() {
		f.ErrCh <- f.listen()
	}()

	return f
}

// blocking operation, returns an error
func (f *Factory) listen() error {
	errGr := &errgroup.Group{}
	errGr.Go(func() error {
		for {
			conn, err := f.ls.Accept()
			if err != nil {
				return err
			}

			rl := socket.NewSocketRelay(conn)
			pid, err := internal.FetchPID(rl)
			if err != nil {
				return err
			}

			f.attachRelayToPid(pid, rl)
		}
	})

	return errGr.Wait()
}

type socketSpawn struct {
	w   worker.BaseProcess
	err error
}

// SpawnWorker creates Process and connects it to appropriate relay or returns error
func (f *Factory) SpawnWorkerWithTimeout(ctx context.Context, cmd *exec.Cmd, listeners ...events.Listener) (worker.BaseProcess, error) {
	const op = errors.Op("factory_spawn_worker_with_timeout")
	c := make(chan socketSpawn)
	go func() {
		ctx, cancel := context.WithTimeout(ctx, f.tout)
		defer cancel()
		w, err := workerImpl.InitBaseWorker(cmd, workerImpl.AddListeners(listeners...))
		if err != nil {
			c <- socketSpawn{
				w:   nil,
				err: err,
			}
			return
		}

		err = w.Start()
		if err != nil {
			c <- socketSpawn{
				w:   nil,
				err: errors.E(op, err),
			}
			return
		}

		rl, err := f.findRelayWithContext(ctx, w)
		if err != nil {
			err = multierr.Combine(
				err,
				w.Kill(),
				w.Wait(),
			)

			c <- socketSpawn{
				w:   nil,
				err: errors.E(op, err),
			}
			return
		}

		w.AttachRelay(rl)
		w.State().Set(internal.StateReady)

		c <- socketSpawn{
			w:   w,
			err: nil,
		}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-c:
		if res.err != nil {
			return nil, res.err
		}

		return res.w, nil
	}
}

func (f *Factory) SpawnWorker(cmd *exec.Cmd, listeners ...events.Listener) (worker.BaseProcess, error) {
	const op = errors.Op("factory_spawn_worker")
	w, err := workerImpl.InitBaseWorker(cmd, workerImpl.AddListeners(listeners...))
	if err != nil {
		return nil, err
	}

	err = w.Start()
	if err != nil {
		return nil, errors.E(op, err)
	}

	rl, err := f.findRelay(w)
	if err != nil {
		err = multierr.Combine(
			err,
			w.Kill(),
			w.Wait(),
		)
		return nil, err
	}

	w.AttachRelay(rl)
	w.State().Set(internal.StateReady)

	return w, nil
}

// Close socket factory and underlying socket connection.
func (f *Factory) Close() error {
	return f.ls.Close()
}

// waits for Process to connect over socket and returns associated relay of timeout
func (f *Factory) findRelayWithContext(ctx context.Context, w worker.BaseProcess) (*socket.Relay, error) {
	ticker := time.NewTicker(time.Millisecond * 100)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			_, err := process.NewProcess(int32(w.Pid()))
			if err != nil {
				return nil, err
			}
		default:
			tmp, ok := f.relays.Load(w.Pid())
			if !ok {
				continue
			}
			return tmp.(*socket.Relay), nil
		}
	}
}

func (f *Factory) findRelay(w worker.BaseProcess) (*socket.Relay, error) {
	const op = errors.Op("factory_find_relay")
	// poll every 1ms for the relay
	pollDone := time.NewTimer(f.tout)
	for {
		select {
		case <-pollDone.C:
			return nil, errors.E(op, errors.Str("relay timeout"))
		default:
			tmp, ok := f.relays.Load(w.Pid())
			if !ok {
				continue
			}
			return tmp.(*socket.Relay), nil
		}
	}
}

// chan to store relay associated with specific pid
func (f *Factory) attachRelayToPid(pid int64, relay relay.Relay) {
	f.relays.Store(pid, relay)
}

// deletes relay chan associated with specific pid
func (f *Factory) removeRelayFromPid(pid int64) {
	f.relays.Delete(pid)
}

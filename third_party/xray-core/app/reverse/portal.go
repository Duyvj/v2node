package reverse

import (
	"context"
	"sync"
	"time"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/mux"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/common/signal"
	"github.com/xtls/xray-core/common/task"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/pipe"
	"google.golang.org/protobuf/proto"
)

type Portal struct {
	ohm    outbound.Manager
	tag    string
	domain string
	picker *StaticMuxPicker
	client *mux.ClientManager
}

func NewPortal(config *PortalConfig, ohm outbound.Manager) (*Portal, error) {
	if config.Tag == "" {
		return nil, errors.New("portal tag is empty")
	}

	if config.Domain == "" {
		return nil, errors.New("portal domain is empty")
	}

	picker, err := NewStaticMuxPicker()
	if err != nil {
		return nil, err
	}

	return &Portal{
		ohm:    ohm,
		tag:    config.Tag,
		domain: config.Domain,
		picker: picker,
		client: &mux.ClientManager{
			Picker: picker,
		},
	}, nil
}

func (p *Portal) Start() error {
	return p.ohm.AddHandler(context.Background(), &Outbound{
		portal: p,
		tag:    p.tag,
	})
}

func (p *Portal) Close() error {
	return errors.Combine(
		p.ohm.RemoveHandler(context.Background(), p.tag),
		p.picker.Close(),
	)
}

func (p *Portal) HandleConnection(ctx context.Context, link *transport.Link) error {
	outbounds := session.OutboundsFromContext(ctx)
	ob := outbounds[len(outbounds)-1]
	if ob == nil {
		return errors.New("outbound metadata not found").AtError()
	}

	if isDomain(ob.Target, p.domain) {
		muxClient, err := mux.NewClientWorker(*link, mux.ClientStrategy{})
		if err != nil {
			return errors.New("failed to create mux client worker").Base(err).AtWarning()
		}

		worker, err := NewPortalWorker(muxClient)
		if err != nil {
			return errors.New("failed to create portal worker").Base(err)
		}

		p.picker.AddWorker(worker)

		if _, ok := link.Reader.(*pipe.Reader); !ok {
			select {
			case <-ctx.Done():
			case <-muxClient.WaitClosed():
			}
		}
		return nil
	}

	if ob.Target.Network == net.Network_UDP && ob.OriginalTarget.Address != nil && ob.OriginalTarget.Address != ob.Target.Address {
		link.Reader = &buf.EndpointOverrideReader{Reader: link.Reader, Dest: ob.Target.Address, OriginalDest: ob.OriginalTarget.Address}
		link.Writer = &buf.EndpointOverrideWriter{Writer: link.Writer, Dest: ob.Target.Address, OriginalDest: ob.OriginalTarget.Address}
	}

	return p.client.Dispatch(ctx, link)
}

type Outbound struct {
	portal *Portal
	tag    string
}

func (o *Outbound) Tag() string {
	return o.tag
}

func (o *Outbound) Dispatch(ctx context.Context, link *transport.Link) {
	if err := o.portal.HandleConnection(ctx, link); err != nil {
		errors.LogInfoInner(ctx, err, "failed to process reverse connection")
		common.Interrupt(link.Writer)
		common.Interrupt(link.Reader)
	}
}

func (o *Outbound) Start() error {
	return nil
}

func (o *Outbound) Close() error {
	return nil
}

// SenderSettings implements outbound.Handler.
func (o *Outbound) SenderSettings() *serial.TypedMessage {
	return nil
}

// ProxySettings implements outbound.Handler.
func (o *Outbound) ProxySettings() *serial.TypedMessage {
	return nil
}

type StaticMuxPicker struct {
	access  sync.Mutex
	workers []*PortalWorker
	cTask   *task.Periodic
	closed  bool
}

func NewStaticMuxPicker() (*StaticMuxPicker, error) {
	p := &StaticMuxPicker{}
	p.cTask = &task.Periodic{
		Execute:  p.cleanup,
		Interval: time.Second * 30,
	}
	if err := p.cTask.Start(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *StaticMuxPicker) cleanup() error {
	p.access.Lock()
	if p.closed {
		p.access.Unlock()
		return nil
	}

	workers := p.workers
	activeWorkers := workers[:0]
	var closedWorkers []*PortalWorker
	for _, w := range workers {
		if !w.Closed() {
			activeWorkers = append(activeWorkers, w)
		} else {
			closedWorkers = append(closedWorkers, w)
		}
	}

	if len(activeWorkers) != len(workers) {
		clear(workers[len(activeWorkers):])
		switch {
		case len(activeWorkers) == 0:
			p.workers = nil
		case cap(activeWorkers) > 2*len(activeWorkers):
			p.workers = append([]*PortalWorker(nil), activeWorkers...)
		default:
			p.workers = activeWorkers
		}
	}
	p.access.Unlock()

	for _, w := range closedWorkers {
		if err := w.Close(); err != nil {
			errors.LogWarningInner(context.Background(), err, "failed to close inactive portal worker")
		}
	}

	return nil
}

func (p *StaticMuxPicker) PickAvailable() (*mux.ClientWorker, error) {
	p.access.Lock()
	defer p.access.Unlock()

	if p.closed {
		return nil, errors.New("mux worker picker is closed")
	}

	if len(p.workers) == 0 {
		return nil, errors.New("empty worker list")
	}

	var minIdx int = -1
	var minConn = ^uint32(0)
	for i, w := range p.workers {
		client, draining, closed := w.snapshot()
		if closed || client == nil || draining {
			continue
		}
		if client.IsFull() {
			continue
		}
		if connections := client.ActiveConnections(); connections < minConn {
			minConn = connections
			minIdx = i
		}
	}

	if minIdx == -1 {
		minConn = ^uint32(0)
		for i, w := range p.workers {
			client, _, closed := w.snapshot()
			if closed || client == nil || client.IsFull() {
				continue
			}
			if connections := client.ActiveConnections(); connections < minConn {
				minConn = connections
				minIdx = i
			}
		}
	}

	if minIdx != -1 {
		client, _, closed := p.workers[minIdx].snapshot()
		if !closed && client != nil {
			return client, nil
		}
	}

	return nil, errors.New("no mux client worker available")
}

func (p *StaticMuxPicker) AddWorker(worker *PortalWorker) {
	p.access.Lock()
	if p.closed {
		p.access.Unlock()
		common.CloseIfExists(worker)
		return
	}

	p.workers = append(p.workers, worker)
	p.access.Unlock()
}

func (p *StaticMuxPicker) Close() error {
	p.access.Lock()
	if p.closed {
		p.access.Unlock()
		return nil
	}
	p.closed = true
	workers := p.workers
	p.workers = nil
	cTask := p.cTask
	p.access.Unlock()

	var errs []error
	errs = append(errs, common.CloseIfExists(cTask))
	for _, worker := range workers {
		errs = append(errs, common.CloseIfExists(worker))
	}
	return errors.Combine(errs...)
}

type PortalWorker struct {
	access   sync.Mutex
	client   *mux.ClientWorker
	control  *task.Periodic
	writer   buf.Writer
	reader   buf.Reader
	draining bool
	counter  uint32
	timer    *signal.ActivityTimer
	closed   bool
}

// Close stops every resource owned by a portal worker. References are detached
// before the potentially blocking closes so a concurrent heartbeat can only
// finish with its local snapshots and cannot retain the worker afterwards.
func (w *PortalWorker) Close() error {
	if w == nil {
		return nil
	}

	w.access.Lock()
	if w.closed {
		w.access.Unlock()
		return nil
	}
	w.closed = true
	control := w.control
	client := w.client
	writer := w.writer
	reader := w.reader
	timer := w.timer
	w.control = nil
	w.client = nil
	w.writer = nil
	w.reader = nil
	w.timer = nil
	w.access.Unlock()

	var errs []error
	errs = append(errs, common.CloseIfExists(control))
	// Interrupt both pipes before waiting on any mux cleanup. In particular,
	// this releases a heartbeat blocked on a full 16 KiB control pipe.
	errs = append(errs, common.Interrupt(writer))
	errs = append(errs, common.Interrupt(reader))
	if timer != nil {
		timer.SetTimeout(0)
	}
	errs = append(errs, common.CloseIfExists(client))
	return errors.Combine(errs...)
}

func NewPortalWorker(client *mux.ClientWorker) (*PortalWorker, error) {
	opt := []pipe.Option{pipe.WithSizeLimit(16 * 1024)}
	uplinkReader, uplinkWriter := pipe.New(opt...)
	downlinkReader, downlinkWriter := pipe.New(opt...)

	ctx := context.Background()
	outbounds := []*session.Outbound{{
		Target: net.UDPDestination(net.DomainAddress(internalDomain), 0),
	}}
	ctx = session.ContextWithOutbounds(ctx, outbounds)
	f := client.Dispatch(ctx, &transport.Link{
		Reader: uplinkReader,
		Writer: downlinkWriter,
	})
	if !f {
		common.Interrupt(uplinkReader)
		common.Interrupt(uplinkWriter)
		common.Interrupt(downlinkReader)
		common.Interrupt(downlinkWriter)
		common.Close(client)
		return nil, errors.New("unable to dispatch control connection")
	}
	terminate := func() {
		client.Close()
	}
	w := &PortalWorker{
		client: client,
		reader: downlinkReader,
		writer: uplinkWriter,
		timer:  signal.CancelAfterInactivity(ctx, terminate, 24*time.Hour), // // prevent leak
	}
	w.control = &task.Periodic{
		Execute:  w.heartbeat,
		Interval: time.Second * 2,
	}
	if err := w.control.Start(); err != nil {
		_ = w.Close()
		return nil, errors.New("unable to start portal worker heartbeat").Base(err)
	}
	return w, nil
}

func (w *PortalWorker) heartbeat() error {
	w.access.Lock()
	if w.closed || w.client == nil || w.client.Closed() {
		w.access.Unlock()
		return errors.New("client worker stopped")
	}

	if w.draining || w.writer == nil {
		w.access.Unlock()
		return errors.New("already disposed")
	}

	client := w.client
	writer := w.writer
	reader := w.reader
	timer := w.timer
	msg := &Control{}
	msg.FillInRandom()

	drain := client.TotalConnections() > 256
	if drain {
		w.draining = true
		msg.State = Control_DRAIN
	}

	w.counter = (w.counter + 1) % 5
	send := drain || w.counter == 1
	w.access.Unlock()

	if send {
		b, err := proto.Marshal(msg)
		if err != nil {
			return err
		}
		mb := buf.MergeBytes(nil, b)
		if timer != nil {
			timer.Update()
		}
		err = writer.WriteMultiBuffer(mb)
		if drain {
			common.Close(writer)
			common.Interrupt(reader)
			w.access.Lock()
			if w.writer == writer {
				w.writer = nil
			}
			if w.reader == reader {
				w.reader = nil
			}
			w.access.Unlock()
		}
		return err
	}
	return nil
}

func (w *PortalWorker) IsFull() bool {
	client, _, closed := w.snapshot()
	return closed || client == nil || client.IsFull()
}

func (w *PortalWorker) Closed() bool {
	client, _, closed := w.snapshot()
	return closed || client == nil || client.Closed()
}

func (w *PortalWorker) snapshot() (*mux.ClientWorker, bool, bool) {
	w.access.Lock()
	defer w.access.Unlock()
	return w.client, w.draining, w.closed
}

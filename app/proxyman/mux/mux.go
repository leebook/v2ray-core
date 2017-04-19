package mux

//go:generate go run $GOPATH/src/v2ray.com/core/tools/generrorgen/main.go -pkg mux -path App,Proxyman,Mux

import (
	"context"
	"io"
	"sync"
	"time"

	"v2ray.com/core/app"
	"v2ray.com/core/app/dispatcher"
	"v2ray.com/core/app/log"
	"v2ray.com/core/app/proxyman"
	"v2ray.com/core/common/buf"
	"v2ray.com/core/common/errors"
	"v2ray.com/core/common/net"
	"v2ray.com/core/common/signal"
	"v2ray.com/core/proxy"
	"v2ray.com/core/transport/ray"
)

const (
	maxTotal = 128
)

type ClientManager struct {
	access  sync.Mutex
	clients []*Client
	proxy   proxy.Outbound
	dialer  proxy.Dialer
	config  *proxyman.MultiplexingConfig
}

func NewClientManager(p proxy.Outbound, d proxy.Dialer, c *proxyman.MultiplexingConfig) *ClientManager {
	return &ClientManager{
		proxy:  p,
		dialer: d,
		config: c,
	}
}

func (m *ClientManager) Dispatch(ctx context.Context, outboundRay ray.OutboundRay) error {
	m.access.Lock()
	defer m.access.Unlock()

	for _, client := range m.clients {
		if client.Dispatch(ctx, outboundRay) {
			return nil
		}
	}

	client, err := NewClient(m.proxy, m.dialer, m)
	if err != nil {
		return newError("failed to create client").Base(err)
	}
	m.clients = append(m.clients, client)
	client.Dispatch(ctx, outboundRay)
	return nil
}

func (m *ClientManager) onClientFinish() {
	m.access.Lock()
	defer m.access.Unlock()

	if len(m.clients) < 10 {
		return
	}

	activeClients := make([]*Client, 0, len(m.clients))

	for _, client := range m.clients {
		if !client.Closed() {
			activeClients = append(activeClients, client)
		}
	}
	m.clients = activeClients
}

type Client struct {
	sessionManager *SessionManager
	inboundRay     ray.InboundRay
	ctx            context.Context
	cancel         context.CancelFunc
	manager        *ClientManager
	session2Remove chan uint16
	concurrency    uint32
}

var muxCoolDestination = net.TCPDestination(net.DomainAddress("v1.mux.cool"), net.Port(9527))

func NewClient(p proxy.Outbound, dialer proxy.Dialer, m *ClientManager) (*Client, error) {
	ctx, cancel := context.WithCancel(context.Background())
	ctx = proxy.ContextWithTarget(ctx, muxCoolDestination)
	pipe := ray.NewRay(ctx)
	go p.Process(ctx, pipe, dialer)
	c := &Client{
		sessionManager: NewSessionManager(),
		inboundRay:     pipe,
		ctx:            ctx,
		cancel:         cancel,
		manager:        m,
		session2Remove: make(chan uint16, 16),
		concurrency:    m.config.Concurrency,
	}
	go c.fetchOutput()
	go c.monitor()
	return c, nil
}

func (m *Client) Closed() bool {
	select {
	case <-m.ctx.Done():
		return true
	default:
		return false
	}
}

func (m *Client) monitor() {
	defer m.manager.onClientFinish()

	for {
		select {
		case <-m.ctx.Done():
			m.sessionManager.Close()
			m.inboundRay.InboundInput().Close()
			m.inboundRay.InboundOutput().CloseError()
			return
		case <-time.After(time.Second * 6):
			size := m.sessionManager.Size()
			if size == 0 && m.sessionManager.CloseIfNoSession() {
				m.cancel()
			}
		}
	}
}

func fetchInput(ctx context.Context, s *Session, output buf.Writer) {
	dest, _ := proxy.TargetFromContext(ctx)
	writer := &Writer{
		dest:   dest,
		id:     s.ID,
		writer: output,
	}
	defer writer.Close()
	defer s.CloseUplink()

	log.Trace(newError("dispatching request to ", dest))
	data, _ := s.input.ReadTimeout(time.Millisecond * 500)
	if data != nil {
		if err := writer.Write(data); err != nil {
			log.Trace(newError("failed to write first payload").Base(err))
			return
		}
	}
	if err := buf.Copy(signal.BackgroundTimer(), s.input, writer); err != nil {
		log.Trace(newError("failed to fetch all input").Base(err))
	}
}

func (m *Client) Dispatch(ctx context.Context, outboundRay ray.OutboundRay) bool {
	numSession := m.sessionManager.Size()
	if numSession >= int(m.concurrency) || numSession >= maxTotal {
		return false
	}

	select {
	case <-m.ctx.Done():
		return false
	default:
	}

	s := m.sessionManager.Allocate()
	if s == nil {
		return false
	}
	s.input = outboundRay.OutboundInput()
	s.output = outboundRay.OutboundOutput()
	go fetchInput(ctx, s, m.inboundRay.InboundInput())
	return true
}

func drain(reader *Reader) error {
	data, err := reader.Read()
	if err != nil {
		return err
	}
	data.Release()
	return nil
}

func pipe(reader *Reader, writer buf.Writer) error {
	data, err := reader.Read()
	if err != nil {
		return err
	}
	return writer.Write(data)
}

func (m *Client) handleStatueKeepAlive(meta *FrameMetadata, reader *Reader) error {
	if meta.Option.Has(OptionData) {
		return drain(reader)
	}
	return nil
}

func (m *Client) handleStatusNew(meta *FrameMetadata, reader *Reader) error {
	if meta.Option.Has(OptionData) {
		return drain(reader)
	}
	return nil
}

func (m *Client) handleStatusKeep(meta *FrameMetadata, reader *Reader) error {
	if !meta.Option.Has(OptionData) {
		return nil
	}

	if s, found := m.sessionManager.Get(meta.SessionID); found {
		return pipe(reader, s.output)
	}
	return drain(reader)
}

func (m *Client) handleStatusEnd(meta *FrameMetadata, reader *Reader) error {
	if s, found := m.sessionManager.Get(meta.SessionID); found {
		s.CloseDownlink()
		s.output.Close()
	}
	if meta.Option.Has(OptionData) {
		return drain(reader)
	}
	return nil
}

func (m *Client) fetchOutput() {
	defer m.cancel()

	reader := NewReader(m.inboundRay.InboundOutput())
	for {
		meta, err := reader.ReadMetadata()
		if err != nil {
			if errors.Cause(err) != io.EOF {
				log.Trace(newError("failed to read metadata").Base(err))
			}
			break
		}

		switch meta.SessionStatus {
		case SessionStatusKeepAlive:
			err = m.handleStatueKeepAlive(meta, reader)
		case SessionStatusEnd:
			err = m.handleStatusEnd(meta, reader)
		case SessionStatusNew:
			err = m.handleStatusNew(meta, reader)
		case SessionStatusKeep:
			err = m.handleStatusKeep(meta, reader)
		default:
			log.Trace(newError("unknown status: ", meta.SessionStatus).AtWarning())
			return
		}

		if err != nil {
			log.Trace(newError("failed to process data").Base(err))
			return
		}
	}
}

type Server struct {
	dispatcher dispatcher.Interface
}

func NewServer(ctx context.Context) *Server {
	s := &Server{}
	space := app.SpaceFromContext(ctx)
	space.OnInitialize(func() error {
		d := dispatcher.FromSpace(space)
		if d == nil {
			return newError("no dispatcher in space")
		}
		s.dispatcher = d
		return nil
	})
	return s
}

func (s *Server) Dispatch(ctx context.Context, dest net.Destination) (ray.InboundRay, error) {
	if dest != muxCoolDestination {
		return s.dispatcher.Dispatch(ctx, dest)
	}

	ray := ray.NewRay(ctx)
	worker := &ServerWorker{
		dispatcher:     s.dispatcher,
		outboundRay:    ray,
		sessionManager: NewSessionManager(),
	}
	go worker.run(ctx)
	return ray, nil
}

type ServerWorker struct {
	dispatcher     dispatcher.Interface
	outboundRay    ray.OutboundRay
	sessionManager *SessionManager
}

func handle(ctx context.Context, s *Session, output buf.Writer) {
	writer := NewResponseWriter(s.ID, output)
	if err := buf.Copy(signal.BackgroundTimer(), s.input, writer); err != nil {
		log.Trace(newError("session ", s.ID, " ends: ").Base(err))
	}
	writer.Close()
	s.CloseDownlink()
}

func (w *ServerWorker) handleStatusKeepAlive(meta *FrameMetadata, reader *Reader) error {
	if meta.Option.Has(OptionData) {
		return drain(reader)
	}
	return nil
}

func (w *ServerWorker) handleStatusNew(ctx context.Context, meta *FrameMetadata, reader *Reader) error {
	log.Trace(newError("received request for ", meta.Target))
	inboundRay, err := w.dispatcher.Dispatch(ctx, meta.Target)
	if err != nil {
		if meta.Option.Has(OptionData) {
			drain(reader)
		}
		return newError("failed to dispatch request.").Base(err)
	}
	s := &Session{
		input:  inboundRay.InboundOutput(),
		output: inboundRay.InboundInput(),
		parent: w.sessionManager,
		ID:     meta.SessionID,
	}
	w.sessionManager.Add(s)
	go handle(ctx, s, w.outboundRay.OutboundOutput())
	if meta.Option.Has(OptionData) {
		return pipe(reader, s.output)
	}
	return nil
}

func (w *ServerWorker) handleStatusKeep(meta *FrameMetadata, reader *Reader) error {
	if !meta.Option.Has(OptionData) {
		return nil
	}
	if s, found := w.sessionManager.Get(meta.SessionID); found {
		return pipe(reader, s.output)
	}
	return drain(reader)
}

func (w *ServerWorker) handleStatusEnd(meta *FrameMetadata, reader *Reader) error {
	if s, found := w.sessionManager.Get(meta.SessionID); found {
		s.CloseUplink()
		s.output.Close()
	}
	if meta.Option.Has(OptionData) {
		return drain(reader)
	}
	return nil
}

func (w *ServerWorker) run(ctx context.Context) {
	input := w.outboundRay.OutboundInput()
	reader := NewReader(input)

	defer w.sessionManager.Close()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		meta, err := reader.ReadMetadata()
		if err != nil {
			log.Trace(newError("failed to read metadata").Base(err))
			return
		}

		switch meta.SessionStatus {
		case SessionStatusKeepAlive:
			err = w.handleStatusKeepAlive(meta, reader)
		case SessionStatusEnd:
			err = w.handleStatusEnd(meta, reader)
		case SessionStatusNew:
			err = w.handleStatusNew(ctx, meta, reader)
		case SessionStatusKeep:
			err = w.handleStatusKeep(meta, reader)
		default:
			log.Trace(newError("unknown status: ", meta.SessionStatus).AtWarning())
			return
		}

		if err != nil {
			log.Trace(newError("failed to process data").Base(err))
			return
		}
	}
}
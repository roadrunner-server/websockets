package websockets

import (
	"context"
	"net/http"
	"sync"

	"github.com/gobwas/ws"
	"github.com/goccy/go-json"
	"github.com/google/uuid"
	"github.com/roadrunner-server/api/v2/payload"
	"github.com/roadrunner-server/api/v2/plugins/config"
	"github.com/roadrunner-server/api/v2/plugins/pubsub"
	"github.com/roadrunner-server/api/v2/plugins/server"
	"github.com/roadrunner-server/api/v2/pool"
	"github.com/roadrunner-server/api/v2/state/process"
	"github.com/roadrunner-server/api/v2/worker"
	"github.com/roadrunner-server/errors"
	poolImpl "github.com/roadrunner-server/sdk/v2/pool"
	processImpl "github.com/roadrunner-server/sdk/v2/state/process"
	"github.com/roadrunner-server/sdk/v2/utils"
	"github.com/roadrunner-server/websockets/v2/attributes"
	"github.com/roadrunner-server/websockets/v2/connection"
	"github.com/roadrunner-server/websockets/v2/executor"
	wsPool "github.com/roadrunner-server/websockets/v2/pool"
	"github.com/roadrunner-server/websockets/v2/validator"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

const (
	PluginName string = "websockets"

	RrMode          string = "RR_MODE"
	RrBroadcastPath string = "RR_BROADCAST_PATH"
	OriginHeaderKey string = "Origin"
)

type Plugin struct {
	sync.RWMutex

	// subscriber+reader interfaces
	subReader pubsub.SubReader
	// broadcaster
	broadcaster pubsub.Broadcaster

	cfg *Config
	log *zap.Logger

	// global connections map
	connections sync.Map

	// GO workers pool
	workersPool *wsPool.WorkersPool

	serveExit chan struct{}

	// workers pool
	phpPool pool.Pool
	// payloads pool
	pldPool sync.Pool
	// server which produces commands to the pool
	server server.Server

	// stop receiving messages
	cancel context.CancelFunc
	ctx    context.Context

	// function used to validate access to the requested resource
	accessValidator validator.AccessValidatorFn
}

func (p *Plugin) Init(cfg config.Configurer, log *zap.Logger, server server.Server, b pubsub.Broadcaster) error {
	const op = errors.Op("websockets_plugin_init")
	if !cfg.Has(PluginName) {
		return errors.E(op, errors.Disabled)
	}

	err := cfg.UnmarshalKey(PluginName, &p.cfg)
	if err != nil {
		return errors.E(op, err)
	}

	err = p.cfg.InitDefault()
	if err != nil {
		return errors.E(op, err)
	}

	p.serveExit = make(chan struct{})
	p.server = server
	p.log = new(zap.Logger)
	*p.log = *log
	p.broadcaster = b

	ctx, cancel := context.WithCancel(context.Background())
	p.ctx = ctx
	p.cancel = cancel

	p.pldPool = sync.Pool{
		New: func() interface{} {
			return &payload.Payload{
				Context: make([]byte, 0, 100),
				Body:    make([]byte, 0, 100),
			}
		},
	}

	return nil
}

func (p *Plugin) Serve() chan error {
	const op = errors.Op("websockets_plugin_serve")
	errCh := make(chan error, 1)
	// init broadcaster
	var err error
	p.subReader, err = p.broadcaster.GetDriver(p.cfg.Broker)
	if err != nil {
		errCh <- errors.E(op, err)
		return errCh
	}

	go func() {
		var err error
		p.Lock()
		defer p.Unlock()

		p.phpPool, err = p.server.NewWorkerPool(context.Background(), &poolImpl.Config{
			Debug:           p.cfg.Pool.Debug,
			NumWorkers:      p.cfg.Pool.NumWorkers,
			MaxJobs:         p.cfg.Pool.MaxJobs,
			AllocateTimeout: p.cfg.Pool.AllocateTimeout,
			DestroyTimeout:  p.cfg.Pool.DestroyTimeout,
			Supervisor:      p.cfg.Pool.Supervisor,
		}, map[string]string{RrMode: "http", RrBroadcastPath: p.cfg.Path}, nil)
		if err != nil {
			errCh <- errors.E(op, err)
			return
		}

		p.accessValidator = p.defaultAccessValidator()
	}()

	p.workersPool = wsPool.NewWorkersPool(p.subReader, &p.connections, p.log)

	// we need here only Reader part of the interface
	go func(ps pubsub.Reader) {
		for {
			data, err := ps.Next(p.ctx)
			if err != nil {
				if errors.Is(errors.TimeOut, err) {
					return
				}

				errCh <- errors.E(op, err)
				return
			}

			p.workersPool.Queue(data)
		}
	}(p.subReader)

	return errCh
}

func (p *Plugin) Stop() error {
	// close workers pool
	if p.workersPool != nil {
		p.workersPool.Stop()
	}
	// cancel context
	p.cancel()
	return nil
}

func (p *Plugin) Name() string {
	return PluginName
}

func (p *Plugin) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if val, ok := r.Context().Value(utils.OtelTracerNameKey).(string); ok {
			tp := trace.SpanFromContext(r.Context()).TracerProvider()
			ctx, span := tp.Tracer(val).Start(r.Context(), PluginName)
			defer span.End()
			r = r.WithContext(ctx)
		}

		if r.URL.Path != p.cfg.Path {
			next.ServeHTTP(w, r)
			return
		}

		// we need to lock here, because accessValidator might not be set in the Serve func at the moment
		p.RLock()
		// check origin
		if !isOriginAllowed(r.Header.Get(OriginHeaderKey), p.cfg) {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		// before we hijacked connection, we still can write to the response headers
		val, err := p.accessValidator(r)
		p.RUnlock()
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		if val.Status != http.StatusOK {
			for k, v := range val.Header {
				for i := 0; i < len(v); i++ {
					w.Header().Add(k, v[i])
				}
			}
			w.WriteHeader(val.Status)
			_, _ = w.Write(val.Body)
			return
		}

		_conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			p.log.Error("upgrade connection", zap.Error(err))
			return
		}

		// construct safe connection protected by mutexes
		safeConn := connection.NewConnection(_conn, p.log)
		// generate UUID from the connection
		connectionID := uuid.NewString()
		// store connection
		p.connections.Store(connectionID, safeConn)

		// Executor wraps a connection to have a safe abstraction
		e := executor.NewExecutor(safeConn, p.log, connectionID, p.subReader, p.accessValidator, r)
		p.log.Debug("websocket client connected", zap.String("uuid", connectionID))

		err = e.StartCommandLoop()
		if err != nil {
			p.log.Error("command loop error, disconnecting", zap.Error(err))
			return
		}

		// when exiting - delete the connection
		p.connections.Delete(connectionID)

		// remove connection from all topics from all pub-sub drivers
		e.CleanUp()

		err = r.Body.Close()
		if err != nil {
			p.log.Error("body close", zap.Error(err))
		}

		// close the connection on exit
		err = safeConn.Close()
		if err != nil {
			p.log.Error("connection close", zap.Error(err))
		}

		safeConn = nil
		p.log.Debug("disconnected", zap.String("connectionID", connectionID))
	})
}

// Workers returns slice with the process states for the workers
func (p *Plugin) Workers() []*process.State {
	p.RLock()
	defer p.RUnlock()

	workers := p.workers()

	ps := make([]*process.State, 0, len(workers))
	for i := 0; i < len(workers); i++ {
		state, err := processImpl.WorkerProcessState(workers[i])
		if err != nil {
			return nil
		}
		ps = append(ps, state)
	}

	return ps
}

// internal
func (p *Plugin) workers() []worker.BaseProcess {
	return p.phpPool.Workers()
}

// Reset destroys the old pool and replaces it with new one, waiting for old pool to die
func (p *Plugin) Reset() error {
	p.Lock()
	defer p.Unlock()
	const op = errors.Op("ws_plugin_reset")
	p.log.Info("reset signal was received")
	err := p.phpPool.Reset(context.Background())
	if err != nil {
		return errors.E(op, err)
	}

	p.log.Info("plugin was successfully reset")
	return nil
}

func (p *Plugin) defaultAccessValidator() validator.AccessValidatorFn {
	return func(r *http.Request, topics ...string) (*validator.AccessValidator, error) {
		const op = errors.Op("access_validator")

		p.log.Debug("validation", zap.Strings("topics", topics))
		r = attributes.Init(r)

		// if channels len is eq to 0, we use serverValidator
		if len(topics) == 0 {
			ctx, err := validator.ServerAccessValidator(r)
			if err != nil {
				return nil, errors.E(op, err)
			}

			val, err := p.exec(ctx)
			if err != nil {
				return nil, errors.E(err)
			}

			return val, nil
		}

		ctx, err := validator.TopicsAccessValidator(r, topics...)
		if err != nil {
			return nil, errors.E(op, err)
		}

		val, err := p.exec(ctx)
		if err != nil {
			return nil, errors.E(op)
		}

		if val.Status != http.StatusOK {
			return val, errors.E(op, errors.Errorf("access forbidden, code: %d", val.Status))
		}

		return val, nil
	}
}

func (p *Plugin) putPld(pld *payload.Payload) {
	pld.Context = make([]byte, 0, 100)
	pld.Body = make([]byte, 0, 100)
	p.pldPool.Put(pld)
}

func (p *Plugin) getPld() *payload.Payload {
	return p.pldPool.Get().(*payload.Payload)
}

func (p *Plugin) exec(ctx []byte) (*validator.AccessValidator, error) {
	const op = errors.Op("exec")
	pd := p.getPld()
	defer p.putPld(pd)

	pd.Context = ctx

	p.RLock()
	rsp, err := p.phpPool.Exec(pd)
	p.RUnlock()
	if err != nil {
		return nil, errors.E(op, err)
	}

	val := &validator.AccessValidator{
		Body: rsp.Body,
	}

	err = json.Unmarshal(rsp.Context, val)
	if err != nil {
		return nil, errors.E(op, err)
	}

	return val, nil
}

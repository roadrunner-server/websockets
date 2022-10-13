package common

import (
	"context"

	"github.com/roadrunner-server/sdk/v3/payload"
	"github.com/roadrunner-server/sdk/v3/pool"
	staticPool "github.com/roadrunner-server/sdk/v3/pool/static_pool"
	"github.com/roadrunner-server/sdk/v3/worker"
	"go.uber.org/zap"
)

type Configurer interface {
	// UnmarshalKey takes a single key and unmarshal it into a Struct.
	UnmarshalKey(name string, out any) error
	// Has checks if config section exists.
	Has(name string) bool
}

type Pool interface {
	// Workers returns worker list associated with the pool.
	Workers() (workers []*worker.Process)

	// Exec payload
	Exec(ctx context.Context, p *payload.Payload) (*payload.Payload, error)

	// Reset kill all workers inside the watcher and replaces with new
	Reset(ctx context.Context) error

	// Destroy all underlying stack (but let them complete the task).
	Destroy(ctx context.Context)
}

// Server creates workers for the application.
type Server interface {
	NewPool(ctx context.Context, cfg *pool.Config, env map[string]string, _ *zap.Logger) (*staticPool.Pool, error)
}

type PubSub interface {
	Publisher
	Subscriber
	Reader
}

type SubReader interface {
	Subscriber
	Reader
}

// Message represents message
type Message interface {
	MarshalBinary() (data []byte, err error)
	Topic() string
	Payload() []byte
}

type Broadcaster interface {
	GetDriver(key string) (SubReader, error)
}

// Subscriber defines the ability to operate as message passing broker.
// BETA interface
type Subscriber interface {
	// Subscribe broker to one or multiple topics.
	Subscribe(connectionID string, topics ...string) error

	// Unsubscribe from one or multiply topics
	Unsubscribe(connectionID string, topics ...string) error

	// Connections returns all connections associated with the particular topic
	Connections(topic string, ret map[string]struct{})

	// Stop used to stop the driver and free up the connection
	Stop()
}

// Publisher publish one or more messages
// BETA interface
type Publisher interface {
	// Publish one or multiple Channel.
	Publish(Message) error

	// PublishAsync publish message and return immediately
	// If error occurred it will be printed into the logger
	PublishAsync(Message)
}

// Reader interface should return next message
type Reader interface {
	Next(ctx context.Context) (Message, error)
}

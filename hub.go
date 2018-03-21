// Package eventhub provides functionality for interacting with Azure Event Hubs.
package eventhub

import (
	"context"
	"os"
	"path"
	"sync"

	"github.com/Azure/azure-event-hubs-go/aad"
	"github.com/Azure/azure-event-hubs-go/auth"
	"github.com/Azure/azure-event-hubs-go/mgmt"
	"github.com/Azure/azure-event-hubs-go/persist"
	"github.com/Azure/azure-event-hubs-go/sas"
	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

const (
	maxUserAgentLen = 128
	rootUserAgent   = "/golang-event-hubs"
)

type (
	// Hub provides the ability to send and receive Event Hub messages
	Hub struct {
		name              string
		namespace         *namespace
		receivers         map[string]*receiver
		sender            *sender
		senderPartitionID *string
		receiverMu        sync.Mutex
		senderMu          sync.Mutex
		offsetPersister   persist.CheckpointPersister
		userAgent         string
	}

	// Handler is the function signature for any receiver of events
	Handler func(ctx context.Context, event *Event) error

	// Sender provides the ability to send a messages
	Sender interface {
		Send(ctx context.Context, event *Event, opts ...SendOption) error
		SendBatch(ctx context.Context, batch *EventBatch, opts ...SendOption) error
	}

	// PartitionedReceiver provides the ability to receive messages from a given partition
	PartitionedReceiver interface {
		Receive(ctx context.Context, partitionID string, handler Handler, opts ...ReceiveOption) (ListenerHandle, error)
	}

	// Manager provides the ability to query management node information about a node
	Manager interface {
		GetRuntimeInformation(context.Context) (*mgmt.HubRuntimeInformation, error)
		GetPartitionInformation(context.Context, string) (*mgmt.HubPartitionRuntimeInformation, error)
	}

	// HubOption provides structure for configuring new Event Hub instances
	HubOption func(h *Hub) error
)

// NewHub creates a new Event Hub client for sending and receiving messages
func NewHub(namespace, name string, tokenProvider auth.TokenProvider, opts ...HubOption) (*Hub, error) {
	ns := newNamespace(namespace, tokenProvider, azure.PublicCloud)
	h := &Hub{
		name:            name,
		namespace:       ns,
		offsetPersister: persist.NewMemoryPersister(),
		userAgent:       rootUserAgent,
		receivers:       make(map[string]*receiver),
	}

	for _, opt := range opts {
		err := opt(h)
		if err != nil {
			return nil, err
		}
	}

	return h, nil
}

// NewHubWithNamespaceNameAndEnvironment creates a new Event Hub client for sending and receiving messages from
// environment variables with supplied namespace and name
func NewHubWithNamespaceNameAndEnvironment(namespace, name string, opts ...HubOption) (*Hub, error) {
	var provider auth.TokenProvider
	aadProvider, aadErr := aad.NewJWTProvider(aad.JWTProviderWithEnvironmentVars())
	sasProvider, sasErr := sas.NewTokenProvider(sas.TokenProviderWithEnvironmentVars())

	if aadErr != nil && sasErr != nil {
		// both failed
		log.Debug("both token providers failed")
		return nil, errors.Errorf("neither Azure Active Directory nor SAS token provider could be built - AAD error: %v, SAS error: %v", aadErr, sasErr)
	}

	if aadProvider != nil {
		log.Debug("using AAD provider")
		provider = aadProvider
	} else {
		log.Debug("using SAS provider")
		provider = sasProvider
	}

	h, err := NewHub(namespace, name, provider, opts...)
	if err != nil {
		return nil, err
	}

	return h, nil
}

// NewHubFromEnvironment creates a new Event Hub client for sending and receiving messages from environment variables
func NewHubFromEnvironment(opts ...HubOption) (*Hub, error) {
	const envErrMsg = "environment var %s must not be empty"
	var namespace, name string

	if namespace = os.Getenv("EVENTHUB_NAMESPACE"); namespace == "" {
		return nil, errors.Errorf(envErrMsg, "EVENTHUB_NAMESPACE")
	}

	if name = os.Getenv("EVENTHUB_NAME"); name == "" {
		return nil, errors.Errorf(envErrMsg, "EVENTHUB_NAME")
	}

	return NewHubWithNamespaceNameAndEnvironment(namespace, name, opts...)
}

// GetRuntimeInformation fetches runtime information from the Event Hub management node
func (h *Hub) GetRuntimeInformation(ctx context.Context) (*mgmt.HubRuntimeInformation, error) {
	client := mgmt.NewClient(h.namespace.name, h.name, h.namespace.tokenProvider, h.namespace.environment)
	conn, err := h.namespace.connection()
	if err != nil {
		return nil, err
	}
	info, err := client.GetHubRuntimeInformation(ctx, conn)
	if err != nil {
		return nil, err
	}
	return info, nil
}

// GetPartitionInformation fetches runtime information about a specific partition from the Event Hub management node
func (h *Hub) GetPartitionInformation(ctx context.Context, partitionID string) (*mgmt.HubPartitionRuntimeInformation, error) {
	client := mgmt.NewClient(h.namespace.name, h.name, h.namespace.tokenProvider, h.namespace.environment)
	conn, err := h.namespace.connection()
	if err != nil {
		return nil, err
	}
	info, err := client.GetHubPartitionRuntimeInformation(ctx, conn, partitionID)
	if err != nil {
		return nil, err
	}
	return info, nil
}

// Close drains and closes all of the existing senders, receivers and connections
func (h *Hub) Close() error {
	var lastErr error
	for _, r := range h.receivers {
		if err := r.Close(); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// Receive subscribes for messages sent to the provided entityPath.
func (h *Hub) Receive(ctx context.Context, partitionID string, handler Handler, opts ...ReceiveOption) (ListenerHandle, error) {
	h.receiverMu.Lock()
	defer h.receiverMu.Unlock()

	receiver, err := h.newReceiver(ctx, partitionID, opts...)
	if err != nil {
		return nil, err
	}

	if r, ok := h.receivers[receiver.getIdentifier()]; ok {
		if err := r.Close(); err != nil {
			log.Error(err)
		}
	}

	h.receivers[receiver.getIdentifier()] = receiver
	listenerContext := receiver.Listen(handler)

	return listenerContext, nil
}

// Send sends an event to the Event Hub
func (h *Hub) Send(ctx context.Context, event *Event, opts ...SendOption) error {
	sender, err := h.getSender(ctx)
	if err != nil {
		return err
	}

	return sender.Send(ctx, event.toMsg(), opts...)
}

// SendBatch sends an EventBatch to the Event Hub
func (h *Hub) SendBatch(ctx context.Context, batch *EventBatch, opts ...SendOption) error {
	sender, err := h.getSender(ctx)
	if err != nil {
		return err
	}
	msg, err := batch.toMsg()
	if err != nil {
		return err
	}
	return sender.Send(ctx, msg, opts...)
}

// HubWithPartitionedSender configures the Hub instance to send to a specific event Hub partition
func HubWithPartitionedSender(partitionID string) HubOption {
	return func(h *Hub) error {
		h.senderPartitionID = &partitionID
		return nil
	}
}

// HubWithOffsetPersistence configures the Hub instance to read and write offsets so that if a Hub is interrupted, it
// can resume after the last consumed event.
func HubWithOffsetPersistence(offsetPersister persist.CheckpointPersister) HubOption {
	return func(h *Hub) error {
		h.offsetPersister = offsetPersister
		return nil
	}
}

// HubWithUserAgent configures the Hub to append the given string to the user agent sent to the server
//
// This option can be specified multiple times to add additional segments.
//
// Max user agent length is specified by the const maxUserAgentLen.
func HubWithUserAgent(userAgent string) HubOption {
	return func(h *Hub) error {
		return h.appendAgent(userAgent)
	}
}

// HubWithEnvironment configures the Hub to use the specified environment.
//
// By default, the Hub instance will use Azure US Public cloud environment
func HubWithEnvironment(env azure.Environment) HubOption {
	return func(h *Hub) error {
		h.namespace.environment = env
		return nil
	}
}

func (h *Hub) appendAgent(userAgent string) error {
	ua := path.Join(h.userAgent, userAgent)
	if len(ua) > maxUserAgentLen {
		return errors.Errorf("user agent string has surpassed the max length of %d", maxUserAgentLen)
	}
	h.userAgent = ua
	return nil
}

func (h *Hub) getSender(ctx context.Context) (*sender, error) {
	h.senderMu.Lock()
	defer h.senderMu.Unlock()

	if h.sender == nil {
		s, err := h.newSender(ctx)
		if err != nil {
			return nil, err
		}
		h.sender = s
	}
	// add recover logic here
	return h.sender, nil
}

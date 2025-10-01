// Copyright AGNTCY Contributors (https://github.com/agntcy)
// SPDX-License-Identifier: Apache-2.0

package pubsub

import (
	"context"
	"fmt"
	"time"

	"github.com/agntcy/dir/server/types/labels"
	"github.com/agntcy/dir/utils/logging"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/host"
)

var logger = logging.Logger("routing/pubsub")

// Manager handles GossipSub operations for label announcements.
// It provides efficient label propagation across the network without
// requiring peers to pull entire records.
//
// Architecture:
//   - Publisher: Announces labels when storing records
//   - Subscriber: Receives and caches labels from remote peers
//   - Integration: Works alongside DHT for resilient discovery
//
// Performance:
//   - Propagation: ~5-20ms (vs DHT's ~100-500ms)
//   - Bandwidth: ~100B per announcement (vs KB-MB for full record pull)
//   - Reach: ALL subscribed peers (vs DHT's k-closest peers)
type Manager struct {
	ctx         context.Context //nolint:containedctx // Needed for long-running message handler goroutine
	host        host.Host
	pubsub      *pubsub.PubSub
	topic       *pubsub.Topic
	sub         *pubsub.Subscription
	localPeerID string
	topicName   string // Topic name (protocol constant)

	// Callback invoked when label announcement is received
	onLabelAnnouncement func(context.Context, *LabelAnnouncement)
}

// New creates a new GossipSub manager for label announcements.
// This initializes the GossipSub router, joins the labels topic, and
// starts the message handler goroutine.
//
// Protocol parameters (TopicLabels, MaxMessageSize) are defined in constants.go
// and are intentionally NOT configurable to ensure network-wide compatibility.
//
// Parameters:
//   - ctx: Context for lifecycle management
//   - h: libp2p host for network operations
//
// Returns:
//   - *Manager: Initialized manager ready for use
//   - error: If GossipSub setup fails
func New(ctx context.Context, h host.Host) (*Manager, error) {
	// Create GossipSub with protocol-defined settings
	ps, err := pubsub.NewGossipSub(
		ctx,
		h,
		// Enable peer exchange for better peer discovery
		pubsub.WithPeerExchange(true),
		// Limit message size to protocol-defined maximum
		pubsub.WithMaxMessageSize(MaxMessageSize),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create gossipsub: %w", err)
	}

	// Join the protocol-defined topic
	topic, err := ps.Join(TopicLabels)
	if err != nil {
		return nil, fmt.Errorf("failed to join labels topic %q: %w", TopicLabels, err)
	}

	// Subscribe to receive label announcements
	sub, err := topic.Subscribe()
	if err != nil {
		return nil, fmt.Errorf("failed to subscribe to labels topic %q: %w", TopicLabels, err)
	}

	manager := &Manager{
		ctx:         ctx,
		host:        h,
		pubsub:      ps,
		topic:       topic,
		sub:         sub,
		localPeerID: h.ID().String(),
		topicName:   TopicLabels,
	}

	// Start message handler goroutine
	go manager.handleMessages()

	logger.Info("GossipSub manager initialized",
		"topic", TopicLabels,
		"maxMessageSize", MaxMessageSize,
		"peerID", manager.localPeerID)

	return manager, nil
}

// PublishLabels announces labels for a record to the network.
// This is called when a record is stored locally and should be
// discoverable by remote peers.
//
// Flow:
//  1. Convert labels.Label to wire format ([]string)
//  2. Create and validate LabelAnnouncement
//  3. Publish to GossipSub topic
//  4. GossipSub mesh propagates to all subscribed peers
//
// Parameters:
//   - ctx: Context for operation timeout/cancellation
//   - cid: Content ID of the record
//   - labelList: List of labels extracted from the record
//
// Returns:
//   - error: If validation or publishing fails
//
// Note: This is non-blocking. GossipSub handles propagation asynchronously.
func (m *Manager) PublishLabels(ctx context.Context, cid string, labelList []labels.Label) error {
	// Convert labels.Label to strings for wire format
	labelStrings := make([]string, len(labelList))
	for i, label := range labelList {
		labelStrings[i] = label.String()
	}

	// Create announcement with current timestamp
	announcement := &LabelAnnouncement{
		CID:       cid,
		PeerID:    m.localPeerID,
		Labels:    labelStrings,
		Timestamp: time.Now(),
	}

	// Validate before publishing to catch issues early
	if err := announcement.Validate(); err != nil {
		return fmt.Errorf("invalid announcement: %w", err)
	}

	// Serialize to JSON
	data, err := announcement.Marshal()
	if err != nil {
		return fmt.Errorf("failed to marshal announcement: %w", err)
	}

	// Publish to GossipSub topic
	if err := m.topic.Publish(ctx, data); err != nil {
		return fmt.Errorf("failed to publish announcement: %w", err)
	}

	logger.Info("Published label announcement",
		"cid", cid,
		"labels", len(labelList),
		"topicPeers", len(m.topic.ListPeers()),
		"size", len(data))

	return nil
}

// SetOnLabelAnnouncement sets the callback for received label announcements.
// This callback is invoked for each valid announcement received from remote peers.
//
// The callback should:
//   - Convert wire format ([]string) to labels.Label
//   - Build enhanced keys using BuildEnhancedLabelKey()
//   - Store labels.LabelMetadata in datastore
//
// Example:
//
//	manager.SetOnLabelAnnouncement(func(ctx context.Context, ann *LabelAnnouncement) {
//	    for _, labelStr := range ann.Labels {
//	        label := labels.Label(labelStr)
//	        key := BuildEnhancedLabelKey(label, ann.CID, ann.PeerID)
//	        // ... store in datastore ...
//	    }
//	})
func (m *Manager) SetOnLabelAnnouncement(fn func(context.Context, *LabelAnnouncement)) {
	m.onLabelAnnouncement = fn
}

// handleMessages is the main message processing loop.
// It runs in a goroutine and processes all incoming label announcements.
//
// Flow:
//  1. Wait for next message from subscription
//  2. Skip own messages (already cached locally)
//  3. Unmarshal and validate announcement
//  4. Invoke callback for processing
//
// Error handling:
//   - Context cancellation: Normal shutdown, exit loop
//   - Invalid messages: Log warning, continue processing
//   - Unmarshal errors: Log warning, continue processing
//
// This goroutine runs for the lifetime of the Manager.
func (m *Manager) handleMessages() {
	for {
		msg, err := m.sub.Next(m.ctx)
		if err != nil {
			// Check if context was cancelled (normal shutdown)
			if m.ctx.Err() != nil {
				logger.Debug("Message handler stopping", "reason", "context_cancelled")

				return
			}

			// Log error but continue processing
			logger.Error("Error reading from labels topic", "error", err)

			continue
		}

		// Skip our own messages (we already cached labels locally)
		if msg.ReceivedFrom == m.host.ID() {
			continue
		}

		// Parse and validate announcement
		announcement, err := UnmarshalLabelAnnouncement(msg.Data)
		if err != nil {
			logger.Warn("Received invalid label announcement",
				"from", msg.ReceivedFrom,
				"error", err,
				"size", len(msg.Data))

			continue
		}

		logger.Debug("Received label announcement",
			"from", msg.ReceivedFrom,
			"cid", announcement.CID,
			"peer", announcement.PeerID,
			"labels", len(announcement.Labels))

		// Invoke callback for processing
		if m.onLabelAnnouncement != nil {
			// Use context from Manager, not from message
			m.onLabelAnnouncement(m.ctx, announcement)
		}
	}
}

// GetTopicPeers returns the list of peers subscribed to the labels topic.
// This is useful for monitoring network connectivity and debugging.
//
// Returns:
//   - []string: List of peer IDs (as strings)
func (m *Manager) GetTopicPeers() []string {
	peers := m.topic.ListPeers()
	peerIDs := make([]string, len(peers))

	for i, p := range peers {
		peerIDs[i] = p.String()
	}

	return peerIDs
}

// Close stops the GossipSub manager and releases resources.
// This should be called during shutdown to clean up gracefully.
//
// Flow:
//  1. Cancel subscription (stops handleMessages goroutine)
//  2. Leave topic
//  3. Release resources
//
// Returns:
//   - error: If cleanup fails (rare)
func (m *Manager) Close() error {
	m.sub.Cancel()

	if err := m.topic.Close(); err != nil {
		return fmt.Errorf("failed to close gossipsub topic: %w", err)
	}

	return nil
}

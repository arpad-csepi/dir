// Copyright AGNTCY Contributors (https://github.com/agntcy)
// SPDX-License-Identifier: Apache-2.0

package pubsub

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// LabelAnnouncement is the wire format for label announcements via GossipSub.
// This is a minimal structure optimized for network efficiency.
//
// Protocol parameters: See constants.go for TopicLabels, MaxMessageSize, etc.
// These are intentionally NOT configurable to ensure network-wide compatibility.
//
// Conversion to storage format:
//   - Wire: LabelAnnouncement with []string labels
//   - Storage: Enhanced keys (/skills/AI/CID/PeerID) with labels.LabelMetadata
//
// Example:
//
//	{
//	  "cid": "bafybeigdyrzt5sfp7udm7hu76uh7y26nf3efuylqabf3oclgtqy55fbzdi",
//	  "peer_id": "12D3KooWD3bfmNbuuuT5Zch8fj9Cg9dQR2FpGm7JzCfCzPWZnxLn",
//	  "labels": ["/skills/AI/ML", "/domains/research", "/modules/tensorflow"],
//	  "timestamp": "2025-10-01T10:00:00Z"
//	}
type LabelAnnouncement struct {
	// CID is the content identifier of the record.
	// This uniquely identifies the record being announced.
	CID string `json:"cid"`

	// PeerID is the libp2p peer ID of the node that has this record.
	// This identifies which peer can provide the content.
	PeerID string `json:"peer_id"`

	// Labels is the list of label strings extracted from the record.
	// Format: namespace-prefixed paths (e.g., "/skills/AI/ML")
	// These will be converted to labels.Label type upon receipt.
	Labels []string `json:"labels"`

	// Timestamp is when this announcement was created.
	// This becomes the labels.LabelMetadata.Timestamp field.
	Timestamp time.Time `json:"timestamp"`
}

// Validate checks if the announcement is well-formed and safe to process.
// This prevents malformed or malicious announcements from being processed.
func (a *LabelAnnouncement) Validate() error {
	if a.CID == "" {
		return errors.New("missing CID")
	}

	if a.PeerID == "" {
		return errors.New("missing PeerID")
	}

	if len(a.Labels) == 0 {
		return errors.New("no labels provided")
	}

	if len(a.Labels) > MaxLabelsPerAnnouncement {
		return errors.New("too many labels")
	}

	if a.Timestamp.IsZero() {
		return errors.New("missing timestamp")
	}

	return nil
}

// Marshal serializes the announcement to JSON for network transmission.
func (a *LabelAnnouncement) Marshal() ([]byte, error) {
	data, err := json.Marshal(a)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal label announcement: %w", err)
	}

	// Validate size to prevent oversized messages
	if len(data) > MaxMessageSize {
		return nil, errors.New("announcement exceeds maximum size")
	}

	return data, nil
}

// UnmarshalLabelAnnouncement deserializes and validates a label announcement.
// This is the entry point for processing received GossipSub messages.
func UnmarshalLabelAnnouncement(data []byte) (*LabelAnnouncement, error) {
	// Check size before unmarshaling to prevent resource exhaustion
	if len(data) > MaxMessageSize {
		return nil, errors.New("announcement exceeds maximum size")
	}

	var ann LabelAnnouncement
	if err := json.Unmarshal(data, &ann); err != nil {
		return nil, fmt.Errorf("failed to unmarshal label announcement: %w", err)
	}

	// Validate after unmarshaling to ensure well-formed data
	if err := ann.Validate(); err != nil {
		return nil, err
	}

	return &ann, nil
}

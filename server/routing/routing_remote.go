// Copyright AGNTCY Contributors (https://github.com/agntcy)
// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "github.com/agntcy/dir/api/core/v1"
	routingv1 "github.com/agntcy/dir/api/routing/v1"
	"github.com/agntcy/dir/server/routing/internal/p2p"
	"github.com/agntcy/dir/server/routing/pubsub"
	"github.com/agntcy/dir/server/routing/rpc"
	validators "github.com/agntcy/dir/server/routing/validators"
	"github.com/agntcy/dir/server/types"
	"github.com/agntcy/dir/server/types/labels"
	"github.com/agntcy/dir/utils/logging"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	"github.com/ipfs/go-datastore/query"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p-kad-dht/providers"
	record "github.com/libp2p/go-libp2p-record"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/protocol"
	ma "github.com/multiformats/go-multiaddr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var remoteLogger = logging.Logger("routing/remote")

// NamespaceEntry contains processed namespace query data.
// This is used by namespace iteration functions for routing operations.
type NamespaceEntry struct {
	Namespace string
	Key       string
	Value     []byte
}

// QueryAllNamespaces queries all supported label namespaces and returns processed entries.
// This centralizes namespace iteration and datastore querying, eliminating code duplication
// between local and remote routing operations. All resource management is handled internally.
func QueryAllNamespaces(ctx context.Context, dstore types.Datastore) ([]NamespaceEntry, error) {
	var entries []NamespaceEntry

	// Query all label namespaces
	namespaces := []string{
		labels.LabelTypeSkill.Prefix(),
		labels.LabelTypeDomain.Prefix(),
		labels.LabelTypeModule.Prefix(),
		labels.LabelTypeLocator.Prefix(),
	}

	for _, namespace := range namespaces {
		// Check for context cancellation
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("namespace query canceled: %w", ctx.Err())
		default:
		}

		results, err := dstore.Query(ctx, query.Query{Prefix: namespace})
		if err != nil {
			remoteLogger.Warn("Failed to query namespace", "namespace", namespace, "error", err)

			continue
		}

		// Process results and handle cleanup
		func() {
			defer results.Close()

			for result := range results.Next() {
				if result.Error != nil {
					continue
				}

				entries = append(entries, NamespaceEntry{
					Namespace: namespace,
					Key:       result.Key,
					Value:     result.Value,
				})
			}
		}()
	}

	return entries, nil
}

// routeRemote handles routing across the network with hybrid label discovery.
// It uses both GossipSub (efficient, wide propagation) and DHT+Pull (fallback).
type routeRemote struct {
	storeAPI       types.StoreAPI
	server         *p2p.Server
	service        *rpc.Service
	notifyCh       chan *handlerSync
	dstore         types.Datastore
	cleanupManager *CleanupManager
	pubsubManager  *pubsub.Manager // GossipSub manager for label announcements (nil if disabled)
}

func newRemote(ctx context.Context,
	storeAPI types.StoreAPI,
	dstore types.Datastore,
	opts types.APIOptions,
) (*routeRemote, error) {
	// Create routing
	routeAPI := &routeRemote{
		storeAPI: storeAPI,
		notifyCh: make(chan *handlerSync, NotificationChannelSize),
		dstore:   dstore,
	}

	refreshInterval := RefreshInterval
	if opts.Config().Routing.RefreshInterval > 0 {
		refreshInterval = opts.Config().Routing.RefreshInterval
	}

	server, err := p2p.New(ctx,
		p2p.WithListenAddress(opts.Config().Routing.ListenAddress),
		p2p.WithDirectoryAPIAddress(opts.Config().Routing.DirectoryAPIAddress),
		p2p.WithBootstrapAddrs(opts.Config().Routing.BootstrapPeers),
		p2p.WithRefreshInterval(refreshInterval),
		p2p.WithRandevous(ProtocolRendezvous), // enable libp2p auto-discovery
		p2p.WithIdentityKeyPath(opts.Config().Routing.KeyPath),
		p2p.WithCustomDHTOpts(
			func(h host.Host) ([]dht.Option, error) {
				providerMgr, err := providers.NewProviderManager(h.ID(), h.Peerstore(), dstore)
				if err != nil {
					return nil, fmt.Errorf("failed to create provider manager: %w", err)
				}

				labelValidators := validators.CreateLabelValidators()
				validator := record.NamespacedValidator{
					labels.LabelTypeSkill.String():  labelValidators[labels.LabelTypeSkill.String()],
					labels.LabelTypeDomain.String(): labelValidators[labels.LabelTypeDomain.String()],
					labels.LabelTypeModule.String(): labelValidators[labels.LabelTypeModule.String()],
				}

				return []dht.Option{
					dht.Datastore(dstore),                           // custom DHT datastore
					dht.ProtocolPrefix(protocol.ID(ProtocolPrefix)), // custom DHT protocol prefix
					dht.Validator(validator),                        // custom validators for label namespaces
					dht.MaxRecordAge(RecordTTL),                     // set consistent TTL for all DHT records
					dht.Mode(dht.ModeServer),
					dht.ProviderStore(&handler{
						ProviderManager: providerMgr,
						hostID:          h.ID().String(),
						notifyCh:        routeAPI.notifyCh,
					}),
				}, nil
			},
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create p2p: %w", err)
	}

	routeAPI.server = server

	rpcService, err := rpc.New(server.Host(), storeAPI)
	if err != nil {
		defer server.Close()

		return nil, fmt.Errorf("failed to create RPC service: %w", err)
	}

	routeAPI.service = rpcService

	// Initialize GossipSub manager if enabled
	// Protocol parameters (topic, message size) are defined in pubsub.constants
	// and are NOT configurable to ensure network-wide compatibility
	if opts.Config().Routing.GossipSub.Enabled {
		pubsubManager, err := pubsub.New(ctx, server.Host())
		if err != nil {
			defer server.Close()

			return nil, fmt.Errorf("failed to create pubsub manager: %w", err)
		}

		routeAPI.pubsubManager = pubsubManager

		// Set callback for received label announcements
		pubsubManager.SetOnLabelAnnouncement(routeAPI.handleLabelAnnouncement)

		remoteLogger.Info("GossipSub label announcements enabled")
	} else {
		remoteLogger.Info("GossipSub disabled, using DHT+Pull fallback only")
	}

	// Pass publishToNetwork as callback to avoid circular dependency
	// The method value captures routeAPI's state (server, pubsubManager)
	routeAPI.cleanupManager = NewCleanupManager(dstore, storeAPI, server, routeAPI.publishToNetwork)

	go routeAPI.handleNotify(ctx)

	go routeAPI.cleanupManager.StartLabelRepublishTask(ctx)

	routeAPI.cleanupManager.StartRemoteLabelCleanupTask(ctx)

	return routeAPI, nil
}

// publishToNetwork is the core publishing logic shared by Publish and republishing tasks.
// It announces a record's CID and labels to the network via DHT and GossipSub.
//
// This method captures receiver state (server, pubsubManager) and can be passed
// as a callback to CleanupManager without creating circular dependencies.
//
// Flow:
//  1. Parse and validate CID
//  2. Announce CID to DHT (critical - returns error if fails)
//  3. Publish labels via GossipSub (best-effort - logs warning if fails)
//
// Parameters:
//   - ctx: Operation context
//   - cidStr: Content ID as string
//   - record: Record to extract labels from
//
// Returns:
//   - error: If critical operations fail (CID parsing, DHT announcement)
func (r *routeRemote) publishToNetwork(ctx context.Context, cidStr string, record *corev1.Record) error {
	// Parse and validate CID
	decodedCID, err := cid.Decode(cidStr)
	if err != nil {
		return fmt.Errorf("failed to parse CID: %w", err)
	}

	// 1. Announce CID to DHT network (content discovery)
	err = r.server.DHT().Provide(ctx, decodedCID, true)
	if err != nil {
		return fmt.Errorf("failed to announce CID to DHT: %w", err)
	}

	// 2. Publish labels via GossipSub (if enabled)
	// This provides efficient label propagation to ALL subscribed peers
	if r.pubsubManager != nil {
		labelList := GetLabelsFromRecord(record)
		if len(labelList) > 0 {
			if err := r.pubsubManager.PublishLabels(ctx, cidStr, labelList); err != nil {
				// Log warning but don't fail - DHT announcement already succeeded
				// Remote peers can still discover via DHT+Pull fallback
				remoteLogger.Warn("Failed to publish labels via GossipSub",
					"cid", cidStr,
					"error", err,
					"fallback", "DHT+Pull will handle discovery")
			} else {
				remoteLogger.Debug("Successfully published labels via GossipSub",
					"cid", cidStr,
					"labels", len(labelList),
					"topicPeers", len(r.pubsubManager.GetTopicPeers()))
			}
		}
	}

	return nil
}

func (r *routeRemote) Publish(ctx context.Context, ref *corev1.RecordRef, record *corev1.Record) error {
	remoteLogger.Debug("Publishing record to network", "ref", ref)

	// Use shared publishing logic
	if err := r.publishToNetwork(ctx, ref.GetCid(), record); err != nil {
		return status.Errorf(codes.Internal, "%v", err)
	}

	remoteLogger.Debug("Successfully announced record to network",
		"cid", ref.GetCid(),
		"dhtPeers", r.server.DHT().RoutingTable().Size(),
		"gossipSubEnabled", r.pubsubManager != nil)

	return nil
}

// Search queries remote records using cached labels with OR logic and minimum threshold.
// Records are returned if they match at least minMatchScore queries (OR relationship).
func (r *routeRemote) Search(ctx context.Context, req *routingv1.SearchRequest) (<-chan *routingv1.SearchResponse, error) {
	remoteLogger.Debug("Called remote routing's Search method", "req", req)

	// Deduplicate queries to ensure consistent scoring regardless of client behavior
	originalQueries := req.GetQueries()
	deduplicatedQueries := deduplicateQueries(originalQueries)

	if len(originalQueries) != len(deduplicatedQueries) {
		remoteLogger.Info("Deduplicated search queries for consistent scoring",
			"originalCount", len(originalQueries), "deduplicatedCount", len(deduplicatedQueries))
	}

	// Enforce minimum match score for proto compliance
	// Proto: "If not set, it will return records that match at least one query"
	minMatchScore := req.GetMinMatchScore()
	if minMatchScore < DefaultMinMatchScore {
		minMatchScore = DefaultMinMatchScore
		remoteLogger.Debug("Applied minimum match score for production safety", "original", req.GetMinMatchScore(), "applied", minMatchScore)
	}

	outCh := make(chan *routingv1.SearchResponse)

	go func() {
		defer close(outCh)
		r.searchRemoteRecords(ctx, deduplicatedQueries, req.GetLimit(), minMatchScore, outCh)
	}()

	return outCh, nil
}

// searchRemoteRecords searches for remote records using cached labels with OR logic.
// Records are returned if they match at least minMatchScore queries.
//
//nolint:gocognit // Core search algorithm requires complex logic for namespace iteration, filtering, and scoring
func (r *routeRemote) searchRemoteRecords(ctx context.Context, queries []*routingv1.RecordQuery, limit uint32, minMatchScore uint32, outCh chan<- *routingv1.SearchResponse) {
	localPeerID := r.server.Host().ID().String()
	processedCIDs := make(map[string]bool) // Avoid duplicates
	processedCount := 0
	limitInt := int(limit)

	remoteLogger.Debug("Starting remote search with OR logic and minimum threshold", "queries", len(queries), "minMatchScore", minMatchScore, "localPeerID", localPeerID)

	// Query all namespaces to find remote records
	entries, err := QueryAllNamespaces(ctx, r.dstore)
	if err != nil {
		remoteLogger.Error("Failed to get namespace entries for search", "error", err)

		return
	}

	for _, entry := range entries {
		if limitInt > 0 && processedCount >= limitInt {
			break
		}

		_, keyCID, keyPeerID, err := ParseEnhancedLabelKey(entry.Key)
		if err != nil {
			remoteLogger.Warn("Failed to parse enhanced label key", "key", entry.Key, "error", err)

			continue
		}

		// Filter for remote records only (exclude local records)
		if keyPeerID == localPeerID {
			continue // Skip local records
		}

		// Avoid duplicate CIDs (same record might have multiple matching labels)
		if processedCIDs[keyCID] {
			continue
		}

		// Calculate match score using OR logic (how many queries match this record)
		matchQueries, score := r.calculateMatchScore(ctx, keyCID, queries, keyPeerID)

		remoteLogger.Debug("Calculated match score for remote record", "cid", keyCID, "score", score, "minMatchScore", minMatchScore, "matchingQueries", len(matchQueries))

		// Apply minimum match score filter (record included if score ≥ threshold)
		if score >= minMatchScore {
			peer := r.createPeerInfo(ctx, keyPeerID)

			outCh <- &routingv1.SearchResponse{
				RecordRef:    &corev1.RecordRef{Cid: keyCID},
				Peer:         peer,
				MatchQueries: matchQueries,
				MatchScore:   score,
			}

			processedCIDs[keyCID] = true
			processedCount++

			remoteLogger.Debug("Record meets minimum threshold, including in results", "cid", keyCID, "score", score)

			if limitInt > 0 && processedCount >= limitInt {
				break
			}
		} else {
			remoteLogger.Debug("Record does not meet minimum threshold, excluding from results", "cid", keyCID, "score", score, "minMatchScore", minMatchScore)
		}
	}

	remoteLogger.Debug("Completed Search operation", "processed", processedCount, "queries", len(queries))
}

// calculateMatchScore calculates how many queries match a remote record (OR logic).
// Returns the matching queries and the match score for minimum threshold filtering.
func (r *routeRemote) calculateMatchScore(ctx context.Context, cid string, queries []*routingv1.RecordQuery, peerID string) ([]*routingv1.RecordQuery, uint32) {
	if len(queries) == 0 {
		return nil, 0
	}

	labels := r.getRemoteRecordLabels(ctx, cid, peerID)
	if len(labels) == 0 {
		return nil, 0
	}

	var matchingQueries []*routingv1.RecordQuery

	// Check each query against all labels - any match counts toward the score (OR logic)
	for _, query := range queries {
		if QueryMatchesLabels(query, labels) {
			matchingQueries = append(matchingQueries, query)
		}
	}

	score := safeIntToUint32(len(matchingQueries))

	remoteLogger.Debug("OR logic match score calculated", "cid", cid, "total_queries", len(queries), "matching_queries", len(matchingQueries), "score", score)

	return matchingQueries, score
}

// getRemoteRecordLabels gets labels for a remote record by finding all enhanced keys for this CID/PeerID.
func (r *routeRemote) getRemoteRecordLabels(ctx context.Context, cid, peerID string) []labels.Label {
	var labelList []labels.Label

	entries, err := QueryAllNamespaces(ctx, r.dstore)
	if err != nil {
		remoteLogger.Error("Failed to get namespace entries for labels", "error", err)

		return nil
	}

	for _, entry := range entries {
		label, keyCID, keyPeerID, err := ParseEnhancedLabelKey(entry.Key)
		if err != nil {
			continue
		}

		if keyCID == cid && keyPeerID == peerID {
			labelList = append(labelList, label)
		}
	}

	return labelList
}

// createPeerInfo creates a Peer message from a PeerID string.
func (r *routeRemote) createPeerInfo(ctx context.Context, peerID string) *routingv1.Peer {
	dirAPIAddr := r.getDirectoryAPIAddress(ctx, peerID)

	return &routingv1.Peer{
		Id:    peerID,
		Addrs: []string{dirAPIAddr},
	}
}

func (r *routeRemote) getDirectoryAPIAddress(ctx context.Context, peerID string) string {
	key := datastore.NewKey("peer_addrs/" + peerID)

	addresses, err := r.dstore.Get(ctx, key)
	if err != nil {
		remoteLogger.Error("Failed to get peer addresses", "error", err)

		return ""
	}

	// Unmarshal the addresses
	var multiaddrs []ma.Multiaddr
	if err := json.Unmarshal(addresses, &multiaddrs); err != nil {
		remoteLogger.Error("Failed to unmarshal peer addresses", "error", err)

		return ""
	}

	remoteLogger.Debug("Unmarshalled peer addresses", "peerID", peerID, "multiaddrs", multiaddrs)

	for _, addr := range multiaddrs {
		protocols := addr.Protocols()
		for _, protocol := range protocols {
			if protocol.Code == p2p.DirProtocolCode { // dir protocol
				value, err := addr.ValueForProtocol(p2p.DirProtocolCode)
				if err != nil {
					remoteLogger.Error("Failed to get dir protocol value", "peerID", peerID, "addr", addr.String(), "error", err)
				} else {
					return value
				}
			}
		}
	}

	return ""
}

func (r *routeRemote) handleNotify(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Process DHT provider notifications and handle pull-based label discovery
	for {
		select {
		case <-ctx.Done():
			return
		case notif := <-r.notifyCh:
			// All announcements are now CID provider announcements
			// Labels are discovered via pull-based mechanism
			r.handleCIDProviderNotification(ctx, notif)
		}
	}
}

// handleCIDProviderNotification implements fallback label discovery via DHT+Pull.
// This is the secondary mechanism when GossipSub labels haven't arrived yet.
//
// Flow:
//  1. Check if labels already cached (from GossipSub) → Update timestamps, skip pull
//  2. If not cached → FALLBACK: Pull record, extract labels, cache
//
// Timing scenarios:
//   - 90% case: GossipSub arrives first (~15ms) → This function skips pull (efficient!)
//   - 10% case: DHT arrives first (~80ms) → This function pulls (fallback)
//
// This ensures labels are always cached regardless of network race conditions.
func (r *routeRemote) handleCIDProviderNotification(ctx context.Context, notif *handlerSync) {
	peerIDStr := notif.Peer.ID.String()

	if peerIDStr == r.server.Host().ID().String() {
		remoteLogger.Debug("Ignoring self announcement", "cid", notif.Ref.GetCid())

		return
	}

	// Add the peer's addresses to our peerstore so we can contact them later
	//nolint:nestif
	if len(notif.Peer.Addrs) > 0 {
		key := datastore.NewKey("peer_addrs/" + peerIDStr)
		if _, err := r.dstore.Get(ctx, key); err != nil {
			addresses, err := json.Marshal(notif.Peer.Addrs)
			if err != nil {
				remoteLogger.Error("Failed to marshal peer addresses", "error", err)
			}

			err = r.dstore.Put(ctx, key, addresses)
			if err != nil {
				remoteLogger.Error("Failed to store peer addresses", "error", err)
			}

			remoteLogger.Debug("Stored peer addresses", "peerID", peerIDStr, "addresses", addresses)
		}
	}

	// Check if we already have labels cached (from GossipSub announcement)
	if r.hasRemoteRecordCached(ctx, notif.Ref.GetCid(), peerIDStr) {
		// Labels already cached via GossipSub or previous pull
		// Just update lastSeen timestamps for freshness
		remoteLogger.Debug("Labels already cached (likely from GossipSub), updating lastSeen",
			"cid", notif.Ref.GetCid(),
			"peer", peerIDStr,
			"source", "gossipsub_or_previous_pull")

		r.updateRemoteRecordLastSeen(ctx, notif.Ref.GetCid(), peerIDStr)

		return
	}

	// FALLBACK: Labels not cached yet, need to pull record
	// This happens when:
	// - GossipSub message hasn't arrived yet (race condition)
	// - GossipSub is disabled
	// - GossipSub message was lost
	// - Peer doesn't support GossipSub
	remoteLogger.Debug("No cached labels, falling back to pull-based discovery",
		"cid", notif.Ref.GetCid(),
		"peer", peerIDStr,
		"reason", "gossipsub_not_received")

	record, err := r.service.Pull(ctx, notif.Peer.ID, notif.Ref)
	if err != nil {
		remoteLogger.Error("Failed to pull remote content for label caching",
			"cid", notif.Ref.GetCid(),
			"peer", peerIDStr,
			"error", err)

		return
	}

	labelList := GetLabelsFromRecord(record)
	if len(labelList) == 0 {
		remoteLogger.Warn("No labels found in remote record",
			"cid", notif.Ref.GetCid(),
			"peer", peerIDStr)

		return
	}

	now := time.Now()
	cachedCount := 0

	for _, label := range labelList {
		enhancedKey := BuildEnhancedLabelKey(label, notif.Ref.GetCid(), peerIDStr)

		metadata := &labels.LabelMetadata{
			Timestamp: now,
			LastSeen:  now,
		}

		metadataBytes, err := json.Marshal(metadata)
		if err != nil {
			remoteLogger.Warn("Failed to marshal label metadata",
				"enhanced_key", enhancedKey,
				"error", err)

			continue
		}

		err = r.dstore.Put(ctx, datastore.NewKey(enhancedKey), metadataBytes)
		if err != nil {
			remoteLogger.Warn("Failed to cache remote label",
				"enhanced_key", enhancedKey,
				"error", err)
		} else {
			cachedCount++
		}
	}

	remoteLogger.Info("Successfully cached labels via DHT+Pull fallback",
		"cid", notif.Ref.GetCid(),
		"peer", peerIDStr,
		"totalLabels", len(labelList),
		"cached", cachedCount,
		"source", "pull_fallback")
}

// hasRemoteRecordCached checks if we already have cached labels for this remote record.
// This helps avoid duplicate work and identifies reannouncement events.
func (r *routeRemote) hasRemoteRecordCached(ctx context.Context, cid, peerID string) bool {
	entries, err := QueryAllNamespaces(ctx, r.dstore)
	if err != nil {
		remoteLogger.Error("Failed to get namespace entries for cache check", "error", err)

		return false
	}

	for _, entry := range entries {
		// Parse enhanced key to check if it matches our CID/PeerID
		_, keyCID, keyPeerID, err := ParseEnhancedLabelKey(entry.Key)
		if err != nil {
			continue
		}

		if keyCID == cid && keyPeerID == peerID {
			return true
		}
	}

	return false
}

// handleLabelAnnouncement processes incoming label announcements from GossipSub.
// This is the primary label discovery mechanism when GossipSub is enabled.
// It converts the wire format to storage format using existing infrastructure.
//
// Flow:
//  1. Skip own announcements (already cached locally)
//  2. Convert []string labels to labels.Label
//  3. Build enhanced keys: /skills/AI/CID/PeerID
//  4. Store labels.LabelMetadata in datastore
//
// This completely avoids pulling the entire record from remote peers,
// providing ~95% bandwidth savings and ~5-20ms propagation time.
func (r *routeRemote) handleLabelAnnouncement(ctx context.Context, ann *pubsub.LabelAnnouncement) {
	// Skip our own announcements (already cached during local Publish)
	if ann.PeerID == r.server.Host().ID().String() {
		return
	}

	remoteLogger.Info("Caching labels from GossipSub announcement",
		"cid", ann.CID,
		"peer", ann.PeerID,
		"labels", len(ann.Labels))

	now := time.Now()
	cachedCount := 0

	// Convert wire format ([]string) to storage format using existing infrastructure
	for _, labelStr := range ann.Labels {
		label := labels.Label(labelStr)

		// Use existing BuildEnhancedLabelKey function
		enhancedKey := BuildEnhancedLabelKey(label, ann.CID, ann.PeerID)

		// Use existing labels.LabelMetadata structure
		metadata := &labels.LabelMetadata{
			Timestamp: ann.Timestamp, // When label was announced
			LastSeen:  now,           // When we received it
		}

		metadataBytes, err := json.Marshal(metadata)
		if err != nil {
			remoteLogger.Warn("Failed to marshal label metadata",
				"key", enhancedKey,
				"error", err)

			continue
		}

		err = r.dstore.Put(ctx, datastore.NewKey(enhancedKey), metadataBytes)
		if err != nil {
			remoteLogger.Warn("Failed to cache label from GossipSub",
				"key", enhancedKey,
				"error", err)
		} else {
			cachedCount++
		}
	}

	remoteLogger.Info("Successfully cached labels from GossipSub",
		"cid", ann.CID,
		"peer", ann.PeerID,
		"total", len(ann.Labels),
		"cached", cachedCount)
}

// updateLabelMetadataTimestamp updates the lastSeen timestamp for a single cached label entry.
func (r *routeRemote) updateLabelMetadataTimestamp(ctx context.Context, key string, value []byte, timestamp time.Time) error {
	var metadata labels.LabelMetadata
	if err := json.Unmarshal(value, &metadata); err != nil {
		return fmt.Errorf("failed to unmarshal label metadata: %w", err)
	}

	metadata.LastSeen = timestamp

	metadataBytes, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("failed to marshal label metadata: %w", err)
	}

	err = r.dstore.Put(ctx, datastore.NewKey(key), metadataBytes)
	if err != nil {
		return fmt.Errorf("failed to save label metadata: %w", err)
	}

	return nil
}

// updateRemoteRecordLastSeen updates the lastSeen timestamp for all cached labels
// from a specific remote peer/CID combination (for reannouncement handling).
func (r *routeRemote) updateRemoteRecordLastSeen(ctx context.Context, cid, peerID string) {
	now := time.Now()
	updatedCount := 0

	entries, err := QueryAllNamespaces(ctx, r.dstore)
	if err != nil {
		remoteLogger.Error("Failed to get namespace entries for lastSeen update", "error", err)

		return
	}

	for _, entry := range entries {
		// Parse enhanced key to check if it matches our CID/PeerID
		_, keyCID, keyPeerID, err := ParseEnhancedLabelKey(entry.Key)
		if err != nil {
			continue
		}

		if keyCID == cid && keyPeerID == peerID {
			if err := r.updateLabelMetadataTimestamp(ctx, entry.Key, entry.Value, now); err != nil {
				remoteLogger.Warn("Failed to update lastSeen for cached label", "key", entry.Key, "error", err)
			} else {
				updatedCount++

				remoteLogger.Debug("Updated lastSeen for cached label", "key", entry.Key)
			}
		}
	}

	remoteLogger.Debug("Updated lastSeen timestamps for reannounced record",
		"cid", cid, "peer", peerID, "updatedLabels", updatedCount)
}

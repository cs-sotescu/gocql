package gocql

import (
	"sync"
	"sync/atomic"
)

// ClusterMetadata holds metadata about cluster topology.
// It is used inside atomic.Value and shallow copies are used when replacing it,
// so fields should not be modified in-place. Instead, to modify a field a copy of the field should be made
// and the pointer in ClusterMetadata updated to point to the new value.
type ClusterMetadata struct {
	// replicas is map[keyspace]map[Token]hosts
	replicas  map[string]tokenRingReplicas
	tokenRing *TokenRing
}

// TokenRing returns the token ring.
// Please note that the token ring is only available if at least one cluster node is known and up.
// Several [ClusterConfig] parameters can affect the availability or reliability of the token ring:
// * DisableInitialHostLookup will disable host discovery and therefore the token ring availability.
// * Events.DisableNodeStatusEvents will turn off processing of STATUS_CHANGE events,
//   therefore the token ring will not be updated in response to host UP/DOWN events.
// * Events.DisableTopologyEvents will turn off processing of TOPOLOGY_CHANGE events,
//	 therefore the token ring will not be updated in response to cluster topology changes.
func (m *ClusterMetadata) TokenRing() *TokenRing {
	return m.tokenRing
}

// resetTokenRing creates a new TokenRing.
// It must be called with t.mu locked.
func (m *ClusterMetadata) resetTokenRing(partitioner string, hosts []*HostInfo, logger StdLogger) {
	if partitioner == "" {
		// partitioner not yet set
		return
	}

	// create a new Token ring
	tokenRing, err := newTokenRing(partitioner, hosts)
	if err != nil {
		logger.Printf("Unable to update the token ring due to error: %s", err)
		return
	}

	// replace the Token ring
	m.tokenRing = tokenRing
}

// clusterMetadataManager manages cluster metadata.
type clusterMetadataManager struct {
	getKeyspaceMetadata func(keyspace string) (*KeyspaceMetadata, error)
	getKeyspaceName     func() string

	// mu protects writes to hosts, partitioner, metadata.
	// reads can be unlocked as long as they are not used for updating state later.
	mu          sync.Mutex
	hosts       cowHostList
	partitioner string
	metadata    atomic.Value // *ClusterMetadata

	logger StdLogger
}

func (m *clusterMetadataManager) init(s *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getKeyspaceMetadata != nil {
		// Init was already called.
		// See https://github.com/scylladb/gocql/issues/94.
		panic("sharing token aware host selection policy between sessions is not supported")
	}
	m.getKeyspaceMetadata = s.KeyspaceMetadata
	m.getKeyspaceName = func() string { return s.cfg.Keyspace }
	m.logger = s.logger
}

func (m *clusterMetadataManager) keyspaceChanged(update KeyspaceUpdateEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	meta := m.getMetadataForUpdate()
	m.updateReplicas(meta, update.Keyspace)
	m.metadata.Store(meta)
}

func (m *clusterMetadataManager) setPartitioner(partitioner string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.partitioner != partitioner {
		m.partitioner = partitioner
		meta := m.getMetadataForUpdate()
		meta.resetTokenRing(m.partitioner, m.hosts.get(), m.logger)
		m.updateReplicas(meta, m.getKeyspaceName())
		m.metadata.Store(meta)
	}
}

func (m *clusterMetadataManager) addHost(host *HostInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.hosts.add(host) {
		meta := m.getMetadataForUpdate()
		meta.resetTokenRing(m.partitioner, m.hosts.get(), m.logger)
		m.updateReplicas(meta, m.getKeyspaceName())
		m.metadata.Store(meta)
	}
}

func (m *clusterMetadataManager) addHosts(hosts []*HostInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, host := range hosts {
		m.hosts.add(host)
	}

	meta := m.getMetadataForUpdate()
	meta.resetTokenRing(m.partitioner, m.hosts.get(), m.logger)
	m.updateReplicas(meta, m.getKeyspaceName())
	m.metadata.Store(meta)
}

func (m *clusterMetadataManager) removeHost(host *HostInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.hosts.remove(host.ConnectAddress()) {
		meta := m.getMetadataForUpdate()
		meta.resetTokenRing(m.partitioner, m.hosts.get(), m.logger)
		m.updateReplicas(meta, m.getKeyspaceName())
		m.metadata.Store(meta)
	}
}

// getMetadataReadOnly returns current cluster metadata.
// Metadata uses copy on write, so the returned value should be only used for reading.
// To obtain a copy that could be updated, use getMetadataForUpdate instead.
func (m *clusterMetadataManager) getMetadataReadOnly() *ClusterMetadata {
	meta, _ := m.metadata.Load().(*ClusterMetadata)
	return meta
}

// getMetadataForUpdate returns ClusterMetadata suitable for updating.
// It is a SHALLOW copy of current metadata in case it was already set or new empty ClusterMetadata otherwise.
// This function should be called with t.mu mutex locked and the mutex should not be released before
// storing the new metadata.
func (m *clusterMetadataManager) getMetadataForUpdate() *ClusterMetadata {
	metaReadOnly := m.getMetadataReadOnly()
	meta := new(ClusterMetadata)
	if metaReadOnly != nil {
		*meta = *metaReadOnly
	}
	return meta
}

// updateReplicas updates replicas in ClusterMetadata.
// It must be called with t.mu mutex locked.
// meta must not be nil and it's replicas field will be updated.
func (m *clusterMetadataManager) updateReplicas(meta *ClusterMetadata, keyspace string) {
	newReplicas := make(map[string]tokenRingReplicas, len(meta.replicas))

	ks, err := m.getKeyspaceMetadata(keyspace)
	if err == nil {
		strat := getStrategy(ks, m.logger)
		if strat != nil {
			if meta != nil && meta.tokenRing != nil {
				newReplicas[keyspace] = strat.replicaMap(meta.tokenRing)
			}
		}
	}

	for ks, replicas := range meta.replicas {
		if ks != keyspace {
			newReplicas[ks] = replicas
		}
	}

	meta.replicas = newReplicas
}

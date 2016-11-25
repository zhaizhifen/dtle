package client

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/armon/go-metrics"
	"github.com/hashicorp/consul/lib"
	"github.com/hashicorp/go-multierror"
	"github.com/mitchellh/hashstructure"
	gnatsd "github.com/nats-io/gnatsd/server"

	"udup/client/allocdir"
	"udup/client/config"
	"udup/client/fingerprint"
	"udup/client/rpcproxy"
	"udup/client/stats"
	"udup/server"
	"udup/server/structs"
)

const (
	// clientRPCCache controls how long we keep an idle connection
	// open to a server
	clientRPCCache = 5 * time.Minute

	// clientMaxStreams controsl how many idle streams we keep
	// open to a server
	clientMaxStreams = 2

	// datacenterQueryLimit searches through up to this many adjacent
	// datacenters looking for the Udup server service.
	datacenterQueryLimit = 9

	// registerRetryIntv is minimum interval on which we retry
	// registration. We pick a value between this and 2x this.
	registerRetryIntv = 15 * time.Second

	// getAllocRetryIntv is minimum interval on which we retry
	// to fetch allocations. We pick a value between this and 2x this.
	getAllocRetryIntv = 30 * time.Second

	// devModeRetryIntv is the retry interval used for development
	devModeRetryIntv = time.Second

	// stateSnapshotIntv is how often the client snapshots state
	stateSnapshotIntv = 60 * time.Second

	// registerErrGrace is the grace period where we don't log about
	// register errors after start. This is to improve the user experience
	// in dev mode where the leader isn't elected for a few seconds.
	registerErrGrace = 10 * time.Second

	// initialHeartbeatStagger is used to stagger the interval between
	// starting and the intial heartbeat. After the intial heartbeat,
	// we switch to using the TTL specified by the servers.
	initialHeartbeatStagger = 10 * time.Second

	// nodeUpdateRetryIntv is how often the client checks for updates to the
	// node attributes or meta map.
	nodeUpdateRetryIntv = 5 * time.Second

	// allocSyncIntv is the batching period of allocation updates before they
	// are synced with the server.
	allocSyncIntv = 200 * time.Millisecond

	// allocSyncRetryIntv is the interval on which we retry updating
	// the status of the allocation
	allocSyncRetryIntv = 5 * time.Second
)

// ClientStatsReporter exposes all the APIs related to resource usage of a Udup
// Client
type ClientStatsReporter interface {
	// GetAllocStats returns the AllocStatsReporter for the passed allocation.
	// If it does not exist an error is reported.
	GetAllocStats(allocID string) (AllocStatsReporter, error)

	// LatestHostStats returns the latest resource usage stats for the host
	LatestHostStats() *stats.HostStats
}

// Client is used to implement the client interaction with Udup. Clients
// are expected to register as a schedulable node to the servers, and to
// run allocations as determined by the servers.
type Client struct {
	config *config.Config
	start  time.Time

	// configCopy is a copy that should be passed to alloc-runners.
	configCopy *config.Config
	configLock sync.RWMutex

	logger *log.Logger

	rpcProxy *rpcproxy.RPCProxy

	connPool *server.ConnPool

	// lastHeartbeatFromQuorum is an atomic int32 acting as a bool.  When
	// true, the last heartbeat message had a leader.  When false (0),
	// the last heartbeat did not include the RPC address of the leader,
	// indicating that the server is in the minority or middle of an
	// election.
	lastHeartbeatFromQuorum int32

	lastHeartbeat time.Time
	heartbeatTTL  time.Duration
	heartbeatLock sync.Mutex

	// allocs is the current set of allocations
	allocs    map[string]*AllocRunner
	allocLock sync.RWMutex

	// blockedAllocations are allocations which are blocked because their
	// chained allocations haven't finished running
	blockedAllocations map[string]*structs.Allocation
	blockedAllocsLock  sync.RWMutex

	// allocUpdates stores allocations that need to be synced to the server.
	allocUpdates chan *structs.Allocation

	// HostStatsCollector collects host resource usage stats
	hostStatsCollector *stats.HostStatsCollector
	resourceUsage      *stats.HostStats
	resourceUsageLock  sync.RWMutex

	shutdown     bool
	shutdownCh   chan struct{}
	shutdownLock sync.Mutex
}

// NewClient is used to create a new client from the given configuration
func NewClient(cfg *config.Config, logger *log.Logger) (*Client, error) {
	// Create the client
	c := &Client{
		config:             cfg,
		start:              time.Now(),
		connPool:           server.NewPool(cfg.LogOutput, clientRPCCache, clientMaxStreams),
		logger:             logger,
		hostStatsCollector: stats.NewHostStatsCollector(),
		allocs:             make(map[string]*AllocRunner),
		blockedAllocations: make(map[string]*structs.Allocation),
		allocUpdates:       make(chan *structs.Allocation, 64),
		shutdownCh:         make(chan struct{}),
	}

	// Initialize the client
	if err := c.init(); err != nil {
		return nil, fmt.Errorf("failed to initialize client: %v", err)
	}

	// Setup the node
	if err := c.setupNode(); err != nil {
		return nil, fmt.Errorf("node setup failed: %v", err)
	}

	// Fingerprint the node
	if err := c.fingerprint(); err != nil {
		return nil, fmt.Errorf("fingerprinting failed: %v", err)
	}

	// Setup the reserved resources
	c.reservePorts()

	// Store the config copy before restoring state but after it has been
	// initialized.
	c.configLock.Lock()
	c.configCopy = c.config.Copy()
	c.configLock.Unlock()

	// Create the RPC Proxy and bootstrap with the preconfigured list of
	// static servers
	c.configLock.RLock()
	c.rpcProxy = rpcproxy.NewRPCProxy(c.logger, c.shutdownCh, c, c.connPool)
	for _, serverAddr := range c.configCopy.Servers {
		c.rpcProxy.AddPrimaryServer(serverAddr)
	}
	c.configLock.RUnlock()

	// Restore the state
	if err := c.restoreState(); err != nil {
		return nil, fmt.Errorf("failed to restore state: %v", err)
	}

	// Register and then start heartbeating to the servers.
	go c.registerAndHeartbeat()

	// Begin periodic snapshotting of state.
	go c.periodicSnapshot()

	// Begin syncing allocations to the server
	go c.allocSync()

	// Start the client!
	go c.run()

	// Start collecting stats
	go c.collectHostStats()

	// Start the RPCProxy maintenance task.  This task periodically
	// shuffles the list of Udup Server Endpoints this Client will use
	// when communicating with Udup Servers via RPC.  This is done in
	// order to prevent server fixation in stable Udup clusters.  This
	// task actively populates the active list of Udup Server Endpoints
	// from information from the Udup Client heartbeats.  If a heartbeat
	// times out and there are no Udup servers available
	go c.rpcProxy.Run()

	// start yourself an embedded gnatsd server
	opts := gnatsd.Options{
		Host:  "127.0.0.1",
		Port:  13003,
		Trace: true,
		Debug: true,
	}
	gnats := gnatsd.New(&opts)
	go gnats.Start()

	return c, nil
}

// init is used to initialize the client and perform any setup
// needed before we begin starting its various components.
func (c *Client) init() error {
	// Ensure the state dir exists if we have one
	if c.config.StateDir != "" {
		if err := os.MkdirAll(c.config.StateDir, 0700); err != nil {
			return fmt.Errorf("failed creating state dir: %s", err)
		}

	} else {
		// Othewise make a temp directory to use.
		p, err := ioutil.TempDir("", "UdupClient")
		if err != nil {
			return fmt.Errorf("failed creating temporary directory for the StateDir: %v", err)
		}
		c.config.StateDir = p
	}
	c.logger.Printf("[INFO] client: using state directory %v", c.config.StateDir)

	// Ensure the alloc dir exists if we have one
	if c.config.AllocDir != "" {
		if err := os.MkdirAll(c.config.AllocDir, 0755); err != nil {
			return fmt.Errorf("failed creating alloc dir: %s", err)
		}
	} else {
		// Othewise make a temp directory to use.
		p, err := ioutil.TempDir("", "UdupClient")
		if err != nil {
			return fmt.Errorf("failed creating temporary directory for the AllocDir: %v", err)
		}
		c.config.AllocDir = p
	}

	c.logger.Printf("[INFO] client: using alloc directory %v", c.config.AllocDir)
	return nil
}

// Leave is used to prepare the client to leave the cluster
func (c *Client) Leave() error {
	// TODO
	return nil
}

// Datacenter returns the datacenter for the given client
func (c *Client) Datacenter() string {
	c.configLock.RLock()
	dc := c.configCopy.Node.Datacenter
	c.configLock.RUnlock()
	return dc
}

// Region returns the region for the given client
func (c *Client) Region() string {
	return c.config.Region
}

// RPCMajorVersion returns the structs.ApiMajorVersion supported by the
// client.
func (c *Client) RPCMajorVersion() int {
	return structs.ApiMajorVersion
}

// RPCMinorVersion returns the structs.ApiMinorVersion supported by the
// client.
func (c *Client) RPCMinorVersion() int {
	return structs.ApiMinorVersion
}

// Shutdown is used to tear down the client
func (c *Client) Shutdown() error {
	c.logger.Printf("[INFO] client: shutting down")
	c.shutdownLock.Lock()
	defer c.shutdownLock.Unlock()

	if c.shutdown {
		return nil
	}

	// Destroy all the running allocations.
	if c.config.DevMode {
		c.allocLock.Lock()
		for _, ar := range c.allocs {
			ar.Destroy()
			<-ar.WaitCh()
		}
		c.allocLock.Unlock()
	}

	c.shutdown = true
	close(c.shutdownCh)
	c.connPool.Shutdown()
	return c.saveState()
}

// RPC is used to forward an RPC call to a Udup server, or fail if no servers
func (c *Client) RPC(method string, args interface{}, reply interface{}) error {
	// Invoke the RPCHandler if it exists
	if c.config.RPCHandler != nil {
		return c.config.RPCHandler.RPC(method, args, reply)
	}

	// Pick a server to request from
	server := c.rpcProxy.FindServer()
	if server == nil {
		return fmt.Errorf("no known servers")
	}

	// Make the RPC request
	if err := c.connPool.RPC(c.Region(), server.Addr, c.RPCMajorVersion(), method, args, reply); err != nil {
		c.rpcProxy.NotifyFailedServer(server)
		return fmt.Errorf("RPC failed to server %s: %v", server.Addr, err)
	}
	return nil
}

// Stats is used to return statistics for debugging and insight
// for various sub-systems
func (c *Client) Stats() map[string]map[string]string {
	toString := func(v uint64) string {
		return strconv.FormatUint(v, 10)
	}
	c.allocLock.RLock()
	numAllocs := len(c.allocs)
	c.allocLock.RUnlock()

	c.heartbeatLock.Lock()
	defer c.heartbeatLock.Unlock()
	stats := map[string]map[string]string{
		"client": map[string]string{
			"node_id":         c.Node().ID,
			"known_servers":   toString(uint64(c.rpcProxy.NumServers())),
			"num_allocations": toString(uint64(numAllocs)),
			"last_heartbeat":  fmt.Sprintf("%v", time.Since(c.lastHeartbeat)),
			"heartbeat_ttl":   fmt.Sprintf("%v", c.heartbeatTTL),
		},
		"runtime": server.RuntimeStats(),
	}
	return stats
}

// Node returns the locally registered node
func (c *Client) Node() *structs.Node {
	c.configLock.RLock()
	defer c.configLock.RUnlock()
	return c.config.Node
}

// StatsReporter exposes the various APIs related resource usage of a Udup
// client
func (c *Client) StatsReporter() ClientStatsReporter {
	return c
}

func (c *Client) GetAllocStats(allocID string) (AllocStatsReporter, error) {
	c.allocLock.RLock()
	defer c.allocLock.RUnlock()
	ar, ok := c.allocs[allocID]
	if !ok {
		return nil, fmt.Errorf("unknown allocation ID %q", allocID)
	}
	return ar.StatsReporter(), nil
}

// HostStats returns all the stats related to a Udup client
func (c *Client) LatestHostStats() *stats.HostStats {
	c.resourceUsageLock.RLock()
	defer c.resourceUsageLock.RUnlock()
	return c.resourceUsage
}

// GetAllocFS returns the AllocFS interface for the alloc dir of an allocation
func (c *Client) GetAllocFS(allocID string) (allocdir.AllocDirFS, error) {
	c.allocLock.RLock()
	defer c.allocLock.RUnlock()

	ar, ok := c.allocs[allocID]
	if !ok {
		return nil, fmt.Errorf("alloc not found")
	}
	return ar.ctx.AllocDir, nil
}

// AddPrimaryServerToRPCProxy adds serverAddr to the RPC Proxy's primary
// server list.
func (c *Client) AddPrimaryServerToRPCProxy(serverAddr string) *rpcproxy.ServerEndpoint {
	return c.rpcProxy.AddPrimaryServer(serverAddr)
}

// restoreState is used to restore our state from the data dir
func (c *Client) restoreState() error {
	if c.config.DevMode {
		return nil
	}

	// Scan the directory
	list, err := ioutil.ReadDir(filepath.Join(c.config.StateDir, "alloc"))
	if err != nil && os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("failed to list alloc state: %v", err)
	}

	// Load each alloc back
	var mErr multierror.Error
	for _, entry := range list {
		id := entry.Name()
		alloc := &structs.Allocation{ID: id}
		c.configLock.RLock()
		ar := NewAllocRunner(c.logger, c.configCopy, c.updateAllocStatus, alloc)
		c.configLock.RUnlock()
		c.allocLock.Lock()
		c.allocs[id] = ar
		c.allocLock.Unlock()
		if err := ar.RestoreState(); err != nil {
			c.logger.Printf("[ERR] client: failed to restore state for alloc %s: %v", id, err)
			mErr.Errors = append(mErr.Errors, err)
		} else {
			go ar.Run()
		}
	}
	return mErr.ErrorOrNil()
}

// saveState is used to snapshot our state into the data dir
func (c *Client) saveState() error {
	if c.config.DevMode {
		return nil
	}

	var mErr multierror.Error
	for id, ar := range c.getAllocRunners() {
		if err := ar.SaveState(); err != nil {
			c.logger.Printf("[ERR] client: failed to save state for alloc %s: %v",
				id, err)
			mErr.Errors = append(mErr.Errors, err)
		}
	}
	return mErr.ErrorOrNil()
}

// getAllocRunners returns a snapshot of the current set of alloc runners.
func (c *Client) getAllocRunners() map[string]*AllocRunner {
	c.allocLock.RLock()
	defer c.allocLock.RUnlock()
	runners := make(map[string]*AllocRunner, len(c.allocs))
	for id, ar := range c.allocs {
		runners[id] = ar
	}
	return runners
}

// nodeIDs restores the nodes persistent unique ID or generates new
// ones
func (c *Client) nodeID() (id string, err error) {
	// Do not persist in dev mode
	if c.config.DevMode {
		return structs.GenerateUUID(), nil
	}

	// Attempt to read existing ID
	idPath := filepath.Join(c.config.StateDir, "client-id")
	idBuf, err := ioutil.ReadFile(idPath)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}

	// Use existing ID if any
	if len(idBuf) != 0 {
		id = string(idBuf)
	} else {
		// Generate new ID
		id = structs.GenerateUUID()

		// Persist the ID
		if err := ioutil.WriteFile(idPath, []byte(id), 0700); err != nil {
			return "", err
		}
	}

	return id, nil
}

// setupNode is used to setup the initial node
func (c *Client) setupNode() error {
	node := c.config.Node
	if node == nil {
		node = &structs.Node{}
		c.config.Node = node
	}
	// Generate an iD for the node
	id, err := c.nodeID()
	if err != nil {
		return fmt.Errorf("node ID setup failed: %v", err)
	}

	node.ID = id
	if node.Attributes == nil {
		node.Attributes = make(map[string]string)
	}
	if node.Links == nil {
		node.Links = make(map[string]string)
	}
	if node.Meta == nil {
		node.Meta = make(map[string]string)
	}
	if node.Resources == nil {
		node.Resources = &structs.Resources{}
	}
	if node.Reserved == nil {
		node.Reserved = &structs.Resources{}
	}
	if node.Datacenter == "" {
		node.Datacenter = "dc1"
	}
	if node.Name == "" {
		node.Name, _ = os.Hostname()
	}
	if node.Name == "" {
		node.Name = node.ID
	}
	node.Status = structs.NodeStatusInit
	return nil
}

// reservePorts is used to reserve ports on the fingerprinted network devices.
func (c *Client) reservePorts() {
	c.configLock.RLock()
	defer c.configLock.RUnlock()
	global := c.config.GloballyReservedPorts
	if len(global) == 0 {
		return
	}

	node := c.config.Node
	networks := node.Resources.Networks
	reservedIndex := make(map[string]*structs.NetworkResource, len(networks))
	for _, resNet := range node.Reserved.Networks {
		reservedIndex[resNet.IP] = resNet
	}

	// Go through each network device and reserve ports on it.
	for _, net := range networks {
		res, ok := reservedIndex[net.IP]
		if !ok {
			res = net.Copy()
			res.MBits = 0
			reservedIndex[net.IP] = res
		}

		for _, portVal := range global {
			p := structs.Port{Value: portVal}
			res.ReservedPorts = append(res.ReservedPorts, p)
		}
	}

	// Clear the reserved networks.
	if node.Reserved == nil {
		node.Reserved = new(structs.Resources)
	} else {
		node.Reserved.Networks = nil
	}

	// Restore the reserved networks
	for _, net := range reservedIndex {
		node.Reserved.Networks = append(node.Reserved.Networks, net)
	}
}

// fingerprint is used to fingerprint the client and setup the node
func (c *Client) fingerprint() error {
	whitelist := c.config.ReadStringListToMap("fingerprint.whitelist")
	whitelistEnabled := len(whitelist) > 0
	c.logger.Printf("[DEBUG] client: built-in fingerprints: %v", fingerprint.BuiltinFingerprints())

	var applied []string
	var skipped []string
	for _, name := range fingerprint.BuiltinFingerprints() {
		// Skip modules that are not in the whitelist if it is enabled.
		if _, ok := whitelist[name]; whitelistEnabled && !ok {
			skipped = append(skipped, name)
			continue
		}
		f, err := fingerprint.NewFingerprint(name, c.logger)
		if err != nil {
			return err
		}

		c.configLock.Lock()
		applies, err := f.Fingerprint(c.config, c.config.Node)
		c.configLock.Unlock()
		if err != nil {
			return err
		}
		if applies {
			applied = append(applied, name)
		}
		p, period := f.Periodic()
		if p {
			// TODO: If more periodic fingerprinters are added, then
			// fingerprintPeriodic should be used to handle all the periodic
			// fingerprinters by using a priority queue.
			go c.fingerprintPeriodic(name, f, period)
		}
	}
	c.logger.Printf("[DEBUG] client: applied fingerprints %v", applied)
	if len(skipped) != 0 {
		c.logger.Printf("[DEBUG] client: fingerprint modules skipped due to whitelist: %v", skipped)
	}
	return nil
}

// fingerprintPeriodic runs a fingerprinter at the specified duration.
func (c *Client) fingerprintPeriodic(name string, f fingerprint.Fingerprint, d time.Duration) {
	c.logger.Printf("[DEBUG] client: fingerprinting %v every %v", name, d)
	for {
		select {
		case <-time.After(d):
			c.configLock.Lock()
			if _, err := f.Fingerprint(c.config, c.config.Node); err != nil {
				c.logger.Printf("[DEBUG] client: periodic fingerprinting for %v failed: %v", name, err)
			}
			c.configLock.Unlock()
		case <-c.shutdownCh:
			return
		}
	}
}

// retryIntv calculates a retry interval value given the base
func (c *Client) retryIntv(base time.Duration) time.Duration {
	if c.config.DevMode {
		return devModeRetryIntv
	}
	return base + lib.RandomStagger(base)
}

// registerAndHeartbeat is a long lived goroutine used to register the client
// and then start heartbeatng to the server.
func (c *Client) registerAndHeartbeat() {
	// Register the node
	c.retryRegisterNode()

	// Start watching changes for node changes
	go c.watchNodeUpdates()

	// Setup the heartbeat timer, for the initial registration
	// we want to do this quickly. We want to do it extra quickly
	// in development mode.
	var heartbeat <-chan time.Time
	if c.config.DevMode {
		heartbeat = time.After(0)
	} else {
		heartbeat = time.After(lib.RandomStagger(initialHeartbeatStagger))
	}

	for {
		select {
		case <-heartbeat:
			if err := c.updateNodeStatus(); err != nil {
				// The servers have changed such that this node has not been
				// registered before
				if strings.Contains(err.Error(), "node not found") {
					// Re-register the node
					c.logger.Printf("[INFO] client: re-registering node")
					c.retryRegisterNode()
					heartbeat = time.After(lib.RandomStagger(initialHeartbeatStagger))
				} else {
					intv := c.retryIntv(registerRetryIntv)
					c.logger.Printf("[ERR] client: heartbeating failed. Retrying in %v: %v", intv, err)
					heartbeat = time.After(intv)
				}
			} else {
				c.heartbeatLock.Lock()
				heartbeat = time.After(c.heartbeatTTL)
				c.heartbeatLock.Unlock()
			}

		case <-c.shutdownCh:
			return
		}
	}
}

// periodicSnapshot is a long lived goroutine used to periodically snapshot the
// state of the client
func (c *Client) periodicSnapshot() {
	// Create a snapshot timer
	snapshot := time.After(stateSnapshotIntv)

	for {
		select {
		case <-snapshot:
			snapshot = time.After(stateSnapshotIntv)
			if err := c.saveState(); err != nil {
				c.logger.Printf("[ERR] client: failed to save state: %v", err)
			}

		case <-c.shutdownCh:
			return
		}
	}
}

// run is a long lived goroutine used to run the client
func (c *Client) run() {
	// Watch for changes in allocations
	allocUpdates := make(chan *allocUpdates, 8)
	go c.watchAllocations(allocUpdates)

	for {
		select {
		case update := <-allocUpdates:
			c.runAllocs(update)

		case <-c.shutdownCh:
			return
		}
	}
}

// hasNodeChanged calculates a hash for the node attributes- and meta map.
// The new hash values are compared against the old (passed-in) hash values to
// determine if the node properties have changed. It returns the new hash values
// in case they are different from the old hash values.
func (c *Client) hasNodeChanged(oldAttrHash uint64, oldMetaHash uint64) (bool, uint64, uint64) {
	c.configLock.RLock()
	defer c.configLock.RUnlock()
	newAttrHash, err := hashstructure.Hash(c.config.Node.Attributes, nil)
	if err != nil {
		c.logger.Printf("[DEBUG] client: unable to calculate node attributes hash: %v", err)
	}
	// Calculate node meta map hash
	newMetaHash, err := hashstructure.Hash(c.config.Node.Meta, nil)
	if err != nil {
		c.logger.Printf("[DEBUG] client: unable to calculate node meta hash: %v", err)
	}
	if newAttrHash != oldAttrHash || newMetaHash != oldMetaHash {
		return true, newAttrHash, newMetaHash
	}
	return false, oldAttrHash, oldMetaHash
}

// retryRegisterNode is used to register the node or update the registration and
// retry in case of failure.
func (c *Client) retryRegisterNode() {
	// Register the client
	for {
		if err := c.registerNode(); err == nil {
			break
		} else {
			c.logger.Printf("[ERR] client: %v", err)
		}
		select {
		case <-time.After(c.retryIntv(registerRetryIntv)):
		case <-c.shutdownCh:
			return
		}
	}
}

// registerNode is used to register the node or update the registration
func (c *Client) registerNode() error {
	node := c.Node()
	req := structs.NodeRegisterRequest{
		Node:         node,
		WriteRequest: structs.WriteRequest{Region: c.Region()},
	}
	var resp structs.NodeUpdateResponse
	if err := c.RPC("Node.Register", &req, &resp); err != nil {
		if time.Since(c.start) > registerErrGrace {
			return fmt.Errorf("failed to register node: %v", err)
		}
		return err
	}

	// Update the node status to ready after we register.
	c.configLock.Lock()
	node.Status = structs.NodeStatusReady
	c.configLock.Unlock()

	c.logger.Printf("[DEBUG] client: node registration complete")
	if len(resp.EvalIDs) != 0 {
		c.logger.Printf("[DEBUG] client: %d evaluations triggered by node registration", len(resp.EvalIDs))
	}

	c.heartbeatLock.Lock()
	defer c.heartbeatLock.Unlock()
	c.lastHeartbeat = time.Now()
	c.heartbeatTTL = resp.HeartbeatTTL
	return nil
}

// updateNodeStatus is used to heartbeat and update the status of the node
func (c *Client) updateNodeStatus() error {
	node := c.Node()
	req := structs.NodeUpdateStatusRequest{
		NodeID:       node.ID,
		Status:       structs.NodeStatusReady,
		WriteRequest: structs.WriteRequest{Region: c.Region()},
	}
	var resp structs.NodeUpdateResponse
	if err := c.RPC("Node.UpdateStatus", &req, &resp); err != nil {
		return fmt.Errorf("failed to update status: %v", err)
	}
	if len(resp.EvalIDs) != 0 {
		c.logger.Printf("[DEBUG] client: %d evaluations triggered by node update", len(resp.EvalIDs))
	}
	if resp.Index != 0 {
		c.logger.Printf("[DEBUG] client: state updated to %s", req.Status)
	}

	c.heartbeatLock.Lock()
	defer c.heartbeatLock.Unlock()
	c.lastHeartbeat = time.Now()
	c.heartbeatTTL = resp.HeartbeatTTL

	if err := c.rpcProxy.RefreshServerLists(resp.Servers, resp.NumNodes, resp.LeaderRPCAddr); err != nil {
		return err
	}

	// Begin polling Consul if there is no Udup leader.  We could be
	// heartbeating to a Udup server that is in the minority of a
	// partition of the Udup server quorum, but this Udup Agent still
	// has connectivity to the existing majority of Udup Servers, but
	// only if it queries Consul.
	if resp.LeaderRPCAddr == "" {
		atomic.CompareAndSwapInt32(&c.lastHeartbeatFromQuorum, 1, 0)
		return nil
	}

	const heartbeatFallbackFactor = 3
	atomic.CompareAndSwapInt32(&c.lastHeartbeatFromQuorum, 0, 1)

	return nil
}

// updateAllocStatus is used to update the status of an allocation
func (c *Client) updateAllocStatus(alloc *structs.Allocation) {
	// Only send the fields that are updatable by the client.
	stripped := new(structs.Allocation)
	stripped.ID = alloc.ID
	stripped.NodeID = c.Node().ID
	stripped.TaskStates = alloc.TaskStates
	stripped.ClientStatus = alloc.ClientStatus
	stripped.ClientDescription = alloc.ClientDescription
	select {
	case c.allocUpdates <- stripped:
	case <-c.shutdownCh:
	}
}

// allocSync is a long lived function that batches allocation updates to the
// server.
func (c *Client) allocSync() {
	staggered := false
	syncTicker := time.NewTicker(allocSyncIntv)
	updates := make(map[string]*structs.Allocation)
	for {
		select {
		case <-c.shutdownCh:
			syncTicker.Stop()
			return
		case alloc := <-c.allocUpdates:
			// Batch the allocation updates until the timer triggers.
			updates[alloc.ID] = alloc

			// If this alloc was blocking another alloc and transitioned to a
			// terminal state then start the blocked allocation
			c.blockedAllocsLock.Lock()
			if blockedAlloc, ok := c.blockedAllocations[alloc.ID]; ok && alloc.Terminated() {
				if err := c.addAlloc(blockedAlloc); err != nil {
					c.logger.Printf("[ERR] client: failed to add alloc which was previously blocked %q: %v",
						blockedAlloc.ID, err)
				}
				delete(c.blockedAllocations, blockedAlloc.PreviousAllocation)
			}
			c.blockedAllocsLock.Unlock()
		case <-syncTicker.C:
			// Fast path if there are no updates
			if len(updates) == 0 {
				continue
			}

			sync := make([]*structs.Allocation, 0, len(updates))
			for _, alloc := range updates {
				sync = append(sync, alloc)
			}

			// Send to server.
			args := structs.AllocUpdateRequest{
				Alloc:        sync,
				WriteRequest: structs.WriteRequest{Region: c.Region()},
			}

			var resp structs.GenericResponse
			if err := c.RPC("Node.UpdateAlloc", &args, &resp); err != nil {
				c.logger.Printf("[ERR] client: failed to update allocations: %v", err)
				syncTicker.Stop()
				syncTicker = time.NewTicker(c.retryIntv(allocSyncRetryIntv))
				staggered = true
			} else {
				updates = make(map[string]*structs.Allocation)
				if staggered {
					syncTicker.Stop()
					syncTicker = time.NewTicker(allocSyncIntv)
					staggered = false
				}
			}
		}
	}
}

// allocUpdates holds the results of receiving updated allocations from the
// servers.
type allocUpdates struct {
	// pulled is the set of allocations that were downloaded from the servers.
	pulled map[string]*structs.Allocation

	// filtered is the set of allocations that were not pulled because their
	// AllocModifyIndex didn't change.
	filtered map[string]struct{}
}

// watchAllocations is used to scan for updates to allocations
func (c *Client) watchAllocations(updates chan *allocUpdates) {
	// The request and response for getting the map of allocations that should
	// be running on the Node to their AllocModifyIndex which is incremented
	// when the allocation is updated by the servers.
	n := c.Node()
	req := structs.NodeSpecificRequest{
		NodeID: n.ID,
		QueryOptions: structs.QueryOptions{
			Region:     c.Region(),
			AllowStale: true,
		},
	}
	var resp structs.NodeClientAllocsResponse

	// The request and response for pulling down the set of allocations that are
	// new, or updated server side.
	allocsReq := structs.AllocsGetRequest{
		QueryOptions: structs.QueryOptions{
			Region:     c.Region(),
			AllowStale: true,
		},
	}
	var allocsResp structs.AllocsGetResponse

	for {
		// Get the allocation modify index map, blocking for updates. We will
		// use this to determine exactly what allocations need to be downloaded
		// in full.
		resp = structs.NodeClientAllocsResponse{}
		err := c.RPC("Node.GetClientAllocs", &req, &resp)
		if err != nil {
			c.logger.Printf("[ERR] client: failed to query for node allocations: %v", err)
			retry := c.retryIntv(getAllocRetryIntv)
			select {
			case <-time.After(retry):
				continue
			case <-c.shutdownCh:
				return
			}
		}

		// Check for shutdown
		select {
		case <-c.shutdownCh:
			return
		default:
		}

		// Filter all allocations whose AllocModifyIndex was not incremented.
		// These are the allocations who have either not been updated, or whose
		// updates are a result of the client sending an update for the alloc.
		// This lets us reduce the network traffic to the server as we don't
		// need to pull all the allocations.
		var pull []string
		filtered := make(map[string]struct{})
		runners := c.getAllocRunners()
		for allocID, modifyIndex := range resp.Allocs {
			// Pull the allocation if we don't have an alloc runner for the
			// allocation or if the alloc runner requires an updated allocation.
			runner, ok := runners[allocID]
			if !ok || runner.shouldUpdate(modifyIndex) {
				pull = append(pull, allocID)
			} else {
				filtered[allocID] = struct{}{}
			}
		}

		c.logger.Printf("[DEBUG] client: updated allocations at index %d (pulled %d) (filtered %d)",
			resp.Index, len(pull), len(filtered))

		// Pull the allocations that passed filtering.
		allocsResp.Allocs = nil
		if len(pull) != 0 {
			// Pull the allocations that need to be updated.
			allocsReq.AllocIDs = pull
			allocsResp = structs.AllocsGetResponse{}
			if err := c.RPC("Alloc.GetAllocs", &allocsReq, &allocsResp); err != nil {
				c.logger.Printf("[ERR] client: failed to query updated allocations: %v", err)
				retry := c.retryIntv(getAllocRetryIntv)
				select {
				case <-time.After(retry):
					continue
				case <-c.shutdownCh:
					return
				}
			}

			// Check for shutdown
			select {
			case <-c.shutdownCh:
				return
			default:
			}
		}

		// Update the query index.
		if resp.Index > req.MinQueryIndex {
			req.MinQueryIndex = resp.Index
		}

		// Push the updates.
		pulled := make(map[string]*structs.Allocation, len(allocsResp.Allocs))
		for _, alloc := range allocsResp.Allocs {
			pulled[alloc.ID] = alloc
		}
		update := &allocUpdates{
			filtered: filtered,
			pulled:   pulled,
		}
		select {
		case updates <- update:
		case <-c.shutdownCh:
			return
		}
	}
}

// watchNodeUpdates periodically checks for changes to the node attributes or meta map
func (c *Client) watchNodeUpdates() {
	c.logger.Printf("[DEBUG] client: periodically checking for node changes at duration %v", nodeUpdateRetryIntv)

	// Initialize the hashes
	_, attrHash, metaHash := c.hasNodeChanged(0, 0)
	var changed bool
	for {
		select {
		case <-time.After(c.retryIntv(nodeUpdateRetryIntv)):
			changed, attrHash, metaHash = c.hasNodeChanged(attrHash, metaHash)
			if changed {
				c.logger.Printf("[DEBUG] client: state changed, updating node.")

				// Update the config copy.
				c.configLock.Lock()
				node := c.config.Node.Copy()
				c.configCopy.Node = node
				c.configLock.Unlock()

				c.retryRegisterNode()
			}
		case <-c.shutdownCh:
			return
		}
	}
}

// runAllocs is invoked when we get an updated set of allocations
func (c *Client) runAllocs(update *allocUpdates) {
	// Get the existing allocs
	c.allocLock.RLock()
	exist := make([]*structs.Allocation, 0, len(c.allocs))
	for _, ar := range c.allocs {
		exist = append(exist, ar.alloc)
	}
	c.allocLock.RUnlock()

	// Diff the existing and updated allocations
	diff := diffAllocs(exist, update)
	c.logger.Printf("[DEBUG] client: %#v", diff)

	// Remove the old allocations
	for _, remove := range diff.removed {
		if err := c.removeAlloc(remove); err != nil {
			c.logger.Printf("[ERR] client: failed to remove alloc '%s': %v",
				remove.ID, err)
		}
	}

	// Update the existing allocations
	for _, update := range diff.updated {
		if err := c.updateAlloc(update.exist, update.updated); err != nil {
			c.logger.Printf("[ERR] client: failed to update alloc '%s': %v",
				update.exist.ID, err)
		}
	}

	// Start the new allocations
	for _, add := range diff.added {
		// If the allocation is chanined and the previous allocation hasn't
		// terminated yet, then add the alloc to the blocked queue.
		if ar, ok := c.getAllocRunners()[add.PreviousAllocation]; ok && !ar.Alloc().Terminated() {
			c.logger.Printf("[DEBUG] client: added alloc %q to blocked queue", add.ID)
			c.blockedAllocsLock.Lock()
			c.blockedAllocations[add.PreviousAllocation] = add
			c.blockedAllocsLock.Unlock()
			continue
		}

		if err := c.addAlloc(add); err != nil {
			c.logger.Printf("[ERR] client: failed to add alloc '%s': %v",
				add.ID, err)
		}
	}

	// Persist our state
	if err := c.saveState(); err != nil {
		c.logger.Printf("[ERR] client: failed to save state: %v", err)
	}
}

// removeAlloc is invoked when we should remove an allocation
func (c *Client) removeAlloc(alloc *structs.Allocation) error {
	c.allocLock.Lock()
	ar, ok := c.allocs[alloc.ID]
	if !ok {
		c.allocLock.Unlock()
		c.logger.Printf("[WARN] client: missing context for alloc '%s'", alloc.ID)
		return nil
	}
	delete(c.allocs, alloc.ID)
	c.allocLock.Unlock()

	ar.Destroy()
	return nil
}

// updateAlloc is invoked when we should update an allocation
func (c *Client) updateAlloc(exist, update *structs.Allocation) error {
	c.allocLock.RLock()
	ar, ok := c.allocs[exist.ID]
	c.allocLock.RUnlock()
	if !ok {
		c.logger.Printf("[WARN] client: missing context for alloc '%s'", exist.ID)
		return nil
	}

	ar.Update(update)
	return nil
}

// addAlloc is invoked when we should add an allocation
func (c *Client) addAlloc(alloc *structs.Allocation) error {
	c.configLock.RLock()
	ar := NewAllocRunner(c.logger, c.configCopy, c.updateAllocStatus, alloc)
	c.configLock.RUnlock()
	go ar.Run()

	// Store the alloc runner.
	c.allocLock.Lock()
	c.allocs[alloc.ID] = ar
	c.allocLock.Unlock()
	return nil
}

// collectHostStats collects host resource usage stats periodically
func (c *Client) collectHostStats() {
	// Start collecting host stats right away and then keep collecting every
	// collection interval
	next := time.NewTimer(0)
	defer next.Stop()
	for {
		select {
		case <-next.C:
			ru, err := c.hostStatsCollector.Collect()
			next.Reset(c.config.StatsCollectionInterval)
			if err != nil {
				c.logger.Printf("[WARN] client: error fetching host resource usage stats: %v", err)
				continue
			}

			c.resourceUsageLock.Lock()
			c.resourceUsage = ru
			c.resourceUsageLock.Unlock()

		case <-c.shutdownCh:
			return
		}
	}
}

// emitStats pushes host resource usage stats to remote metrics collection sinks
func (c *Client) emitStats(hStats *stats.HostStats) {
	nodeID := c.Node().ID
	metrics.SetGauge([]string{"client", "host", "memory", nodeID, "total"}, float32(hStats.Memory.Total))
	metrics.SetGauge([]string{"client", "host", "memory", nodeID, "available"}, float32(hStats.Memory.Available))
	metrics.SetGauge([]string{"client", "host", "memory", nodeID, "used"}, float32(hStats.Memory.Used))
	metrics.SetGauge([]string{"client", "host", "memory", nodeID, "free"}, float32(hStats.Memory.Free))

	metrics.SetGauge([]string{"uptime"}, float32(hStats.Uptime))

	for _, cpu := range hStats.CPU {
		metrics.SetGauge([]string{"client", "host", "cpu", nodeID, cpu.CPU, "total"}, float32(cpu.Total))
		metrics.SetGauge([]string{"client", "host", "cpu", nodeID, cpu.CPU, "user"}, float32(cpu.User))
		metrics.SetGauge([]string{"client", "host", "cpu", nodeID, cpu.CPU, "idle"}, float32(cpu.Idle))
		metrics.SetGauge([]string{"client", "host", "cpu", nodeID, cpu.CPU, "system"}, float32(cpu.System))
	}

	for _, disk := range hStats.DiskStats {
		metrics.SetGauge([]string{"client", "host", "disk", nodeID, disk.Device, "size"}, float32(disk.Size))
		metrics.SetGauge([]string{"client", "host", "disk", nodeID, disk.Device, "used"}, float32(disk.Used))
		metrics.SetGauge([]string{"client", "host", "disk", nodeID, disk.Device, "available"}, float32(disk.Available))
		metrics.SetGauge([]string{"client", "host", "disk", nodeID, disk.Device, "used_percent"}, float32(disk.UsedPercent))
		metrics.SetGauge([]string{"client", "host", "disk", nodeID, disk.Device, "inodes_percent"}, float32(disk.InodesUsedPercent))
	}
}

// RPCProxy returns the Client's RPCProxy instance
func (c *Client) RPCProxy() *rpcproxy.RPCProxy {
	return c.rpcProxy
}
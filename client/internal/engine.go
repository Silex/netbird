package internal

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/netip"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/pion/ice/v3"
	"github.com/pion/stun/v2"
	log "github.com/sirupsen/logrus"
	"golang.zx2c4.com/wireguard/tun/netstack"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"google.golang.org/protobuf/proto"

	nberrors "github.com/netbirdio/netbird/client/errors"
	"github.com/netbirdio/netbird/client/firewall"
	firewallManager "github.com/netbirdio/netbird/client/firewall/manager"
	"github.com/netbirdio/netbird/client/iface"
	"github.com/netbirdio/netbird/client/iface/bind"
	"github.com/netbirdio/netbird/client/iface/device"
	nbnetstack "github.com/netbirdio/netbird/client/iface/netstack"
	"github.com/netbirdio/netbird/client/internal/acl"
	"github.com/netbirdio/netbird/client/internal/dns"
	"github.com/netbirdio/netbird/client/internal/dnsfwd"
	"github.com/netbirdio/netbird/client/internal/ingressgw"
	"github.com/netbirdio/netbird/client/internal/netflow"
	nftypes "github.com/netbirdio/netbird/client/internal/netflow/types"
	"github.com/netbirdio/netbird/client/internal/networkmonitor"
	"github.com/netbirdio/netbird/client/internal/peer"
	"github.com/netbirdio/netbird/client/internal/peer/guard"
	icemaker "github.com/netbirdio/netbird/client/internal/peer/ice"
	"github.com/netbirdio/netbird/client/internal/peerstore"
	"github.com/netbirdio/netbird/client/internal/relay"
	"github.com/netbirdio/netbird/client/internal/rosenpass"
	"github.com/netbirdio/netbird/client/internal/routemanager"
	"github.com/netbirdio/netbird/client/internal/routemanager/systemops"
	"github.com/netbirdio/netbird/client/internal/statemanager"
	cProto "github.com/netbirdio/netbird/client/proto"
	"github.com/netbirdio/netbird/management/domain"
	semaphoregroup "github.com/netbirdio/netbird/util/semaphore-group"

	nbssh "github.com/netbirdio/netbird/client/ssh"
	"github.com/netbirdio/netbird/client/system"
	nbdns "github.com/netbirdio/netbird/dns"
	mgm "github.com/netbirdio/netbird/management/client"
	mgmProto "github.com/netbirdio/netbird/management/proto"
	auth "github.com/netbirdio/netbird/relay/auth/hmac"
	relayClient "github.com/netbirdio/netbird/relay/client"
	"github.com/netbirdio/netbird/route"
	signal "github.com/netbirdio/netbird/signal/client"
	sProto "github.com/netbirdio/netbird/signal/proto"
	"github.com/netbirdio/netbird/util"
)

// PeerConnectionTimeoutMax is a timeout of an initial connection attempt to a remote peer.
// E.g. this peer will wait PeerConnectionTimeoutMax for the remote peer to respond,
// if not successful then it will retry the connection attempt.
// Todo pass timeout at EnginConfig
const (
	PeerConnectionTimeoutMax = 45000 // ms
	PeerConnectionTimeoutMin = 30000 // ms
	connInitLimit            = 200
)

var ErrResetConnection = fmt.Errorf("reset connection")

// EngineConfig is a config for the Engine
type EngineConfig struct {
	WgPort      int
	WgIfaceName string

	// WgAddr is a Wireguard local address (Netbird Network IP)
	WgAddr string

	// WgPrivateKey is a Wireguard private key of our peer (it MUST never leave the machine)
	WgPrivateKey wgtypes.Key

	// NetworkMonitor is a flag to enable network monitoring
	NetworkMonitor bool

	// IFaceBlackList is a list of network interfaces to ignore when discovering connection candidates (ICE related)
	IFaceBlackList       []string
	DisableIPv6Discovery bool

	PreSharedKey *wgtypes.Key

	// UDPMuxPort default value 0 - the system will pick an available port
	UDPMuxPort int

	// UDPMuxSrflxPort default value 0 - the system will pick an available port
	UDPMuxSrflxPort int

	// SSHKey is a private SSH key in a PEM format
	SSHKey []byte

	NATExternalIPs []string

	CustomDNSAddress string

	RosenpassEnabled    bool
	RosenpassPermissive bool

	ServerSSHAllowed bool

	DNSRouteInterval time.Duration

	DisableClientRoutes bool
	DisableServerRoutes bool
	DisableDNS          bool
	DisableFirewall     bool
	BlockLANAccess      bool
	BlockInbound        bool

	LazyConnectionEnabled bool
}

// Engine is a mechanism responsible for reacting on Signal and Management stream events and managing connections to the remote peers.
type Engine struct {
	// signal is a Signal Service client
	signal   signal.Client
	signaler *peer.Signaler
	// mgmClient is a Management Service client
	mgmClient mgm.Client
	// peerConns is a map that holds all the peers that are known to this peer
	peerStore *peerstore.Store

	connMgr *ConnMgr

	// rpManager is a Rosenpass manager
	rpManager *rosenpass.Manager

	// syncMsgMux is used to guarantee sequential Management Service message processing
	syncMsgMux *sync.Mutex

	config    *EngineConfig
	mobileDep MobileDependency

	// STUNs is a list of STUN servers used by ICE
	STUNs []*stun.URI
	// TURNs is a list of STUN servers used by ICE
	TURNs    []*stun.URI
	stunTurn icemaker.StunTurn

	clientCtx    context.Context
	clientCancel context.CancelFunc

	ctx    context.Context
	cancel context.CancelFunc

	wgInterface WGIface

	udpMux *bind.UniversalUDPMuxDefault

	// networkSerial is the latest CurrentSerial (state ID) of the network sent by the Management service
	networkSerial uint64

	networkMonitor *networkmonitor.NetworkMonitor

	sshServerFunc func(hostKeyPEM []byte, addr string) (nbssh.Server, error)
	sshServer     nbssh.Server

	statusRecorder *peer.Status

	firewall          firewallManager.Manager
	routeManager      routemanager.Manager
	acl               acl.Manager
	dnsForwardMgr     *dnsfwd.Manager
	ingressGatewayMgr *ingressgw.Manager

	dnsServer dns.Server

	// checks are the client-applied posture checks that need to be evaluated on the client
	checks []*mgmProto.Checks

	relayManager *relayClient.Manager
	stateManager *statemanager.Manager
	srWatcher    *guard.SRWatcher

	// Network map persistence
	persistNetworkMap bool
	latestNetworkMap  *mgmProto.NetworkMap
	connSemaphore     *semaphoregroup.SemaphoreGroup
	flowManager       nftypes.FlowManager
}

// Peer is an instance of the Connection Peer
type Peer struct {
	WgPubKey     string
	WgAllowedIps string
}

type localIpUpdater interface {
	UpdateLocalIPs() error
}

// NewEngine creates a new Connection Engine with probes attached
func NewEngine(
	clientCtx context.Context,
	clientCancel context.CancelFunc,
	signalClient signal.Client,
	mgmClient mgm.Client,
	relayManager *relayClient.Manager,
	config *EngineConfig,
	mobileDep MobileDependency,
	statusRecorder *peer.Status,
	checks []*mgmProto.Checks,
) *Engine {
	engine := &Engine{
		clientCtx:      clientCtx,
		clientCancel:   clientCancel,
		signal:         signalClient,
		signaler:       peer.NewSignaler(signalClient, config.WgPrivateKey),
		mgmClient:      mgmClient,
		relayManager:   relayManager,
		peerStore:      peerstore.NewConnStore(),
		syncMsgMux:     &sync.Mutex{},
		config:         config,
		mobileDep:      mobileDep,
		STUNs:          []*stun.URI{},
		TURNs:          []*stun.URI{},
		networkSerial:  0,
		sshServerFunc:  nbssh.DefaultSSHServer,
		statusRecorder: statusRecorder,
		checks:         checks,
		connSemaphore:  semaphoregroup.NewSemaphoreGroup(connInitLimit),
	}

	path := statemanager.GetDefaultStatePath()
	if runtime.GOOS == "ios" {
		if !fileExists(mobileDep.StateFilePath) {
			err := createFile(mobileDep.StateFilePath)
			if err != nil {
				log.Errorf("failed to create state file: %v", err)
				// we are not exiting as we can run without the state manager
			}
		}

		path = mobileDep.StateFilePath
	}
	engine.stateManager = statemanager.New(path)

	return engine
}

func (e *Engine) Stop() error {
	if e == nil {
		// this seems to be a very odd case but there was the possibility if the netbird down command comes before the engine is fully started
		log.Debugf("tried stopping engine that is nil")
		return nil
	}
	e.syncMsgMux.Lock()
	defer e.syncMsgMux.Unlock()

	if e.connMgr != nil {
		e.connMgr.Close()
	}

	// stopping network monitor first to avoid starting the engine again
	if e.networkMonitor != nil {
		e.networkMonitor.Stop()
	}
	log.Info("Network monitor: stopped")

	// stop/restore DNS first so dbus and friends don't complain because of a missing interface
	e.stopDNSServer()

	if e.ingressGatewayMgr != nil {
		if err := e.ingressGatewayMgr.Close(); err != nil {
			log.Warnf("failed to cleanup forward rules: %v", err)
		}
		e.ingressGatewayMgr = nil
	}

	if e.routeManager != nil {
		e.routeManager.Stop(e.stateManager)
	}

	if e.dnsForwardMgr != nil {
		if err := e.dnsForwardMgr.Stop(context.Background()); err != nil {
			log.Errorf("failed to stop DNS forward: %v", err)
		}
		e.dnsForwardMgr = nil
	}

	if e.srWatcher != nil {
		e.srWatcher.Close()
	}

	e.statusRecorder.ReplaceOfflinePeers([]peer.State{})
	e.statusRecorder.UpdateDNSStates([]peer.NSGroupState{})
	e.statusRecorder.UpdateRelayStates([]relay.ProbeResult{})

	if err := e.removeAllPeers(); err != nil {
		return fmt.Errorf("failed to remove all peers: %s", err)
	}

	if e.cancel != nil {
		e.cancel()
	}

	// very ugly but we want to remove peers from the WireGuard interface first before removing interface.
	// Removing peers happens in the conn.Close() asynchronously
	time.Sleep(500 * time.Millisecond)

	e.close()

	// stop flow manager after wg interface is gone
	if e.flowManager != nil {
		e.flowManager.Close()
	}

	log.Infof("stopped Netbird Engine")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := e.stateManager.Stop(ctx); err != nil {
		return fmt.Errorf("failed to stop state manager: %w", err)
	}
	if err := e.stateManager.PersistState(context.Background()); err != nil {
		log.Errorf("failed to persist state: %v", err)
	}

	return nil
}

// Start creates a new WireGuard tunnel interface and listens to events from Signal and Management services
// Connections to remote peers are not established here.
// However, they will be established once an event with a list of peers to connect to will be received from Management Service
func (e *Engine) Start() error {
	e.syncMsgMux.Lock()
	defer e.syncMsgMux.Unlock()

	if e.cancel != nil {
		e.cancel()
	}
	e.ctx, e.cancel = context.WithCancel(e.clientCtx)

	wgIface, err := e.newWgIface()
	if err != nil {
		log.Errorf("failed creating wireguard interface instance %s: [%s]", e.config.WgIfaceName, err)
		return fmt.Errorf("new wg interface: %w", err)
	}
	e.wgInterface = wgIface
	e.statusRecorder.SetWgIface(wgIface)

	// start flow manager right after interface creation
	publicKey := e.config.WgPrivateKey.PublicKey()
	e.flowManager = netflow.NewManager(e.wgInterface, publicKey[:], e.statusRecorder)

	if e.config.RosenpassEnabled {
		log.Infof("rosenpass is enabled")
		if e.config.RosenpassPermissive {
			log.Infof("running rosenpass in permissive mode")
		} else {
			log.Infof("running rosenpass in strict mode")
		}
		e.rpManager, err = rosenpass.NewManager(e.config.PreSharedKey, e.config.WgIfaceName)
		if err != nil {
			return fmt.Errorf("create rosenpass manager: %w", err)
		}
		err := e.rpManager.Run()
		if err != nil {
			return fmt.Errorf("run rosenpass manager: %w", err)
		}
	}
	e.stateManager.Start()

	initialRoutes, dnsConfig, dnsFeatureFlag, err := e.readInitialSettings()
	if err != nil {
		e.close()
		return fmt.Errorf("read initial settings: %w", err)
	}

	dnsServer, err := e.newDnsServer(dnsConfig)
	if err != nil {
		e.close()
		return fmt.Errorf("create dns server: %w", err)
	}
	e.dnsServer = dnsServer

	e.routeManager = routemanager.NewManager(routemanager.ManagerConfig{
		Context:             e.ctx,
		PublicKey:           e.config.WgPrivateKey.PublicKey().String(),
		DNSRouteInterval:    e.config.DNSRouteInterval,
		WGInterface:         e.wgInterface,
		StatusRecorder:      e.statusRecorder,
		RelayManager:        e.relayManager,
		InitialRoutes:       initialRoutes,
		StateManager:        e.stateManager,
		DNSServer:           dnsServer,
		DNSFeatureFlag:      dnsFeatureFlag,
		PeerStore:           e.peerStore,
		DisableClientRoutes: e.config.DisableClientRoutes,
		DisableServerRoutes: e.config.DisableServerRoutes,
	})
	if err := e.routeManager.Init(); err != nil {
		log.Errorf("Failed to initialize route manager: %s", err)
	}

	e.routeManager.SetRouteChangeListener(e.mobileDep.NetworkChangeListener)

	if err = e.wgInterfaceCreate(); err != nil {
		log.Errorf("failed creating tunnel interface %s: [%s]", e.config.WgIfaceName, err.Error())
		e.close()
		return fmt.Errorf("create wg interface: %w", err)
	}

	if err := e.createFirewall(); err != nil {
		return err
	}

	e.udpMux, err = e.wgInterface.Up()
	if err != nil {
		log.Errorf("failed to pull up wgInterface [%s]: %s", e.wgInterface.Name(), err.Error())
		e.close()
		return fmt.Errorf("up wg interface: %w", err)
	}

	// if inbound conns are blocked there is no need to create the ACL manager
	if e.firewall != nil && !e.config.BlockInbound {
		e.acl = acl.NewDefaultManager(e.firewall)
	}

	err = e.dnsServer.Initialize()
	if err != nil {
		e.close()
		return fmt.Errorf("initialize dns server: %w", err)
	}

	iceCfg := icemaker.Config{
		StunTurn:             &e.stunTurn,
		InterfaceBlackList:   e.config.IFaceBlackList,
		DisableIPv6Discovery: e.config.DisableIPv6Discovery,
		UDPMux:               e.udpMux.UDPMuxDefault,
		UDPMuxSrflx:          e.udpMux,
		NATExternalIPs:       e.parseNATExternalIPMappings(),
	}

	e.connMgr = NewConnMgr(e.config, e.statusRecorder, e.peerStore, wgIface)
	e.connMgr.Start(e.ctx)

	e.srWatcher = guard.NewSRWatcher(e.signal, e.relayManager, e.mobileDep.IFaceDiscover, iceCfg)
	e.srWatcher.Start()

	e.receiveSignalEvents()
	e.receiveManagementEvents()

	// starting network monitor at the very last to avoid disruptions
	e.startNetworkMonitor()
	return nil
}

func (e *Engine) createFirewall() error {
	if e.config.DisableFirewall {
		log.Infof("firewall is disabled")
		return nil
	}

	var err error
	e.firewall, err = firewall.NewFirewall(e.wgInterface, e.stateManager, e.flowManager.GetLogger(), e.config.DisableServerRoutes)
	if err != nil || e.firewall == nil {
		log.Errorf("failed creating firewall manager: %s", err)
		return nil
	}

	if err := e.initFirewall(); err != nil {
		return err
	}

	return nil
}

func (e *Engine) initFirewall() error {
	if err := e.routeManager.SetFirewall(e.firewall); err != nil {
		e.close()
		return fmt.Errorf("set firewall: %w", err)
	}

	if e.config.BlockLANAccess {
		e.blockLanAccess()
	}

	if e.rpManager == nil || !e.config.RosenpassEnabled {
		return nil
	}

	rosenpassPort := e.rpManager.GetAddress().Port
	port := firewallManager.Port{Values: []uint16{uint16(rosenpassPort)}}

	// this rule is static and will be torn down on engine down by the firewall manager
	if _, err := e.firewall.AddPeerFiltering(
		nil,
		net.IP{0, 0, 0, 0},
		firewallManager.ProtocolUDP,
		nil,
		&port,
		firewallManager.ActionAccept,
		"",
	); err != nil {
		log.Errorf("failed to allow rosenpass interface traffic: %v", err)
		return nil
	}

	log.Infof("rosenpass interface traffic allowed on port %d", rosenpassPort)

	return nil
}

func (e *Engine) blockLanAccess() {
	if e.config.BlockInbound {
		// no need to set up extra deny rules if inbound is already blocked in general
		return
	}

	var merr *multierror.Error

	// TODO: keep this updated
	toBlock, err := getInterfacePrefixes()
	if err != nil {
		merr = multierror.Append(merr, fmt.Errorf("get local addresses: %w", err))
	}

	log.Infof("blocking route LAN access for networks: %v", toBlock)
	v4 := netip.PrefixFrom(netip.IPv4Unspecified(), 0)
	for _, network := range toBlock {
		if _, err := e.firewall.AddRouteFiltering(
			nil,
			[]netip.Prefix{v4},
			firewallManager.Network{Prefix: network},
			firewallManager.ProtocolALL,
			nil,
			nil,
			firewallManager.ActionDrop,
		); err != nil {
			merr = multierror.Append(merr, fmt.Errorf("add fw rule for network %s: %w", network, err))
		}
	}

	if merr != nil {
		log.Warnf("encountered errors blocking IPs to block LAN access: %v", nberrors.FormatErrorOrNil(merr))
	}
}

// modifyPeers updates peers that have been modified (e.g. IP address has been changed).
// It closes the existing connection, removes it from the peerConns map, and creates a new one.
func (e *Engine) modifyPeers(peersUpdate []*mgmProto.RemotePeerConfig) error {

	// first, check if peers have been modified
	var modified []*mgmProto.RemotePeerConfig
	for _, p := range peersUpdate {
		peerPubKey := p.GetWgPubKey()
		currentPeer, ok := e.peerStore.PeerConn(peerPubKey)
		if !ok {
			continue
		}

		if currentPeer.AgentVersionString() != p.AgentVersion {
			modified = append(modified, p)
			continue
		}

		allowedIPs, ok := e.peerStore.AllowedIPs(peerPubKey)
		if !ok {
			continue
		}
		if !compareNetIPLists(allowedIPs, p.GetAllowedIps()) {
			modified = append(modified, p)
			continue
		}

		if err := e.statusRecorder.UpdatePeerFQDN(peerPubKey, p.GetFqdn()); err != nil {
			log.Warnf("error updating peer's %s fqdn in the status recorder, got error: %v", peerPubKey, err)
		}
	}

	// second, close all modified connections and remove them from the state map
	for _, p := range modified {
		err := e.removePeer(p.GetWgPubKey())
		if err != nil {
			return err
		}
	}
	// third, add the peer connections again
	for _, p := range modified {
		err := e.addNewPeer(p)
		if err != nil {
			return err
		}
	}
	return nil
}

// removePeers finds and removes peers that do not exist anymore in the network map received from the Management Service.
// It also removes peers that have been modified (e.g. change of IP address). They will be added again in addPeers method.
func (e *Engine) removePeers(peersUpdate []*mgmProto.RemotePeerConfig) error {
	newPeers := make([]string, 0, len(peersUpdate))
	for _, p := range peersUpdate {
		newPeers = append(newPeers, p.GetWgPubKey())
	}

	toRemove := util.SliceDiff(e.peerStore.PeersPubKey(), newPeers)

	for _, p := range toRemove {
		err := e.removePeer(p)
		if err != nil {
			return err
		}
		log.Infof("removed peer %s", p)
	}
	return nil
}

func (e *Engine) removeAllPeers() error {
	log.Debugf("removing all peer connections")
	for _, p := range e.peerStore.PeersPubKey() {
		err := e.removePeer(p)
		if err != nil {
			return err
		}
	}
	return nil
}

// removePeer closes an existing peer connection, removes a peer, and clears authorized key of the SSH server
func (e *Engine) removePeer(peerKey string) error {
	log.Debugf("removing peer from engine %s", peerKey)

	if !isNil(e.sshServer) {
		e.sshServer.RemoveAuthorizedKey(peerKey)
	}

	e.connMgr.RemovePeerConn(peerKey)

	err := e.statusRecorder.RemovePeer(peerKey)
	if err != nil {
		log.Warnf("received error when removing peer %s from status recorder: %v", peerKey, err)
	}
	return nil
}

func (e *Engine) handleSync(update *mgmProto.SyncResponse) error {
	e.syncMsgMux.Lock()
	defer e.syncMsgMux.Unlock()

	if update.GetNetbirdConfig() != nil {
		wCfg := update.GetNetbirdConfig()
		err := e.updateTURNs(wCfg.GetTurns())
		if err != nil {
			return fmt.Errorf("update TURNs: %w", err)
		}

		err = e.updateSTUNs(wCfg.GetStuns())
		if err != nil {
			return fmt.Errorf("update STUNs: %w", err)
		}

		var stunTurn []*stun.URI
		stunTurn = append(stunTurn, e.STUNs...)
		stunTurn = append(stunTurn, e.TURNs...)
		e.stunTurn.Store(stunTurn)

		err = e.handleRelayUpdate(wCfg.GetRelay())
		if err != nil {
			return err
		}

		err = e.handleFlowUpdate(wCfg.GetFlow())
		if err != nil {
			return fmt.Errorf("handle the flow configuration: %w", err)
		}

		// todo update signal
	}

	if err := e.updateChecksIfNew(update.Checks); err != nil {
		return err
	}

	nm := update.GetNetworkMap()
	if nm == nil {
		return nil
	}

	// Store network map if persistence is enabled
	if e.persistNetworkMap {
		e.latestNetworkMap = nm
		log.Debugf("network map persisted with serial %d", nm.GetSerial())
	}

	// only apply new changes and ignore old ones
	if err := e.updateNetworkMap(nm); err != nil {
		return err
	}

	e.statusRecorder.PublishEvent(cProto.SystemEvent_INFO, cProto.SystemEvent_SYSTEM, "Network map updated", "", nil)

	return nil
}

func (e *Engine) handleRelayUpdate(update *mgmProto.RelayConfig) error {
	if update != nil {
		// when we receive token we expect valid address list too
		c := &auth.Token{
			Payload:   update.GetTokenPayload(),
			Signature: update.GetTokenSignature(),
		}
		if err := e.relayManager.UpdateToken(c); err != nil {
			return fmt.Errorf("update relay token: %w", err)
		}

		e.relayManager.UpdateServerURLs(update.Urls)

		// Just in case the agent started with an MGM server where the relay was disabled but was later enabled.
		// We can ignore all errors because the guard will manage the reconnection retries.
		_ = e.relayManager.Serve()
	} else {
		e.relayManager.UpdateServerURLs(nil)
	}

	return nil
}

func (e *Engine) handleFlowUpdate(config *mgmProto.FlowConfig) error {
	if config == nil {
		return nil
	}

	flowConfig, err := toFlowLoggerConfig(config)
	if err != nil {
		return err
	}
	return e.flowManager.Update(flowConfig)
}

func toFlowLoggerConfig(config *mgmProto.FlowConfig) (*nftypes.FlowConfig, error) {
	if config.GetInterval() == nil {
		return nil, errors.New("flow interval is nil")
	}
	return &nftypes.FlowConfig{
		Enabled:            config.GetEnabled(),
		Counters:           config.GetCounters(),
		URL:                config.GetUrl(),
		TokenPayload:       config.GetTokenPayload(),
		TokenSignature:     config.GetTokenSignature(),
		Interval:           config.GetInterval().AsDuration(),
		DNSCollection:      config.GetDnsCollection(),
		ExitNodeCollection: config.GetExitNodeCollection(),
	}, nil
}

// updateChecksIfNew updates checks if there are changes and sync new meta with management
func (e *Engine) updateChecksIfNew(checks []*mgmProto.Checks) error {
	// if checks are equal, we skip the update
	if isChecksEqual(e.checks, checks) {
		return nil
	}
	e.checks = checks

	info, err := system.GetInfoWithChecks(e.ctx, checks)
	if err != nil {
		log.Warnf("failed to get system info with checks: %v", err)
		info = system.GetInfo(e.ctx)
	}
	info.SetFlags(
		e.config.RosenpassEnabled,
		e.config.RosenpassPermissive,
		&e.config.ServerSSHAllowed,
		e.config.DisableClientRoutes,
		e.config.DisableServerRoutes,
		e.config.DisableDNS,
		e.config.DisableFirewall,
		e.config.BlockLANAccess,
		e.config.BlockInbound,
		e.config.LazyConnectionEnabled,
	)

	if err := e.mgmClient.SyncMeta(info); err != nil {
		log.Errorf("could not sync meta: error %s", err)
		return err
	}
	return nil
}

func isNil(server nbssh.Server) bool {
	return server == nil || reflect.ValueOf(server).IsNil()
}

func (e *Engine) updateSSH(sshConf *mgmProto.SSHConfig) error {
	if e.config.BlockInbound {
		log.Infof("SSH server is disabled because inbound connections are blocked")
		return nil
	}

	if !e.config.ServerSSHAllowed {
		log.Info("SSH server is not enabled")
		return nil
	}

	if sshConf.GetSshEnabled() {
		if runtime.GOOS == "windows" {
			log.Warnf("running SSH server on %s is not supported", runtime.GOOS)
			return nil
		}
		// start SSH server if it wasn't running
		if isNil(e.sshServer) {
			listenAddr := fmt.Sprintf("%s:%d", e.wgInterface.Address().IP.String(), nbssh.DefaultSSHPort)
			if nbnetstack.IsEnabled() {
				listenAddr = fmt.Sprintf("127.0.0.1:%d", nbssh.DefaultSSHPort)
			}
			// nil sshServer means it has not yet been started
			var err error
			e.sshServer, err = e.sshServerFunc(e.config.SSHKey, listenAddr)

			if err != nil {
				return fmt.Errorf("create ssh server: %w", err)
			}
			go func() {
				// blocking
				err = e.sshServer.Start()
				if err != nil {
					// will throw error when we stop it even if it is a graceful stop
					log.Debugf("stopped SSH server with error %v", err)
				}
				e.syncMsgMux.Lock()
				defer e.syncMsgMux.Unlock()
				e.sshServer = nil
				log.Infof("stopped SSH server")
			}()
		} else {
			log.Debugf("SSH server is already running")
		}
	} else if !isNil(e.sshServer) {
		// Disable SSH server request, so stop it if it was running
		err := e.sshServer.Stop()
		if err != nil {
			log.Warnf("failed to stop SSH server %v", err)
		}
		e.sshServer = nil
	}
	return nil
}

func (e *Engine) updateConfig(conf *mgmProto.PeerConfig) error {
	if e.wgInterface == nil {
		return errors.New("wireguard interface is not initialized")
	}

	if e.wgInterface.Address().String() != conf.Address {
		oldAddr := e.wgInterface.Address().String()
		log.Debugf("updating peer address from %s to %s", oldAddr, conf.Address)
		err := e.wgInterface.UpdateAddr(conf.Address)
		if err != nil {
			return err
		}
		e.config.WgAddr = conf.Address
		log.Infof("updated peer address from %s to %s", oldAddr, conf.Address)
	}

	if conf.GetSshConfig() != nil {
		err := e.updateSSH(conf.GetSshConfig())
		if err != nil {
			log.Warnf("failed handling SSH server setup: %v", err)
		}
	}

	state := e.statusRecorder.GetLocalPeerState()
	state.IP = e.config.WgAddr
	state.PubKey = e.config.WgPrivateKey.PublicKey().String()
	state.KernelInterface = device.WireGuardModuleIsLoaded()
	state.FQDN = conf.GetFqdn()

	e.statusRecorder.UpdateLocalPeerState(state)

	return nil
}

// receiveManagementEvents connects to the Management Service event stream to receive updates from the management service
// E.g. when a new peer has been registered and we are allowed to connect to it.
func (e *Engine) receiveManagementEvents() {
	go func() {
		info, err := system.GetInfoWithChecks(e.ctx, e.checks)
		if err != nil {
			log.Warnf("failed to get system info with checks: %v", err)
			info = system.GetInfo(e.ctx)
		}
		info.SetFlags(
			e.config.RosenpassEnabled,
			e.config.RosenpassPermissive,
			&e.config.ServerSSHAllowed,
			e.config.DisableClientRoutes,
			e.config.DisableServerRoutes,
			e.config.DisableDNS,
			e.config.DisableFirewall,
			e.config.BlockLANAccess,
			e.config.BlockInbound,
			e.config.LazyConnectionEnabled,
		)

		// err = e.mgmClient.Sync(info, e.handleSync)
		err = e.mgmClient.Sync(e.ctx, info, e.handleSync)
		if err != nil {
			// happens if management is unavailable for a long time.
			// We want to cancel the operation of the whole client
			_ = CtxGetState(e.ctx).Wrap(ErrResetConnection)
			e.clientCancel()
			return
		}
		log.Debugf("stopped receiving updates from Management Service")
	}()
	log.Debugf("connecting to Management Service updates stream")
}

func (e *Engine) updateSTUNs(stuns []*mgmProto.HostConfig) error {
	if len(stuns) == 0 {
		return nil
	}
	var newSTUNs []*stun.URI
	log.Debugf("got STUNs update from Management Service, updating")
	for _, s := range stuns {
		url, err := stun.ParseURI(s.Uri)
		if err != nil {
			return err
		}
		newSTUNs = append(newSTUNs, url)
	}
	e.STUNs = newSTUNs

	return nil
}

func (e *Engine) updateTURNs(turns []*mgmProto.ProtectedHostConfig) error {
	if len(turns) == 0 {
		return nil
	}
	var newTURNs []*stun.URI
	log.Debugf("got TURNs update from Management Service, updating")
	for _, turn := range turns {
		url, err := stun.ParseURI(turn.HostConfig.Uri)
		if err != nil {
			return err
		}
		url.Username = turn.User
		url.Password = turn.Password
		newTURNs = append(newTURNs, url)
	}
	e.TURNs = newTURNs

	return nil
}

func (e *Engine) updateNetworkMap(networkMap *mgmProto.NetworkMap) error {
	// intentionally leave it before checking serial because for now it can happen that peer IP changed but serial didn't
	if networkMap.GetPeerConfig() != nil {
		err := e.updateConfig(networkMap.GetPeerConfig())
		if err != nil {
			return err
		}
	}

	serial := networkMap.GetSerial()
	if e.networkSerial > serial {
		log.Debugf("received outdated NetworkMap with serial %d, ignoring", serial)
		return nil
	}

	if err := e.connMgr.UpdatedRemoteFeatureFlag(e.ctx, networkMap.GetPeerConfig().GetLazyConnectionEnabled()); err != nil {
		log.Errorf("failed to update lazy connection feature flag: %v", err)
	}

	if e.firewall != nil {
		if localipfw, ok := e.firewall.(localIpUpdater); ok {
			if err := localipfw.UpdateLocalIPs(); err != nil {
				log.Errorf("failed to update local IPs: %v", err)
			}
		}

		// If we got empty rules list but management did not set the networkMap.FirewallRulesIsEmpty flag,
		// then the mgmt server is older than the client, and we need to allow all traffic for routes.
		// This needs to be toggled before applying routes.
		isLegacy := len(networkMap.RoutesFirewallRules) == 0 && !networkMap.RoutesFirewallRulesIsEmpty
		if err := e.firewall.SetLegacyManagement(isLegacy); err != nil {
			log.Errorf("failed to set legacy management flag: %v", err)
		}
	}

	protoDNSConfig := networkMap.GetDNSConfig()
	if protoDNSConfig == nil {
		protoDNSConfig = &mgmProto.DNSConfig{}
	}

	if err := e.dnsServer.UpdateDNSServer(serial, toDNSConfig(protoDNSConfig, e.wgInterface.Address().Network)); err != nil {
		log.Errorf("failed to update dns server, err: %v", err)
	}

	// apply routes first, route related actions might depend on routing being enabled
	routes := toRoutes(networkMap.GetRoutes())
	serverRoutes, clientRoutes := e.routeManager.ClassifyRoutes(routes)

	// lazy mgr needs to be aware of which routes are available before they are applied
	if e.connMgr != nil {
		e.connMgr.UpdateRouteHAMap(clientRoutes)
		log.Debugf("updated lazy connection manager with %d HA groups", len(clientRoutes))
	}

	dnsRouteFeatureFlag := toDNSFeatureFlag(networkMap)
	if err := e.routeManager.UpdateRoutes(serial, serverRoutes, clientRoutes, dnsRouteFeatureFlag); err != nil {
		log.Errorf("failed to update routes: %v", err)
	}

	if e.acl != nil {
		e.acl.ApplyFiltering(networkMap, dnsRouteFeatureFlag)
	}

	fwdEntries := toRouteDomains(e.config.WgPrivateKey.PublicKey().String(), routes)
	e.updateDNSForwarder(dnsRouteFeatureFlag, fwdEntries)

	// Ingress forward rules
	forwardingRules, err := e.updateForwardRules(networkMap.GetForwardingRules())
	if err != nil {
		log.Errorf("failed to update forward rules, err: %v", err)
	}

	log.Debugf("got peers update from Management Service, total peers to connect to = %d", len(networkMap.GetRemotePeers()))

	e.updateOfflinePeers(networkMap.GetOfflinePeers())

	// cleanup request, most likely our peer has been deleted
	if networkMap.GetRemotePeersIsEmpty() {
		err := e.removeAllPeers()
		e.statusRecorder.FinishPeerListModifications()
		if err != nil {
			return err
		}
	} else {
		err := e.removePeers(networkMap.GetRemotePeers())
		if err != nil {
			return err
		}

		err = e.modifyPeers(networkMap.GetRemotePeers())
		if err != nil {
			return err
		}

		err = e.addNewPeers(networkMap.GetRemotePeers())
		if err != nil {
			return err
		}

		e.statusRecorder.FinishPeerListModifications()

		// update SSHServer by adding remote peer SSH keys
		if !isNil(e.sshServer) {
			for _, config := range networkMap.GetRemotePeers() {
				if config.GetSshConfig() != nil && config.GetSshConfig().GetSshPubKey() != nil {
					err := e.sshServer.AddAuthorizedKey(config.WgPubKey, string(config.GetSshConfig().GetSshPubKey()))
					if err != nil {
						log.Warnf("failed adding authorized key to SSH DefaultServer %v", err)
					}
				}
			}
		}
	}

	// must set the exclude list after the peers are added. Without it the manager can not figure out the peers parameters from the store
	excludedLazyPeers := e.toExcludedLazyPeers(forwardingRules, networkMap.GetRemotePeers())
	e.connMgr.SetExcludeList(e.ctx, excludedLazyPeers)

	e.networkSerial = serial

	// Test received (upstream) servers for availability right away instead of upon usage.
	// If no server of a server group responds this will disable the respective handler and retry later.
	e.dnsServer.ProbeAvailability()

	return nil
}

func toDNSFeatureFlag(networkMap *mgmProto.NetworkMap) bool {
	if networkMap.PeerConfig != nil {
		return networkMap.PeerConfig.RoutingPeerDnsResolutionEnabled
	}
	return false
}

func toRoutes(protoRoutes []*mgmProto.Route) []*route.Route {
	if protoRoutes == nil {
		protoRoutes = []*mgmProto.Route{}
	}

	routes := make([]*route.Route, 0)
	for _, protoRoute := range protoRoutes {
		var prefix netip.Prefix
		if len(protoRoute.Domains) == 0 {
			var err error
			if prefix, err = netip.ParsePrefix(protoRoute.Network); err != nil {
				log.Errorf("Failed to parse prefix %s: %v", protoRoute.Network, err)
				continue
			}
		}

		convertedRoute := &route.Route{
			ID:          route.ID(protoRoute.ID),
			Network:     prefix.Masked(),
			Domains:     domain.FromPunycodeList(protoRoute.Domains),
			NetID:       route.NetID(protoRoute.NetID),
			NetworkType: route.NetworkType(protoRoute.NetworkType),
			Peer:        protoRoute.Peer,
			Metric:      int(protoRoute.Metric),
			Masquerade:  protoRoute.Masquerade,
			KeepRoute:   protoRoute.KeepRoute,
		}
		routes = append(routes, convertedRoute)
	}
	return routes
}

func toRouteDomains(myPubKey string, routes []*route.Route) []*dnsfwd.ForwarderEntry {
	var entries []*dnsfwd.ForwarderEntry
	for _, route := range routes {
		if len(route.Domains) == 0 {
			continue
		}
		if route.Peer == myPubKey {
			domainSet := firewallManager.NewDomainSet(route.Domains)
			for _, d := range route.Domains {
				entries = append(entries, &dnsfwd.ForwarderEntry{
					Domain: d,
					Set:    domainSet,
					ResID:  route.GetResourceID(),
				})
			}
		}
	}
	return entries
}

func toDNSConfig(protoDNSConfig *mgmProto.DNSConfig, network netip.Prefix) nbdns.Config {
	dnsUpdate := nbdns.Config{
		ServiceEnable:    protoDNSConfig.GetServiceEnable(),
		CustomZones:      make([]nbdns.CustomZone, 0),
		NameServerGroups: make([]*nbdns.NameServerGroup, 0),
	}

	for _, zone := range protoDNSConfig.GetCustomZones() {
		dnsZone := nbdns.CustomZone{
			Domain: zone.GetDomain(),
		}
		for _, record := range zone.Records {
			dnsRecord := nbdns.SimpleRecord{
				Name:  record.GetName(),
				Type:  int(record.GetType()),
				Class: record.GetClass(),
				TTL:   int(record.GetTTL()),
				RData: record.GetRData(),
			}
			dnsZone.Records = append(dnsZone.Records, dnsRecord)
		}
		dnsUpdate.CustomZones = append(dnsUpdate.CustomZones, dnsZone)
	}

	for _, nsGroup := range protoDNSConfig.GetNameServerGroups() {
		dnsNSGroup := &nbdns.NameServerGroup{
			Primary:              nsGroup.GetPrimary(),
			Domains:              nsGroup.GetDomains(),
			SearchDomainsEnabled: nsGroup.GetSearchDomainsEnabled(),
		}
		for _, ns := range nsGroup.GetNameServers() {
			dnsNS := nbdns.NameServer{
				IP:     netip.MustParseAddr(ns.GetIP()),
				NSType: nbdns.NameServerType(ns.GetNSType()),
				Port:   int(ns.GetPort()),
			}
			dnsNSGroup.NameServers = append(dnsNSGroup.NameServers, dnsNS)
		}
		dnsUpdate.NameServerGroups = append(dnsUpdate.NameServerGroups, dnsNSGroup)
	}

	if len(dnsUpdate.CustomZones) > 0 {
		addReverseZone(&dnsUpdate, network)
	}

	return dnsUpdate
}

func (e *Engine) updateOfflinePeers(offlinePeers []*mgmProto.RemotePeerConfig) {
	replacement := make([]peer.State, len(offlinePeers))
	for i, offlinePeer := range offlinePeers {
		log.Debugf("added offline peer %s", offlinePeer.Fqdn)
		replacement[i] = peer.State{
			IP:               strings.Join(offlinePeer.GetAllowedIps(), ","),
			PubKey:           offlinePeer.GetWgPubKey(),
			FQDN:             offlinePeer.GetFqdn(),
			ConnStatus:       peer.StatusIdle,
			ConnStatusUpdate: time.Now(),
			Mux:              new(sync.RWMutex),
		}
	}
	e.statusRecorder.ReplaceOfflinePeers(replacement)
}

// addNewPeers adds peers that were not know before but arrived from the Management service with the update
func (e *Engine) addNewPeers(peersUpdate []*mgmProto.RemotePeerConfig) error {
	for _, p := range peersUpdate {
		err := e.addNewPeer(p)
		if err != nil {
			return err
		}
	}
	return nil
}

// addNewPeer add peer if connection doesn't exist
func (e *Engine) addNewPeer(peerConfig *mgmProto.RemotePeerConfig) error {
	peerKey := peerConfig.GetWgPubKey()
	peerIPs := make([]netip.Prefix, 0, len(peerConfig.GetAllowedIps()))
	if _, ok := e.peerStore.PeerConn(peerKey); ok {
		return nil
	}

	for _, ipString := range peerConfig.GetAllowedIps() {
		allowedNetIP, err := netip.ParsePrefix(ipString)
		if err != nil {
			log.Errorf("failed to parse allowedIPS: %v", err)
			return err
		}
		peerIPs = append(peerIPs, allowedNetIP)
	}

	conn, err := e.createPeerConn(peerKey, peerIPs, peerConfig.AgentVersion)
	if err != nil {
		return fmt.Errorf("create peer connection: %w", err)
	}

	err = e.statusRecorder.AddPeer(peerKey, peerConfig.Fqdn, peerIPs[0].Addr().String())
	if err != nil {
		log.Warnf("error adding peer %s to status recorder, got error: %v", peerKey, err)
	}

	if exists := e.connMgr.AddPeerConn(e.ctx, peerKey, conn); exists {
		conn.Close(false)
		return fmt.Errorf("peer already exists: %s", peerKey)
	}

	return nil
}

func (e *Engine) createPeerConn(pubKey string, allowedIPs []netip.Prefix, agentVersion string) (*peer.Conn, error) {
	log.Debugf("creating peer connection %s", pubKey)

	wgConfig := peer.WgConfig{
		RemoteKey:    pubKey,
		WgListenPort: e.config.WgPort,
		WgInterface:  e.wgInterface,
		AllowedIps:   allowedIPs,
		PreSharedKey: e.config.PreSharedKey,
	}

	// randomize connection timeout
	timeout := time.Duration(rand.Intn(PeerConnectionTimeoutMax-PeerConnectionTimeoutMin)+PeerConnectionTimeoutMin) * time.Millisecond
	config := peer.ConnConfig{
		Key:          pubKey,
		LocalKey:     e.config.WgPrivateKey.PublicKey().String(),
		AgentVersion: agentVersion,
		Timeout:      timeout,
		WgConfig:     wgConfig,
		LocalWgPort:  e.config.WgPort,
		RosenpassConfig: peer.RosenpassConfig{
			PubKey:         e.getRosenpassPubKey(),
			Addr:           e.getRosenpassAddr(),
			PermissiveMode: e.config.RosenpassPermissive,
		},
		ICEConfig: icemaker.Config{
			StunTurn:             &e.stunTurn,
			InterfaceBlackList:   e.config.IFaceBlackList,
			DisableIPv6Discovery: e.config.DisableIPv6Discovery,
			UDPMux:               e.udpMux.UDPMuxDefault,
			UDPMuxSrflx:          e.udpMux,
			NATExternalIPs:       e.parseNATExternalIPMappings(),
		},
	}

	serviceDependencies := peer.ServiceDependencies{
		StatusRecorder: e.statusRecorder,
		Signaler:       e.signaler,
		IFaceDiscover:  e.mobileDep.IFaceDiscover,
		RelayManager:   e.relayManager,
		SrWatcher:      e.srWatcher,
		Semaphore:      e.connSemaphore,
	}
	peerConn, err := peer.NewConn(config, serviceDependencies)
	if err != nil {
		return nil, err
	}

	if e.rpManager != nil {
		peerConn.SetOnConnected(e.rpManager.OnConnected)
		peerConn.SetOnDisconnected(e.rpManager.OnDisconnected)
	}

	return peerConn, nil
}

// receiveSignalEvents connects to the Signal Service event stream to negotiate connection with remote peers
func (e *Engine) receiveSignalEvents() {
	go func() {
		// connect to a stream of messages coming from the signal server
		err := e.signal.Receive(e.ctx, func(msg *sProto.Message) error {
			e.syncMsgMux.Lock()
			defer e.syncMsgMux.Unlock()

			conn, ok := e.peerStore.PeerConn(msg.Key)
			if !ok {
				return fmt.Errorf("wrongly addressed message %s", msg.Key)
			}

			msgType := msg.GetBody().GetType()
			if msgType != sProto.Body_GO_IDLE {
				e.connMgr.ActivatePeer(e.ctx, conn)
			}

			switch msg.GetBody().Type {
			case sProto.Body_OFFER:
				remoteCred, err := signal.UnMarshalCredential(msg)
				if err != nil {
					return err
				}

				var rosenpassPubKey []byte
				rosenpassAddr := ""
				if msg.GetBody().GetRosenpassConfig() != nil {
					rosenpassPubKey = msg.GetBody().GetRosenpassConfig().GetRosenpassPubKey()
					rosenpassAddr = msg.GetBody().GetRosenpassConfig().GetRosenpassServerAddr()
				}
				conn.OnRemoteOffer(peer.OfferAnswer{
					IceCredentials: peer.IceCredentials{
						UFrag: remoteCred.UFrag,
						Pwd:   remoteCred.Pwd,
					},
					WgListenPort:    int(msg.GetBody().GetWgListenPort()),
					Version:         msg.GetBody().GetNetBirdVersion(),
					RosenpassPubKey: rosenpassPubKey,
					RosenpassAddr:   rosenpassAddr,
					RelaySrvAddress: msg.GetBody().GetRelayServerAddress(),
				})
			case sProto.Body_ANSWER:
				remoteCred, err := signal.UnMarshalCredential(msg)
				if err != nil {
					return err
				}

				var rosenpassPubKey []byte
				rosenpassAddr := ""
				if msg.GetBody().GetRosenpassConfig() != nil {
					rosenpassPubKey = msg.GetBody().GetRosenpassConfig().GetRosenpassPubKey()
					rosenpassAddr = msg.GetBody().GetRosenpassConfig().GetRosenpassServerAddr()
				}
				conn.OnRemoteAnswer(peer.OfferAnswer{
					IceCredentials: peer.IceCredentials{
						UFrag: remoteCred.UFrag,
						Pwd:   remoteCred.Pwd,
					},
					WgListenPort:    int(msg.GetBody().GetWgListenPort()),
					Version:         msg.GetBody().GetNetBirdVersion(),
					RosenpassPubKey: rosenpassPubKey,
					RosenpassAddr:   rosenpassAddr,
					RelaySrvAddress: msg.GetBody().GetRelayServerAddress(),
				})
			case sProto.Body_CANDIDATE:
				candidate, err := ice.UnmarshalCandidate(msg.GetBody().Payload)
				if err != nil {
					log.Errorf("failed on parsing remote candidate %s -> %s", candidate, err)
					return err
				}

				go conn.OnRemoteCandidate(candidate, e.routeManager.GetClientRoutes())
			case sProto.Body_MODE:
			case sProto.Body_GO_IDLE:
				e.connMgr.DeactivatePeer(conn)
			}

			return nil
		})
		if err != nil {
			// happens if signal is unavailable for a long time.
			// We want to cancel the operation of the whole client
			_ = CtxGetState(e.ctx).Wrap(ErrResetConnection)
			e.clientCancel()
			return
		}
	}()

	e.signal.WaitStreamConnected()
}

func (e *Engine) parseNATExternalIPMappings() []string {
	var mappedIPs []string
	var ignoredIFaces = make(map[string]interface{})
	for _, iFace := range e.config.IFaceBlackList {
		ignoredIFaces[iFace] = nil
	}
	for _, mapping := range e.config.NATExternalIPs {
		var external, internal string
		var externalIP, internalIP net.IP
		var err error

		split := strings.Split(mapping, "/")
		if len(split) > 2 {
			log.Warnf("ignoring invalid external mapping '%s', too many delimiters", mapping)
			break
		}
		if len(split) > 1 {
			internal = split[1]
			internalIP = net.ParseIP(internal)
			if internalIP == nil {
				// not a properly formatted IP address, maybe it's interface name?
				if _, present := ignoredIFaces[internal]; present {
					log.Warnf("internal interface '%s' in blacklist, ignoring external mapping '%s'", internal, mapping)
					break
				}
				internalIP, err = findIPFromInterfaceName(internal)
				if err != nil {
					log.Warnf("error finding interface IP for interface '%s', ignoring external mapping '%s': %v", internal, mapping, err)
					break
				}
			}
		}
		external = split[0]
		externalIP = net.ParseIP(external)
		if externalIP == nil {
			log.Warnf("invalid external IP, %s, ignoring external IP mapping '%s'", external, mapping)
			break
		}
		mappedIP := externalIP.String()
		if internalIP != nil {
			mappedIP = mappedIP + "/" + internalIP.String()
		}
		mappedIPs = append(mappedIPs, mappedIP)
		log.Infof("parsed external IP mapping of '%s' as '%s'", mapping, mappedIP)
	}
	if len(mappedIPs) != len(e.config.NATExternalIPs) {
		log.Warnf("one or more external IP mappings failed to parse, ignoring all mappings")
		return nil
	}
	return mappedIPs
}

func (e *Engine) close() {
	log.Debugf("removing Netbird interface %s", e.config.WgIfaceName)
	if e.wgInterface != nil {
		if err := e.wgInterface.Close(); err != nil {
			log.Errorf("failed closing Netbird interface %s %v", e.config.WgIfaceName, err)
		}
		e.wgInterface = nil
		e.statusRecorder.SetWgIface(nil)
	}

	if !isNil(e.sshServer) {
		err := e.sshServer.Stop()
		if err != nil {
			log.Warnf("failed stopping the SSH server: %v", err)
		}
	}

	if e.firewall != nil {
		err := e.firewall.Close(e.stateManager)
		if err != nil {
			log.Warnf("failed to reset firewall: %s", err)
		}
	}

	if e.rpManager != nil {
		_ = e.rpManager.Close()
	}
}

func (e *Engine) readInitialSettings() ([]*route.Route, *nbdns.Config, bool, error) {
	if runtime.GOOS != "android" {
		// nolint:nilnil
		return nil, nil, false, nil
	}

	info := system.GetInfo(e.ctx)
	info.SetFlags(
		e.config.RosenpassEnabled,
		e.config.RosenpassPermissive,
		&e.config.ServerSSHAllowed,
		e.config.DisableClientRoutes,
		e.config.DisableServerRoutes,
		e.config.DisableDNS,
		e.config.DisableFirewall,
		e.config.BlockLANAccess,
		e.config.BlockInbound,
		e.config.LazyConnectionEnabled,
	)

	netMap, err := e.mgmClient.GetNetworkMap(info)
	if err != nil {
		return nil, nil, false, err
	}
	routes := toRoutes(netMap.GetRoutes())
	dnsCfg := toDNSConfig(netMap.GetDNSConfig(), e.wgInterface.Address().Network)
	dnsFeatureFlag := toDNSFeatureFlag(netMap)
	return routes, &dnsCfg, dnsFeatureFlag, nil
}

func (e *Engine) newWgIface() (*iface.WGIface, error) {
	transportNet, err := e.newStdNet()
	if err != nil {
		log.Errorf("failed to create pion's stdnet: %s", err)
	}

	opts := iface.WGIFaceOpts{
		IFaceName:    e.config.WgIfaceName,
		Address:      e.config.WgAddr,
		WGPort:       e.config.WgPort,
		WGPrivKey:    e.config.WgPrivateKey.String(),
		MTU:          iface.DefaultMTU,
		TransportNet: transportNet,
		FilterFn:     e.addrViaRoutes,
		DisableDNS:   e.config.DisableDNS,
	}

	switch runtime.GOOS {
	case "android":
		opts.MobileArgs = &device.MobileIFaceArguments{
			TunAdapter: e.mobileDep.TunAdapter,
			TunFd:      int(e.mobileDep.FileDescriptor),
		}
	case "ios":
		opts.MobileArgs = &device.MobileIFaceArguments{
			TunFd: int(e.mobileDep.FileDescriptor),
		}
	}

	return iface.NewWGIFace(opts)
}

func (e *Engine) wgInterfaceCreate() (err error) {
	switch runtime.GOOS {
	case "android":
		err = e.wgInterface.CreateOnAndroid(e.routeManager.InitialRouteRange(), e.dnsServer.DnsIP(), e.dnsServer.SearchDomains())
	case "ios":
		e.mobileDep.NetworkChangeListener.SetInterfaceIP(e.config.WgAddr)
		err = e.wgInterface.Create()
	default:
		err = e.wgInterface.Create()
	}
	return err
}

func (e *Engine) newDnsServer(dnsConfig *nbdns.Config) (dns.Server, error) {
	// due to tests where we are using a mocked version of the DNS server
	if e.dnsServer != nil {
		return e.dnsServer, nil
	}

	switch runtime.GOOS {
	case "android":
		dnsServer := dns.NewDefaultServerPermanentUpstream(
			e.ctx,
			e.wgInterface,
			e.mobileDep.HostDNSAddresses,
			*dnsConfig,
			e.mobileDep.NetworkChangeListener,
			e.statusRecorder,
			e.config.DisableDNS,
		)
		go e.mobileDep.DnsReadyListener.OnReady()
		return dnsServer, nil

	case "ios":
		dnsServer := dns.NewDefaultServerIos(e.ctx, e.wgInterface, e.mobileDep.DnsManager, e.statusRecorder, e.config.DisableDNS)
		return dnsServer, nil

	default:
		dnsServer, err := dns.NewDefaultServer(e.ctx, e.wgInterface, e.config.CustomDNSAddress, e.statusRecorder, e.stateManager, e.config.DisableDNS)
		if err != nil {
			return nil, err
		}

		return dnsServer, nil
	}
}

// GetRouteManager returns the route manager
func (e *Engine) GetRouteManager() routemanager.Manager {
	return e.routeManager
}

// GetFirewallManager returns the firewall manager
func (e *Engine) GetFirewallManager() firewallManager.Manager {
	return e.firewall
}

func findIPFromInterfaceName(ifaceName string) (net.IP, error) {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return nil, err
	}
	return findIPFromInterface(iface)
}

func findIPFromInterface(iface *net.Interface) (net.IP, error) {
	ifaceAddrs, err := iface.Addrs()
	if err != nil {
		return nil, err
	}
	for _, addr := range ifaceAddrs {
		if ipv4Addr := addr.(*net.IPNet).IP.To4(); ipv4Addr != nil {
			return ipv4Addr, nil
		}
	}
	return nil, fmt.Errorf("interface %s don't have an ipv4 address", iface.Name)
}

func (e *Engine) getRosenpassPubKey() []byte {
	if e.rpManager != nil {
		return e.rpManager.GetPubKey()
	}
	return nil
}

func (e *Engine) getRosenpassAddr() string {
	if e.rpManager != nil {
		return e.rpManager.GetAddress().String()
	}
	return ""
}

// RunHealthProbes executes health checks for Signal, Management, Relay and WireGuard services
// and updates the status recorder with the latest states.
func (e *Engine) RunHealthProbes() bool {
	e.syncMsgMux.Lock()

	signalHealthy := e.signal.IsHealthy()
	log.Debugf("signal health check: healthy=%t", signalHealthy)

	managementHealthy := e.mgmClient.IsHealthy()
	log.Debugf("management health check: healthy=%t", managementHealthy)

	stuns := slices.Clone(e.STUNs)
	turns := slices.Clone(e.TURNs)

	if e.wgInterface != nil {
		stats, err := e.wgInterface.GetStats()
		if err != nil {
			log.Warnf("failed to get wireguard stats: %v", err)
			e.syncMsgMux.Unlock()
			return false
		}
		for _, key := range e.peerStore.PeersPubKey() {
			// wgStats could be zero value, in which case we just reset the stats
			wgStats, ok := stats[key]
			if !ok {
				continue
			}
			if err := e.statusRecorder.UpdateWireGuardPeerState(key, wgStats); err != nil {
				log.Debugf("failed to update wg stats for peer %s: %s", key, err)
			}
		}
	}

	e.syncMsgMux.Unlock()

	results := e.probeICE(stuns, turns)
	e.statusRecorder.UpdateRelayStates(results)

	relayHealthy := true
	for _, res := range results {
		if res.Err != nil {
			relayHealthy = false
			break
		}
	}
	log.Debugf("relay health check: healthy=%t", relayHealthy)

	allHealthy := signalHealthy && managementHealthy && relayHealthy
	log.Debugf("all health checks completed: healthy=%t", allHealthy)
	return allHealthy
}

func (e *Engine) probeICE(stuns, turns []*stun.URI) []relay.ProbeResult {
	return append(
		relay.ProbeAll(e.ctx, relay.ProbeSTUN, stuns),
		relay.ProbeAll(e.ctx, relay.ProbeTURN, turns)...,
	)
}

// restartEngine restarts the engine by cancelling the client context
func (e *Engine) restartEngine() {
	e.syncMsgMux.Lock()
	defer e.syncMsgMux.Unlock()

	if e.ctx.Err() != nil {
		return
	}

	log.Info("restarting engine")
	CtxGetState(e.ctx).Set(StatusConnecting)
	_ = CtxGetState(e.ctx).Wrap(ErrResetConnection)
	log.Infof("cancelling client context, engine will be recreated")
	e.clientCancel()
}

func (e *Engine) startNetworkMonitor() {
	if !e.config.NetworkMonitor {
		log.Infof("Network monitor is disabled, not starting")
		return
	}

	e.networkMonitor = networkmonitor.New()
	go func() {
		if err := e.networkMonitor.Listen(e.ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				log.Infof("network monitor stopped")
				return
			}
			log.Errorf("network monitor error: %v", err)
			return
		}

		log.Infof("Network monitor: detected network change, restarting engine")
		e.restartEngine()
	}()
}

func (e *Engine) addrViaRoutes(addr netip.Addr) (bool, netip.Prefix, error) {
	var vpnRoutes []netip.Prefix
	for _, routes := range e.routeManager.GetClientRoutes() {
		if len(routes) > 0 && routes[0] != nil {
			vpnRoutes = append(vpnRoutes, routes[0].Network)
		}
	}

	if isVpn, prefix := systemops.IsAddrRouted(addr, vpnRoutes); isVpn {
		return true, prefix, nil
	}

	return false, netip.Prefix{}, nil
}

func (e *Engine) stopDNSServer() {
	if e.dnsServer == nil {
		return
	}
	e.dnsServer.Stop()
	e.dnsServer = nil
	err := fmt.Errorf("DNS server stopped")
	nsGroupStates := e.statusRecorder.GetDNSStates()
	for i := range nsGroupStates {
		nsGroupStates[i].Enabled = false
		nsGroupStates[i].Error = err
	}
	e.statusRecorder.UpdateDNSStates(nsGroupStates)
}

// SetNetworkMapPersistence enables or disables network map persistence
func (e *Engine) SetNetworkMapPersistence(enabled bool) {
	e.syncMsgMux.Lock()
	defer e.syncMsgMux.Unlock()

	if enabled == e.persistNetworkMap {
		return
	}
	e.persistNetworkMap = enabled
	log.Debugf("Network map persistence is set to %t", enabled)

	if !enabled {
		e.latestNetworkMap = nil
	}
}

// GetLatestNetworkMap returns the stored network map if persistence is enabled
func (e *Engine) GetLatestNetworkMap() (*mgmProto.NetworkMap, error) {
	e.syncMsgMux.Lock()
	defer e.syncMsgMux.Unlock()

	if !e.persistNetworkMap {
		return nil, errors.New("network map persistence is disabled")
	}

	if e.latestNetworkMap == nil {
		//nolint:nilnil
		return nil, nil
	}

	log.Debugf("Retrieving latest network map with size %d bytes", proto.Size(e.latestNetworkMap))
	nm, ok := proto.Clone(e.latestNetworkMap).(*mgmProto.NetworkMap)
	if !ok {

		return nil, fmt.Errorf("failed to clone network map")
	}

	return nm, nil
}

// GetWgAddr returns the wireguard address
func (e *Engine) GetWgAddr() netip.Addr {
	if e.wgInterface == nil {
		return netip.Addr{}
	}
	return e.wgInterface.Address().IP
}

// updateDNSForwarder start or stop the DNS forwarder based on the domains and the feature flag
func (e *Engine) updateDNSForwarder(
	enabled bool,
	fwdEntries []*dnsfwd.ForwarderEntry,
) {
	if e.config.DisableServerRoutes {
		return
	}

	if !enabled {
		if e.dnsForwardMgr == nil {
			return
		}
		if err := e.dnsForwardMgr.Stop(context.Background()); err != nil {
			log.Errorf("failed to stop DNS forward: %v", err)
		}
		return
	}

	if len(fwdEntries) > 0 {
		if e.dnsForwardMgr == nil {
			e.dnsForwardMgr = dnsfwd.NewManager(e.firewall, e.statusRecorder)

			if err := e.dnsForwardMgr.Start(fwdEntries); err != nil {
				log.Errorf("failed to start DNS forward: %v", err)
				e.dnsForwardMgr = nil
			}

			log.Infof("started domain router service with %d entries", len(fwdEntries))
		} else {
			e.dnsForwardMgr.UpdateDomains(fwdEntries)
		}
	} else if e.dnsForwardMgr != nil {
		log.Infof("disable domain router service")
		if err := e.dnsForwardMgr.Stop(context.Background()); err != nil {
			log.Errorf("failed to stop DNS forward: %v", err)
		}
		e.dnsForwardMgr = nil
	}
}

func (e *Engine) GetNet() (*netstack.Net, error) {
	e.syncMsgMux.Lock()
	intf := e.wgInterface
	e.syncMsgMux.Unlock()
	if intf == nil {
		return nil, errors.New("wireguard interface not initialized")
	}

	nsnet := intf.GetNet()
	if nsnet == nil {
		return nil, errors.New("failed to get netstack")
	}
	return nsnet, nil
}

func (e *Engine) Address() (netip.Addr, error) {
	e.syncMsgMux.Lock()
	intf := e.wgInterface
	e.syncMsgMux.Unlock()
	if intf == nil {
		return netip.Addr{}, errors.New("wireguard interface not initialized")
	}

	return e.wgInterface.Address().IP, nil
}

func (e *Engine) updateForwardRules(rules []*mgmProto.ForwardingRule) ([]firewallManager.ForwardRule, error) {
	if e.firewall == nil {
		log.Warn("firewall is disabled, not updating forwarding rules")
		return nil, nil
	}

	if len(rules) == 0 {
		if e.ingressGatewayMgr == nil {
			return nil, nil
		}

		err := e.ingressGatewayMgr.Close()
		e.ingressGatewayMgr = nil
		e.statusRecorder.SetIngressGwMgr(nil)
		return nil, err
	}

	if e.ingressGatewayMgr == nil {
		mgr := ingressgw.NewManager(e.firewall)
		e.ingressGatewayMgr = mgr
		e.statusRecorder.SetIngressGwMgr(mgr)
	}

	var merr *multierror.Error
	forwardingRules := make([]firewallManager.ForwardRule, 0, len(rules))
	for _, rule := range rules {
		proto, err := convertToFirewallProtocol(rule.GetProtocol())
		if err != nil {
			merr = multierror.Append(merr, fmt.Errorf("failed to convert protocol '%s': %w", rule.GetProtocol(), err))
			continue
		}

		dstPortInfo, err := convertPortInfo(rule.GetDestinationPort())
		if err != nil {
			merr = multierror.Append(merr, fmt.Errorf("invalid destination port '%v': %w", rule.GetDestinationPort(), err))
			continue
		}

		translateIP, err := convertToIP(rule.GetTranslatedAddress())
		if err != nil {
			merr = multierror.Append(merr, fmt.Errorf("failed to convert translated address '%s': %w", rule.GetTranslatedAddress(), err))
			continue
		}

		translatePort, err := convertPortInfo(rule.GetTranslatedPort())
		if err != nil {
			merr = multierror.Append(merr, fmt.Errorf("invalid translate port '%v': %w", rule.GetTranslatedPort(), err))
			continue
		}

		forwardRule := firewallManager.ForwardRule{
			Protocol:          proto,
			DestinationPort:   *dstPortInfo,
			TranslatedAddress: translateIP,
			TranslatedPort:    *translatePort,
		}

		forwardingRules = append(forwardingRules, forwardRule)
	}

	log.Infof("updating forwarding rules: %d", len(forwardingRules))
	if err := e.ingressGatewayMgr.Update(forwardingRules); err != nil {
		log.Errorf("failed to update forwarding rules: %v", err)
	}

	return forwardingRules, nberrors.FormatErrorOrNil(merr)
}

func (e *Engine) toExcludedLazyPeers(rules []firewallManager.ForwardRule, peers []*mgmProto.RemotePeerConfig) map[string]bool {
	excludedPeers := make(map[string]bool)
	for _, r := range rules {
		ip := r.TranslatedAddress
		for _, p := range peers {
			for _, allowedIP := range p.GetAllowedIps() {
				if allowedIP != ip.String() {
					continue
				}
				log.Infof("exclude forwarder peer from lazy connection: %s", p.GetWgPubKey())
				excludedPeers[p.GetWgPubKey()] = true
			}
		}
	}

	return excludedPeers
}

// isChecksEqual checks if two slices of checks are equal.
func isChecksEqual(checks []*mgmProto.Checks, oChecks []*mgmProto.Checks) bool {
	for _, check := range checks {
		sort.Slice(check.Files, func(i, j int) bool {
			return check.Files[i] < check.Files[j]
		})
	}
	for _, oCheck := range oChecks {
		sort.Slice(oCheck.Files, func(i, j int) bool {
			return oCheck.Files[i] < oCheck.Files[j]
		})
	}

	return slices.EqualFunc(checks, oChecks, func(checks, oChecks *mgmProto.Checks) bool {
		return slices.Equal(checks.Files, oChecks.Files)
	})
}

func getInterfacePrefixes() ([]netip.Prefix, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("get interfaces: %w", err)
	}

	var prefixes []netip.Prefix
	var merr *multierror.Error

	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			merr = multierror.Append(merr, fmt.Errorf("get addresses for interface %s: %w", iface.Name, err))
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				merr = multierror.Append(merr, fmt.Errorf("cast address to IPNet: %v", addr))
				continue
			}
			addr, ok := netip.AddrFromSlice(ipNet.IP)
			if !ok {
				merr = multierror.Append(merr, fmt.Errorf("cast IPNet to netip.Addr: %v", ipNet.IP))
				continue
			}
			ones, _ := ipNet.Mask.Size()
			prefix := netip.PrefixFrom(addr.Unmap(), ones).Masked()
			ip := prefix.Addr()

			// TODO: add IPv6
			if !ip.Is4() || ip.IsLoopback() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
				continue
			}

			prefixes = append(prefixes, prefix)
		}
	}

	return prefixes, nberrors.FormatErrorOrNil(merr)
}

// compareNetIPLists compares a list of netip.Prefix with a list of strings.
// return true if both lists are equal, false otherwise.
func compareNetIPLists(list1 []netip.Prefix, list2 []string) bool {
	if len(list1) != len(list2) {
		return false
	}

	freq := make(map[string]int, len(list1))
	for _, p := range list1 {
		freq[p.String()]++
	}

	for _, s := range list2 {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return false // invalid prefix in list2.
		}
		key := p.String()
		if freq[key] == 0 {
			return false
		}
		freq[key]--
	}

	// all counts should be zero if lists are equal.
	for _, count := range freq {
		if count != 0 {
			return false
		}
	}
	return true
}

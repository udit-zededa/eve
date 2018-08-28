// Copyright (c) 2017-2018 Zededa, Inc.
// All rights reserved.

// Process input in the form of a collection of AppNetworkConfig structs
// from zedmanager and zedagent. Publish the status as AppNetworkStatus.
// Produce the updated configlets (for radvd, dnsmasq, ip*tables, lisp.config,
// ipset, ip link/addr/route configuration) based on that and apply those
// configlets.

package zedrouter

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/vishvananda/netlink"
	"github.com/zededa/go-provision/adapters"
	"github.com/zededa/go-provision/agentlog"
	"github.com/zededa/go-provision/cast"
	"github.com/zededa/go-provision/devicenetwork"
	"github.com/zededa/go-provision/flextimer"
	"github.com/zededa/go-provision/hardware"
	"github.com/zededa/go-provision/pidfile"
	"github.com/zededa/go-provision/pubsub"
	"github.com/zededa/go-provision/types"
	"github.com/zededa/go-provision/wrap"
	"log"
	"net"
	"os"
	"strconv"
	"time"
)

const (
	agentName     = "zedrouter"
	runDirname    = "/var/run/zedrouter"
	tmpDirname    = "/var/tmp/zededa"
	DataPlaneName = "lisp-ztr"
)

// Set from Makefile
var Version = "No version specified"

type zedrouterContext struct {
	// Experimental Zededa data plane enable/disable flag
	separateDataPlane       bool
	subNetworkObjectConfig  *pubsub.Subscription
	subNetworkServiceConfig *pubsub.Subscription
	pubNetworkObjectStatus  *pubsub.Publication
	pubNetworkServiceStatus *pubsub.Publication
	subAppNetworkConfig     *pubsub.Subscription
	subAppNetworkConfigAg   *pubsub.Subscription // From zedagent for dom0
	pubAppNetworkStatus     *pubsub.Publication
	assignableAdapters      *types.AssignableAdapters
	devicenetwork.DeviceNetworkContext
	ready bool
}

var debug = false

func Run() {
	logf, err := agentlog.Init(agentName)
	if err != nil {
		log.Fatal(err)
	}
	defer logf.Close()

	versionPtr := flag.Bool("v", false, "Version")
	debugPtr := flag.Bool("d", false, "Debug flag")
	flag.Parse()
	debug = *debugPtr
	if *versionPtr {
		fmt.Printf("%s: %s\n", os.Args[0], Version)
		return
	}
	if err := pidfile.CheckAndCreatePidfile(agentName); err != nil {
		log.Fatal(err)
	}
	log.Printf("Starting %s\n", agentName)

	if _, err := os.Stat(runDirname); err != nil {
		log.Printf("Create %s\n", runDirname)
		if err := os.Mkdir(runDirname, 0755); err != nil {
			log.Fatal(err)
		}
	} else {
		// dnsmasq needs to read as nobody
		if err := os.Chmod(runDirname, 0755); err != nil {
			log.Fatal(err)
		}
	}

	pubDeviceNetworkStatus, err := pubsub.Publish(agentName,
		types.DeviceNetworkStatus{})
	if err != nil {
		log.Fatal(err)
	}
	pubDeviceNetworkStatus.ClearRestarted()

	pubDeviceUplinkConfig, err := pubsub.Publish(agentName,
		types.DeviceUplinkConfig{})
	if err != nil {
		log.Fatal(err)
	}
	pubDeviceUplinkConfig.ClearRestarted()

	model := hardware.GetHardwareModel()

	// Pick up (mostly static) AssignableAdapters before we process
	// any Routes; Pbr needs to know which network adapters are assignable
	aa := types.AssignableAdapters{}
	subAa := adapters.Subscribe(&aa, model)

	for !subAa.Found {
		log.Printf("Waiting for AssignableAdapters %v\n", subAa.Found)
		select {
		case change := <-subAa.C:
			subAa.ProcessChange(change)
		}
	}
	log.Printf("Have %d assignable adapters\n", len(aa.IoBundleList))

	zedrouterCtx := zedrouterContext{
		separateDataPlane:  false,
		assignableAdapters: &aa,
	}
	zedrouterCtx.ManufacturerModel = model
	zedrouterCtx.DeviceNetworkConfig = &types.DeviceNetworkConfig{}
	zedrouterCtx.DeviceUplinkConfig = &types.DeviceUplinkConfig{}
	zedrouterCtx.DeviceNetworkStatus = &types.DeviceNetworkStatus{}
	zedrouterCtx.PubDeviceUplinkConfig = pubDeviceUplinkConfig
	zedrouterCtx.PubDeviceNetworkStatus = pubDeviceNetworkStatus

	// Create publish before subscribing and activating subscriptions
	// Also need to do this before we wait for IP addresses since
	// zedagent waits for these to be published/exist, and zedagent
	// runs the fallback timers after that wait.
	pubNetworkObjectStatus, err := pubsub.Publish(agentName,
		types.NetworkObjectStatus{})
	if err != nil {
		log.Fatal(err)
	}
	zedrouterCtx.pubNetworkObjectStatus = pubNetworkObjectStatus

	pubNetworkServiceStatus, err := pubsub.Publish(agentName,
		types.NetworkServiceStatus{})
	if err != nil {
		log.Fatal(err)
	}
	zedrouterCtx.pubNetworkServiceStatus = pubNetworkServiceStatus

	pubAppNetworkStatus, err := pubsub.Publish(agentName,
		types.AppNetworkStatus{})
	if err != nil {
		log.Fatal(err)
	}
	zedrouterCtx.pubAppNetworkStatus = pubAppNetworkStatus
	pubAppNetworkStatus.ClearRestarted()

	appNumAllocatorInit(pubAppNetworkStatus)
	bridgeNumAllocatorInit(pubNetworkObjectStatus)

	// Get the initial DeviceNetworkConfig
	// Subscribe from "" means /var/tmp/zededa/
	subDeviceNetworkConfig, err := pubsub.Subscribe("",
		types.DeviceNetworkConfig{}, false,
		&zedrouterCtx.DeviceNetworkContext)
	if err != nil {
		log.Fatal(err)
	}
	subDeviceNetworkConfig.ModifyHandler = devicenetwork.HandleDNCModify
	subDeviceNetworkConfig.DeleteHandler = devicenetwork.HandleDNCDelete
	zedrouterCtx.SubDeviceNetworkConfig = subDeviceNetworkConfig
	subDeviceNetworkConfig.Activate()

	// We get DeviceUplinkConfig from three sources in this priority:
	// 1. zedagent
	// 2. override file in /var/tmp/zededa/NetworkUplinkConfig/override.json
	// 3. self-generated file derived from per-platform DeviceNetworkConfig
	subDeviceUplinkConfigA, err := pubsub.Subscribe("zedagent",
		types.DeviceUplinkConfig{}, false,
		&zedrouterCtx.DeviceNetworkContext)
	if err != nil {
		log.Fatal(err)
	}
	subDeviceUplinkConfigA.ModifyHandler = devicenetwork.HandleDUCModify
	subDeviceUplinkConfigA.DeleteHandler = devicenetwork.HandleDUCDelete
	zedrouterCtx.SubDeviceUplinkConfigA = subDeviceUplinkConfigA
	subDeviceUplinkConfigA.Activate()

	subDeviceUplinkConfigO, err := pubsub.Subscribe("",
		types.DeviceUplinkConfig{}, false,
		&zedrouterCtx.DeviceNetworkContext)
	if err != nil {
		log.Fatal(err)
	}
	subDeviceUplinkConfigO.ModifyHandler = devicenetwork.HandleDUCModify
	subDeviceUplinkConfigO.DeleteHandler = devicenetwork.HandleDUCDelete
	zedrouterCtx.SubDeviceUplinkConfigO = subDeviceUplinkConfigO
	subDeviceUplinkConfigO.Activate()

	subDeviceUplinkConfigS, err := pubsub.Subscribe(agentName,
		types.DeviceUplinkConfig{}, false,
		&zedrouterCtx.DeviceNetworkContext)
	if err != nil {
		log.Fatal(err)
	}
	subDeviceUplinkConfigS.ModifyHandler = devicenetwork.HandleDUCModify
	subDeviceUplinkConfigS.DeleteHandler = devicenetwork.HandleDUCDelete
	zedrouterCtx.SubDeviceUplinkConfigS = subDeviceUplinkConfigS
	subDeviceUplinkConfigS.Activate()

	// Make sure we wait for a while to process all the DeviceUplinkConfigs
	done := zedrouterCtx.UsableAddressCount != 0
	t1 := time.NewTimer(5 * time.Second)
	for zedrouterCtx.UsableAddressCount == 0 || !done {
		log.Printf("Waiting for UsableAddressCount %d and done %v\n",
			zedrouterCtx.UsableAddressCount, done)
		select {
		case change := <-subDeviceNetworkConfig.C:
			subDeviceNetworkConfig.ProcessChange(change)
			maybeHandleDUC(&zedrouterCtx)

		case change := <-subDeviceUplinkConfigA.C:
			subDeviceUplinkConfigA.ProcessChange(change)
			maybeHandleDUC(&zedrouterCtx)

		case change := <-subDeviceUplinkConfigO.C:
			subDeviceUplinkConfigO.ProcessChange(change)
			maybeHandleDUC(&zedrouterCtx)

		case change := <-subDeviceUplinkConfigS.C:
			subDeviceUplinkConfigS.ProcessChange(change)
			maybeHandleDUC(&zedrouterCtx)

		case <-t1.C:
			done = true
		}
	}
	log.Printf("Got for DeviceNetworkConfig: %d usable addresses\n",
		zedrouterCtx.UsableAddressCount)

	handleInit(runDirname, pubDeviceNetworkStatus)

	// Subscribe to network objects and services from zedagent
	subNetworkObjectConfig, err := pubsub.Subscribe("zedagent",
		types.NetworkObjectConfig{}, false, &zedrouterCtx)
	if err != nil {
		log.Fatal(err)
	}
	subNetworkObjectConfig.ModifyHandler = handleNetworkObjectModify
	subNetworkObjectConfig.DeleteHandler = handleNetworkObjectDelete
	zedrouterCtx.subNetworkObjectConfig = subNetworkObjectConfig
	subNetworkObjectConfig.Activate()

	subNetworkServiceConfig, err := pubsub.Subscribe("zedagent",
		types.NetworkServiceConfig{}, false, &zedrouterCtx)
	if err != nil {
		log.Fatal(err)
	}
	subNetworkServiceConfig.ModifyHandler = handleNetworkServiceModify
	subNetworkServiceConfig.DeleteHandler = handleNetworkServiceDelete
	zedrouterCtx.subNetworkServiceConfig = subNetworkServiceConfig
	subNetworkServiceConfig.Activate()

	// Subscribe to AppNetworkConfig from zedmanager and from zedagent
	subAppNetworkConfig, err := pubsub.Subscribe("zedmanager",
		types.AppNetworkConfig{}, false, &zedrouterCtx)
	if err != nil {
		log.Fatal(err)
	}
	subAppNetworkConfig.ModifyHandler = handleAppNetworkConfigModify
	subAppNetworkConfig.DeleteHandler = handleAppNetworkConfigDelete
	subAppNetworkConfig.RestartHandler = handleRestart
	zedrouterCtx.subAppNetworkConfig = subAppNetworkConfig
	subAppNetworkConfig.Activate()

	// Subscribe to AppNetworkConfig from zedmanager
	subAppNetworkConfigAg, err := pubsub.Subscribe("zedagent",
		types.AppNetworkConfig{}, false, &zedrouterCtx)
	if err != nil {
		log.Fatal(err)
	}
	subAppNetworkConfigAg.ModifyHandler = handleAppNetworkConfigModify
	subAppNetworkConfigAg.DeleteHandler = handleAppNetworkConfigDelete
	zedrouterCtx.subAppNetworkConfigAg = subAppNetworkConfigAg
	subAppNetworkConfigAg.Activate()

	// XXX should we make geoRedoTime configurable?
	// We refresh the gelocation information when the underlay
	// IP address(es) change, or once an hour.
	geoRedoTime := time.Hour

	// Timer for retries after failure etc. Should be less than geoRedoTime
	geoInterval := time.Duration(10 * time.Minute)
	geoMax := float64(geoInterval)
	geoMin := geoMax * 0.3
	geoTimer := flextimer.NewRangeTicker(time.Duration(geoMin),
		time.Duration(geoMax))

	// This function is called from PBR when some uplink interface changes
	// its IP address(es)
	addrChangeUplinkFn := func(ifname string) {
		if debug {
			log.Printf("addrChangeUplinkFn(%s) called\n", ifname)
		}
		devicenetwork.HandleAddressChange(&zedrouterCtx.DeviceNetworkContext,
			ifname)
	}

	// This function is called from PBR when some non-uplink interface
	// changes its IP address(es)
	addrChangeNonUplinkFn := func(ifname string) {
		if debug {
			log.Printf("addrChangeNonUplinkFn(%s) called\n", ifname)
		}
		// Even if ethN isn't individually assignable, it
		// could be used for a bridge.
		maybeUpdateBridgeIPAddr(&zedrouterCtx, ifname)
	}
	routeChanges, addrChanges, linkChanges := PbrInit(
		&zedrouterCtx, addrChangeUplinkFn, addrChangeNonUplinkFn)

	// Publish network metrics for zedagent every 10 seconds
	nms := getNetworkMetrics(&zedrouterCtx) // Need type of data
	pub, err := pubsub.Publish(agentName, nms)
	if err != nil {
		log.Fatal(err)
	}
	interval := time.Duration(10 * time.Second)
	max := float64(interval)
	min := max * 0.3
	publishTimer := flextimer.NewRangeTicker(time.Duration(min),
		time.Duration(max))

	// Apply any changes from the uplink config to date.
	publishDeviceNetworkStatus(&zedrouterCtx)
	updateLispConfiglets(&zedrouterCtx, zedrouterCtx.separateDataPlane)

	setFreeUplinks(devicenetwork.GetFreeUplinks(*zedrouterCtx.DeviceUplinkConfig))

	zedrouterCtx.ready = true

	// First wait for restarted from zedmanager
	for !subAppNetworkConfig.Restarted() {
		log.Printf("Waiting for zedmanager to report restarted\n")
		select {
		case change := <-subAppNetworkConfig.C:
			subAppNetworkConfig.ProcessChange(change)
		}
	}
	log.Printf("Zedmanager has restarted\n")

	for {
		select {
		case change := <-subAppNetworkConfig.C:
			subAppNetworkConfig.ProcessChange(change)

		case change := <-subAppNetworkConfigAg.C:
			subAppNetworkConfigAg.ProcessChange(change)

		case change := <-subDeviceNetworkConfig.C:
			subDeviceNetworkConfig.ProcessChange(change)
			maybeHandleDUC(&zedrouterCtx)

		case change := <-subDeviceUplinkConfigA.C:
			subDeviceUplinkConfigA.ProcessChange(change)
			maybeHandleDUC(&zedrouterCtx)

		case change := <-subDeviceUplinkConfigO.C:
			subDeviceUplinkConfigO.ProcessChange(change)
			maybeHandleDUC(&zedrouterCtx)

		case change := <-subDeviceUplinkConfigS.C:
			subDeviceUplinkConfigS.ProcessChange(change)
			maybeHandleDUC(&zedrouterCtx)

		case change := <-addrChanges:
			PbrAddrChange(zedrouterCtx.DeviceUplinkConfig, change)
		case change := <-linkChanges:
			PbrLinkChange(zedrouterCtx.DeviceUplinkConfig, change)
		case change := <-routeChanges:
			PbrRouteChange(zedrouterCtx.DeviceUplinkConfig, change)
		case <-publishTimer.C:
			if debug {
				log.Println("publishTimer at",
					time.Now())
			}
			err := pub.Publish("global",
				getNetworkMetrics(&zedrouterCtx))
			if err != nil {
				log.Println(err)
			}
		case <-geoTimer.C:
			if debug {
				log.Println("geoTimer at", time.Now())
			}
			change := devicenetwork.UpdateDeviceNetworkGeo(
				geoRedoTime, zedrouterCtx.DeviceNetworkStatus)
			if change {
				publishDeviceNetworkStatus(&zedrouterCtx)
			}

		case change := <-subNetworkObjectConfig.C:
			subNetworkObjectConfig.ProcessChange(change)

		case change := <-subNetworkServiceConfig.C:
			subNetworkServiceConfig.ProcessChange(change)

		case change := <-subAa.C:
			subAa.ProcessChange(change)
		}
	}
}

func maybeHandleDUC(ctx *zedrouterContext) {
	if !ctx.Changed {
		return
	}
	ctx.Changed = false
	if !ctx.ready {
		return
	}
	updateLispConfiglets(ctx, ctx.separateDataPlane)
	setFreeUplinks(devicenetwork.GetFreeUplinks(*ctx.DeviceUplinkConfig))
	// XXX do a NatInactivate/NatActivate if freeuplinks/uplinks changed?
}

func handleRestart(ctxArg interface{}, done bool) {
	if debug {
		log.Printf("handleRestart(%v)\n", done)
	}
	ctx := ctxArg.(*zedrouterContext)
	if ctx.ready {
		handleLispRestart(done, ctx.separateDataPlane)
	}
	if done {
		// Since all work is done inline we can immediately say that
		// we have restarted.
		ctx.pubAppNetworkStatus.SignalRestarted()
	}
}

var globalRunDirname string
var lispRunDirname string

// XXX hack to avoid the pslisp hang on Erik's laptop
var broken = false

func handleInit(runDirname string, pubDeviceNetworkStatus *pubsub.Publication) {

	globalRunDirname = runDirname

	// XXX should this be in the lisp code?
	lispRunDirname = runDirname + "/lisp"
	if _, err := os.Stat(lispRunDirname); err != nil {
		log.Printf("Create %s\n", lispRunDirname)
		if err := os.Mkdir(lispRunDirname, 0700); err != nil {
			log.Fatal(err)
		}
	}
	// XXX should this be in dnsmasq code?
	// Need to make sure we don't have any stale leases
	leasesFile := "/var/lib/misc/dnsmasq.leases"
	if _, err := os.Stat(leasesFile); err == nil {
		if err := os.Remove(leasesFile); err != nil {
			log.Fatal(err)
		}
	}

	// Setup initial iptables rules
	iptablesInit()

	// ipsets which are independent of config
	createDefaultIpset()

	_, err := wrap.Command("sysctl", "-w",
		"net.ipv4.ip_forward=1").Output()
	if err != nil {
		log.Fatal("Failed setting ip_forward ", err)
	}
	_, err = wrap.Command("sysctl", "-w",
		"net.ipv6.conf.all.forwarding=1").Output()
	if err != nil {
		log.Fatal("Failed setting ipv6.conf.all.forwarding ", err)
	}
	// We use ip6tables for the bridge
	_, err = wrap.Command("sysctl", "-w",
		"net.bridge.bridge-nf-call-ip6tables=1").Output()
	if err != nil {
		log.Fatal("Failed setting net.bridge-nf-call-ip6tables ", err)
	}
	_, err = wrap.Command("sysctl", "-w",
		"net.bridge.bridge-nf-call-iptables=1").Output()
	if err != nil {
		log.Fatal("Failed setting net.bridge-nf-call-iptables ", err)
	}
	_, err = wrap.Command("sysctl", "-w",
		"net.bridge.bridge-nf-call-arptables=1").Output()
	if err != nil {
		log.Fatal("Failed setting net.bridge-nf-call-arptables ", err)
	}

	// XXX hack to determine whether a real system or Erik's laptop
	_, err = wrap.Command("xl", "list").Output()
	if err != nil {
		log.Printf("Command xl list failed: %s\n", err)
		broken = true
	}
}

func publishDeviceNetworkStatus(ctx *zedrouterContext) {
	ctx.PubDeviceNetworkStatus.Publish("global", ctx.DeviceNetworkStatus)
}

func publishAppNetworkStatus(ctx *zedrouterContext,
	status *types.AppNetworkStatus) {

	key := status.Key()
	log.Printf("publishAppNetworkStatus(%s)\n", key)
	pub := ctx.pubAppNetworkStatus
	pub.Publish(key, status)
}

func publishNetworkObjectStatus(ctx *zedrouterContext,
	status *types.NetworkObjectStatus) {
	key := status.Key()
	log.Printf("publishNetworkObjectStatus(%s)\n", key)
	pub := ctx.pubNetworkObjectStatus
	pub.Publish(key, *status)
}

func unpublishAppNetworkStatus(ctx *zedrouterContext,
	status *types.AppNetworkStatus) {

	key := status.Key()
	log.Printf("unpublishAppNetworkStatus(%s)\n", key)
	pub := ctx.pubAppNetworkStatus
	st, _ := pub.Get(key)
	if st == nil {
		log.Printf("unpublishAppNetworkStatus(%s) not found\n", key)
		return
	}
	pub.Unpublish(key)
}

func unpublishNetworkObjectStatus(ctx *zedrouterContext,
	status *types.NetworkObjectStatus) {
	key := status.Key()
	log.Printf("unpublishNetworkObjectStatus(%s)\n", key)
	pub := ctx.pubNetworkObjectStatus
	st, _ := pub.Get(key)
	if st == nil {
		log.Printf("unpublishNetworkObjectStatus(%s) not found\n", key)
		return
	}
	pub.Unpublish(key)
}

// Format a json string with any additional info
func generateAdditionalInfo(status types.AppNetworkStatus, olConfig types.OverlayNetworkConfig) string {
	additionalInfo := ""
	if status.IsZedmanager {
		if olConfig.AdditionalInfoDevice != nil {
			b, err := json.Marshal(olConfig.AdditionalInfoDevice)
			if err != nil {
				log.Fatal(err, "json Marshal AdditionalInfoDevice")
			}
			additionalInfo = string(b)
			if debug {
				log.Printf("Generated additional info device %s\n",
					additionalInfo)
			}
		}
	} else {
		// Combine subset of the device and application information
		addInfoApp := types.AdditionalInfoApp{
			DeviceEID:   deviceEID,
			DeviceIID:   deviceIID,
			DisplayName: status.DisplayName,
		}
		if additionalInfoDevice != nil {
			addInfoApp.UnderlayIP = additionalInfoDevice.UnderlayIP
			addInfoApp.Hostname = additionalInfoDevice.Hostname
		}
		b, err := json.Marshal(addInfoApp)
		if err != nil {
			log.Fatal(err, "json Marshal AdditionalInfoApp")
		}
		additionalInfo = string(b)
		if debug {
			log.Printf("Generated additional info app %s\n",
				additionalInfo)
		}
	}
	return additionalInfo
}

func updateLispConfiglets(ctx *zedrouterContext, separateDataPlane bool) {
	pub := ctx.pubAppNetworkStatus
	items := pub.GetAll()
	for _, st := range items {
		status := cast.CastAppNetworkStatus(st)
		for i, olStatus := range status.OverlayNetworkList {
			olNum := i + 1
			var olIfname string
			if status.IsZedmanager {
				olIfname = "dbo" + strconv.Itoa(olNum) + "x" +
					strconv.Itoa(status.AppNum)
			} else {
				olIfname = olStatus.Bridge
			}
			additionalInfo := generateAdditionalInfo(status,
				olStatus.OverlayNetworkConfig)
			if debug {
				log.Printf("updateLispConfiglets for %s isMgmt %v\n",
					olIfname, status.IsZedmanager)
			}
			createLispConfiglet(lispRunDirname, status.IsZedmanager,
				olStatus.MgmtIID, olStatus.EID,
				olStatus.LispSignature,
				*ctx.DeviceNetworkStatus, olIfname,
				olIfname, additionalInfo,
				olStatus.MgmtMapServers, separateDataPlane)
		}
	}
}

// Wrappers around handleCreate, handleModify, and handleDelete

// Determine whether it is an create or modify
func handleAppNetworkConfigModify(ctxArg interface{}, key string, configArg interface{}) {

	log.Printf("handleAppNetworkConfigModify(%s)\n", key)
	ctx := ctxArg.(*zedrouterContext)
	config := cast.CastAppNetworkConfig(configArg)
	if config.Key() != key {
		log.Printf("handleAppNetworkConfigModify key/UUID mismatch %s vs %s; ignored %+v\n",
			key, config.Key(), config)
		return
	}
	status := lookupAppNetworkStatus(ctx, key)
	if status == nil {
		handleCreate(ctx, key, config)
	} else {
		handleModify(ctx, key, config, status)
	}
	log.Printf("handleAppNetworkConfigModify(%s) done\n", key)
}

func handleAppNetworkConfigDelete(ctxArg interface{}, key string,
	configArg interface{}) {

	log.Printf("handleAppNetworkConfigDelete(%s)\n", key)
	ctx := ctxArg.(*zedrouterContext)
	status := lookupAppNetworkStatus(ctx, key)
	if status == nil {
		log.Printf("handleAppNetworkConfigDelete: unknown %s\n", key)
		return
	}
	handleDelete(ctx, key, status)
	log.Printf("handleAppNetworkConfigDelete(%s) done\n", key)
}

// Callers must be careful to publish any changes to AppNetworkStatus
func lookupAppNetworkStatus(ctx *zedrouterContext, key string) *types.AppNetworkStatus {

	pub := ctx.pubAppNetworkStatus
	st, _ := pub.Get(key)
	if st == nil {
		log.Printf("lookupAppNetworkStatus(%s) not found\n", key)
		return nil
	}
	status := cast.CastAppNetworkStatus(st)
	if status.Key() != key {
		log.Printf("lookupAppNetworkStatus key/UUID mismatch %s vs %s; ignored %+v\n",
			key, status.Key(), status)
		return nil
	}
	return &status
}

func lookupAppNetworkConfig(ctx *zedrouterContext, key string) *types.AppNetworkConfig {

	sub := ctx.subAppNetworkConfig
	c, _ := sub.Get(key)
	if c == nil {
		sub = ctx.subAppNetworkConfigAg
		c, _ = sub.Get(key)
		if c == nil {
			log.Printf("lookupAppNetworkConfig(%s) not found\n", key)
			return nil
		}
	}
	config := cast.CastAppNetworkConfig(c)
	if config.Key() != key {
		log.Printf("lookupAppNetworkConfig key/UUID mismatch %s vs %s; ignored %+v\n",
			key, config.Key(), config)
		return nil
	}
	return &config
}

// Track the device information so we can annotate the application EIDs
// Note that when we start with zedrouter config files in place the
// device one might be processed after application ones, in which case these
// empty. This results in less additional info recorded in the map servers.
// XXX note that this only works well when the IsZedmanager AppNetworkConfig
// arrives first so that these fields are filled in before other
// AppNetworkConfig entries are processed.
var deviceEID net.IP
var deviceIID uint32
var additionalInfoDevice *types.AdditionalInfoDevice

func handleCreate(ctx *zedrouterContext, key string,
	config types.AppNetworkConfig) {
	log.Printf("handleCreate(%v) for %s\n",
		config.UUIDandVersion, config.DisplayName)

	// Pick a local number to identify the application instance
	// Used for IP addresses as well bridge and file names.
	appNum := appNumAllocate(config.UUIDandVersion.UUID,
		config.IsZedmanager)

	// Start by marking with PendingAdd
	status := types.AppNetworkStatus{
		UUIDandVersion: config.UUIDandVersion,
		AppNum:         appNum,
		PendingAdd:     true,
		OlNum:          len(config.OverlayNetworkList),
		UlNum:          len(config.UnderlayNetworkList),
		DisplayName:    config.DisplayName,
		IsZedmanager:   config.IsZedmanager,
	}
	publishAppNetworkStatus(ctx, &status)

	if config.IsZedmanager {
		log.Printf("handleCreate: for %s IsZedmanager\n",
			config.DisplayName)
		if len(config.OverlayNetworkList) != 1 ||
			len(config.UnderlayNetworkList) != 0 {
			// XXX report IsZedmanager error to cloud?
			err := errors.New("Malformed IsZedmanager config; ignored")
			status.PendingAdd = false
			addError(ctx, &status, "IsZedmanager", err)
			log.Printf("handleCreate done for %s\n",
				config.DisplayName)
			return
		}
		ctx.separateDataPlane = config.SeparateDataPlane

		// Use this olIfname to name files
		// XXX some files might not be used until Zedmanager becomes
		// a domU at which point IsZedMansger boolean won't be needed
		olConfig := config.OverlayNetworkList[0]
		olNum := 1
		olIfname := "dbo" + strconv.Itoa(olNum) + "x" +
			strconv.Itoa(appNum)
		// Assume there is no UUID for management overlay

		// Create olIfname dummy interface with EID and fd00::/8 route
		// pointing at it.
		// XXX also a separate route for eidAllocationPrefix if global

		// Start clean
		attrs := netlink.NewLinkAttrs()
		attrs.Name = olIfname
		oLink := &netlink.Dummy{LinkAttrs: attrs}
		netlink.LinkDel(oLink)

		//    ip link add ${olIfname} type dummy
		attrs = netlink.NewLinkAttrs()
		attrs.Name = olIfname
		// Note: we ignore olConfig.AppMacAddr for IsMgmt
		olIfMac := fmt.Sprintf("00:16:3e:02:%02x:%02x", olNum, appNum)
		hw, err := net.ParseMAC(olIfMac)
		if err != nil {
			log.Fatal("ParseMAC failed: ", olIfMac, err)
		}
		attrs.HardwareAddr = hw
		oLink = &netlink.Dummy{LinkAttrs: attrs}
		if err := netlink.LinkAdd(oLink); err != nil {
			errStr := fmt.Sprintf("LinkAdd on %s failed: %s",
				olIfname, err)
			addError(ctx, &status, "IsZedmanager",
				errors.New(errStr))
		}

		// ip link set ${olIfname} mtu 1280
		if err := netlink.LinkSetMTU(oLink, 1280); err != nil {
			errStr := fmt.Sprintf("LinkSetMTU on %s failed: %s",
				olIfname, err)
			addError(ctx, &status, "IsZedmanager",
				errors.New(errStr))
		}

		//    ip link set ${olIfname} up
		if err := netlink.LinkSetUp(oLink); err != nil {
			errStr := fmt.Sprintf("LinkSetUp on %s failed: %s",
				olIfname, err)
			addError(ctx, &status, "IsZedmanager",
				errors.New(errStr))
		}

		//    ip link set ${olIfname} arp on
		if err := netlink.LinkSetARPOn(oLink); err != nil {
			errStr := fmt.Sprintf("LinkSetARPOn on %s failed: %s",
				olIfname, err)
			addError(ctx, &status, "IsZedmanager",
				errors.New(errStr))
		}

		// Configure the EID on olIfname and set up a default route
		// for all fd00 EIDs
		//    ip addr add ${EID}/128 dev ${olIfname}
		EID := olConfig.EID
		addr, err := netlink.ParseAddr(EID.String() + "/128")
		if err != nil {
			errStr := fmt.Sprintf("ParseAddr %s failed: %s",
				EID, err)
			status.PendingAdd = false
			addError(ctx, &status, "IsZedmanager",
				errors.New(errStr))
			log.Printf("handleCreate done for %s\n",
				config.DisplayName)
			return
		}
		if err := netlink.AddrAdd(oLink, addr); err != nil {
			errStr := fmt.Sprintf("AddrAdd %s failed: %s", EID, err)
			addError(ctx, &status, "IsZedmanager",
				errors.New(errStr))
		}

		//    ip route add fd00::/8 via fe80::1 dev $intf
		index := oLink.Attrs().Index
		_, ipnet, err := net.ParseCIDR("fd00::/8")
		if err != nil {
			log.Fatal("ParseCIDR fd00::/8 failed:\n", err)
		}
		via := net.ParseIP("fe80::1")
		if via == nil {
			log.Fatal("ParseIP fe80::1 failed: ", err)
		}
		// Need to do both an add and a change since we could have
		// a FAILED neighbor entry from a previous run and a down
		// uplink interface.
		//    ip nei add fe80::1 lladdr 00:16:3e:02:01:00 dev $intf
		//    ip nei change fe80::1 lladdr 00:16:3e:02:01:00 dev $intf
		neigh := netlink.Neigh{LinkIndex: index, IP: via,
			HardwareAddr: hw, State: netlink.NUD_PERMANENT}
		if err := netlink.NeighAdd(&neigh); err != nil {
			errStr := fmt.Sprintf("NeighAdd fe80::1 failed: %s",
				err)
			addError(ctx, &status, "IsZedmanager",
				errors.New(errStr))
		}
		if err := netlink.NeighSet(&neigh); err != nil {
			errStr := fmt.Sprintf("NeighSet fe80::1 failed: %s",
				err)
			addError(ctx, &status, "IsZedmanager",
				errors.New(errStr))
		}

		rt := netlink.Route{Dst: ipnet, LinkIndex: index, Gw: via}
		if err := netlink.RouteAdd(&rt); err != nil {
			errStr := fmt.Sprintf("RouteAdd fd00::/8 failed: %s",
				err)
			addError(ctx, &status, "IsZedmanager",
				errors.New(errStr))
		}

		// XXX NOTE: this hosts file is not read!
		// XXX easier when Zedmanager is in separate domU!
		// Create a hosts file for the overlay based on DnsNameToIPList
		// Directory is /var/run/zedrouter/hosts.${OLIFNAME}
		// Each hostname in a separate file in directory to facilitate
		// adds and deletes
		hostsDirpath := globalRunDirname + "/hosts." + olIfname
		deleteHostsConfiglet(hostsDirpath, false)
		createHostsConfiglet(hostsDirpath, olConfig.MgmtDnsNameToIPList)

		// Default ipset
		deleteDefaultIpsetConfiglet(olIfname, false)
		createDefaultIpsetConfiglet(olIfname, olConfig.MgmtDnsNameToIPList,
			EID.String())

		// Set up ACLs
		err = createACLConfiglet(olIfname, olIfname, true, olConfig.ACLs,
			6, "", "")
		if err != nil {
			addError(ctx, &status, "createACL", err)
		}

		// Save information about zedmanger EID and additional info
		deviceEID = EID
		deviceIID = olConfig.MgmtIID
		additionalInfoDevice = olConfig.AdditionalInfoDevice

		additionalInfo := generateAdditionalInfo(status, olConfig)

		// Create LISP configlets for IID and EID/signature
		createLispConfiglet(lispRunDirname, config.IsZedmanager,
			olConfig.MgmtIID, olConfig.EID, olConfig.LispSignature,
			*ctx.DeviceNetworkStatus, olIfname, olIfname,
			additionalInfo, olConfig.MgmtMapServers,
			ctx.separateDataPlane)
		status.OverlayNetworkList = make([]types.OverlayNetworkStatus,
			len(config.OverlayNetworkList))
		for i, _ := range config.OverlayNetworkList {
			status.OverlayNetworkList[i].OverlayNetworkConfig =
				config.OverlayNetworkList[i]
			// XXX set BridgeName, BridgeIPAddr?
		}
		status.PendingAdd = false
		publishAppNetworkStatus(ctx, &status)
		log.Printf("handleCreate done for %s\n", config.DisplayName)
		return
	}

	// Check that Network exists for all overlays and underlays.
	// XXX if not, for now just delete status and the periodic walk will
	// retry
	allNetworksExist := true
	for _, olConfig := range config.OverlayNetworkList {
		netconfig := lookupNetworkObjectConfig(ctx,
			olConfig.Network.String())
		if netconfig != nil {
			continue
		}
		log.Printf("handleCreate(%v) for %s: missing overlay network %s\n",
			config.UUIDandVersion, config.DisplayName,
			olConfig.Network.String())
		allNetworksExist = false
	}
	for _, ulConfig := range config.UnderlayNetworkList {
		netconfig := lookupNetworkObjectConfig(ctx,
			ulConfig.Network.String())
		if netconfig != nil {
			continue
		}
		log.Printf("handleCreate(%v) for %s: missing underlay network %s\n",
			config.UUIDandVersion, config.DisplayName,
			ulConfig.Network.String())
		allNetworksExist = false
	}
	if !allNetworksExist {
		log.Printf("handleCreate(%v) for %s: missing networks XXX defer\n",
			config.UUIDandVersion, config.DisplayName)
		unpublishAppNetworkStatus(ctx, &status)
		return
	}

	olcount := len(config.OverlayNetworkList)
	if olcount > 0 {
		log.Printf("Received olcount %d\n", olcount)
	}
	status.OverlayNetworkList = make([]types.OverlayNetworkStatus,
		olcount)
	for i, _ := range config.OverlayNetworkList {
		status.OverlayNetworkList[i].OverlayNetworkConfig =
			config.OverlayNetworkList[i]
	}
	ulcount := len(config.UnderlayNetworkList)
	status.UnderlayNetworkList = make([]types.UnderlayNetworkStatus,
		ulcount)
	for i, _ := range config.UnderlayNetworkList {
		status.UnderlayNetworkList[i].UnderlayNetworkConfig =
			config.UnderlayNetworkList[i]
	}

	// Note that with IPv4/IPv6/LISP interfaces the domU can do
	// dns lookups on either IPv4 and IPv6 on any interface, hence we
	// configure the ipsets for all the domU's interfaces/bridges.
	ipsets := compileAppInstanceIpsets(ctx, config.OverlayNetworkList,
		config.UnderlayNetworkList)

	for i, olConfig := range config.OverlayNetworkList {
		olNum := i + 1
		if debug {
			log.Printf("olNum %d network %s ACLs %v\n",
				olNum, olConfig.Network.String(), olConfig.ACLs)
		}
		netconfig := lookupNetworkObjectConfig(ctx, olConfig.Network.String())
		if netconfig == nil {
			// Checked for nil above
			return
		}

		// Fetch the network that this overlay is attached to
		netstatus := lookupNetworkObjectStatus(ctx,
			olConfig.Network.String())
		if netstatus == nil {
			// We had a netconfig but no status!
			errStr := fmt.Sprintf("no network status for %s",
				olConfig.Network.String())
			err := errors.New(errStr)
			addError(ctx, &status, "lookupNetworkObjectStatus", err)
			continue
		}
		bridgeNum := netstatus.BridgeNum
		bridgeName := netstatus.BridgeName
		vifName := "nbo" + strconv.Itoa(olNum) + "x" +
			strconv.Itoa(bridgeNum)

		oLink, err := findBridge(bridgeName)
		if err != nil {
			status.PendingAdd = false
			addError(ctx, &status, "findBridge", err)
			log.Printf("handleCreate done for %s\n",
				config.DisplayName)
			return
		}
		bridgeMac := oLink.HardwareAddr
		log.Printf("bridgeName %s MAC %s\n",
			bridgeName, bridgeMac.String())

		var appMac string // Handed to domU
		if olConfig.AppMacAddr != nil {
			appMac = olConfig.AppMacAddr.String()
		} else {
			appMac = fmt.Sprintf("00:16:3e:01:%02x:%02x",
				olNum, appNum)
		}
		log.Printf("appMac %s\n", appMac)

		// Record what we have so far
		olStatus := &status.OverlayNetworkList[olNum-1]
		olStatus.Bridge = bridgeName
		olStatus.BridgeMac = bridgeMac
		olStatus.Vif = vifName
		olStatus.Mac = appMac
		olStatus.HostName = config.Key()

		olStatus.BridgeIPAddr = netstatus.BridgeIPAddr

		// XXX add isIPv6 check
		// XXX do we need an IPv4 in-subnet EID for route+dnsmasq?
		// BridgeIPAddr is set when network is up.
		EID := olConfig.EID
		//    ip -6 route add ${EID}/128 dev ${bridgeName}
		_, ipnet, err := net.ParseCIDR(EID.String() + "/128")
		if err != nil {
			errStr := fmt.Sprintf("ParseCIDR %s failed: %v",
				EID, err)
			addError(ctx, &status, "handleCreate",
				errors.New(errStr))
		}
		rt := netlink.Route{Dst: ipnet, LinkIndex: oLink.Index}
		if err := netlink.RouteAdd(&rt); err != nil {
			errStr := fmt.Sprintf("RouteAdd %s failed: %s",
				EID, err)
			addError(ctx, &status, "handleCreate",
				errors.New(errStr))
		}

		// Write our EID hostname in a separate file in directory to
		// facilitate adds and deletes
		hostsDirpath := globalRunDirname + "/hosts." + bridgeName
		addToHostsConfiglet(hostsDirpath, config.DisplayName,
			[]string{EID.String()})

		// Default ipset
		deleteDefaultIpsetConfiglet(vifName, false)
		createDefaultIpsetConfiglet(vifName, netstatus.DnsNameToIPList,
			EID.String())

		// Set up ACLs
		// XXX remove 6/4 arg? From bridgeIPAddr
		err = createACLConfiglet(bridgeName, vifName, false,
			olConfig.ACLs, 6, olStatus.BridgeIPAddr, EID.String())
		if err != nil {
			addError(ctx, &status, "createACL", err)
		}

		addhostDnsmasq(bridgeName, appMac, EID.String(),
			config.UUIDandVersion.UUID.String())

		// Look for added or deleted ipsets
		newIpsets, staleIpsets, restartDnsmasq := diffIpsets(ipsets,
			netstatus.BridgeIPSets)

		if restartDnsmasq && olStatus.BridgeIPAddr != "" {
			stopDnsmasq(bridgeName, false)
			createDnsmasqConfiglet(bridgeName,
				olStatus.BridgeIPAddr, netconfig, hostsDirpath,
				newIpsets)
			startDnsmasq(bridgeName)
		}
		addVifToBridge(netstatus, vifName)
		netstatus.BridgeIPSets = newIpsets
		publishNetworkObjectStatus(ctx, netstatus)

		maybeRemoveStaleIpsets(staleIpsets)

		// Create LISP configlets for IID and EID/signature
		serviceStatus := lookupAppLink(ctx, olConfig.Network)
		if serviceStatus == nil {
			// Lisp service might not have arrived as part of configuration.
			// Bail now and let the service activation take care of creating
			// Lisp configlets and re-start lispers.net
			log.Printf("handleCreate: Network %s is not attached to any service\n",
				netconfig.Key())
			continue
		}
		if serviceStatus.Activated == false {
			// Lisp service is not activate yet. Let the Lisp service activation
			// code take care of creating the Lisp configlets.
			log.Printf("handleCreate: Network service %s in not activated.\n",
				serviceStatus.Key())
			continue
		}

		createAndStartLisp(ctx, status, olConfig,
			serviceStatus, lispRunDirname, bridgeName)
	}

	for i, ulConfig := range config.UnderlayNetworkList {
		ulNum := i + 1
		if debug {
			log.Printf("ulNum %d network %s ACLs %v\n",
				ulNum, ulConfig.Network.String(), ulConfig.ACLs)
		}
		netconfig := lookupNetworkObjectConfig(ctx, ulConfig.Network.String())
		if netconfig == nil {
			// Checked for nil above
			return
		}

		// Fetch the network that this underlay is attached to
		netstatus := lookupNetworkObjectStatus(ctx,
			ulConfig.Network.String())
		if netstatus == nil {
			errStr := fmt.Sprintf("no status for %s",
				ulConfig.Network.String())
			err := errors.New(errStr)
			addError(ctx, &status, "lookupNetworkObjectStatus", err)
			continue
		}
		bridgeName := netstatus.BridgeName
		vifName := "nbu" + strconv.Itoa(ulNum) + "x" +
			strconv.Itoa(appNum)
		uLink, err := findBridge(bridgeName)
		if err != nil {
			status.PendingAdd = false
			addError(ctx, &status, "findBridge", err)
			log.Printf("handleCreate done for %s\n",
				config.DisplayName)
			return
		}
		bridgeMac := uLink.HardwareAddr
		log.Printf("bridgeName %s MAC %s\n",
			bridgeName, bridgeMac.String())

		var appMac string // Handed to domU
		if ulConfig.AppMacAddr != nil {
			appMac = ulConfig.AppMacAddr.String()
		} else {
			// Room to handle multiple underlays in 5th byte
			appMac = fmt.Sprintf("00:16:3e:00:%02x:%02x",
				ulNum, appNum)
		}
		log.Printf("appMac %s\n", appMac)

		// Record what we have so far
		ulStatus := &status.UnderlayNetworkList[ulNum-1]
		ulStatus.Bridge = bridgeName
		ulStatus.BridgeMac = bridgeMac
		ulStatus.Vif = vifName
		ulStatus.Mac = appMac
		ulStatus.HostName = config.Key()

		bridgeIPAddr, appIPAddr := getUlAddrs(ctx, ulNum-1, appNum,
			ulStatus, netstatus)
		// Check if we have a bridge service with an address
		bridgeIP, err := getBridgeServiceIPv4Addr(ctx, ulConfig.Network)
		if err != nil {
			log.Printf("handleCreate getBridgeServiceIPv4Addr %s\n",
				err)
		} else if bridgeIP != "" {
			bridgeIPAddr = bridgeIP
		}
		log.Printf("bridgeIPAddr %s appIPAddr %s\n", bridgeIPAddr, appIPAddr)
		ulStatus.BridgeIPAddr = bridgeIPAddr
		// XXX appIPAddr is "" if bridge service
		ulStatus.AssignedIPAddr = appIPAddr
		hostsDirpath := globalRunDirname + "/hosts." + bridgeName
		if appIPAddr != "" {
			addToHostsConfiglet(hostsDirpath, config.DisplayName,
				[]string{appIPAddr})
		}

		// Default ipset
		deleteDefaultIpsetConfiglet(vifName, false)
		createDefaultIpsetConfiglet(vifName, netstatus.DnsNameToIPList,
			appIPAddr)

		// Set up ACLs
		err = createACLConfiglet(bridgeName, vifName, false,
			ulConfig.ACLs, 4, bridgeIPAddr, appIPAddr)
		if err != nil {
			addError(ctx, &status, "createACL", err)
		}

		if appIPAddr != "" {
			addhostDnsmasq(bridgeName, appMac, appIPAddr,
				config.UUIDandVersion.UUID.String())
		}

		// Look for added or deleted ipsets
		newIpsets, staleIpsets, restartDnsmasq := diffIpsets(ipsets,
			netstatus.BridgeIPSets)

		if restartDnsmasq && ulStatus.BridgeIPAddr != "" {
			stopDnsmasq(bridgeName, false)
			createDnsmasqConfiglet(bridgeName,
				ulStatus.BridgeIPAddr, netconfig, hostsDirpath,
				newIpsets)
			startDnsmasq(bridgeName)
		}
		addVifToBridge(netstatus, vifName)
		netstatus.BridgeIPSets = newIpsets
		publishNetworkObjectStatus(ctx, netstatus)

		maybeRemoveStaleIpsets(staleIpsets)
	}
	// Write out what we created to AppNetworkStatus
	status.PendingAdd = false
	publishAppNetworkStatus(ctx, &status)
	log.Printf("handleCreate done for %s\n", config.DisplayName)
}

func createAndStartLisp(ctx *zedrouterContext,
	status types.AppNetworkStatus,
	olConfig types.OverlayNetworkConfig,
	serviceStatus *types.NetworkServiceStatus,
	lispRunDirname, bridgeName string) {

	if serviceStatus == nil {
		log.Printf("createAndStartLisp: No service configured yet\n")
		return
	}

	additionalInfo := generateAdditionalInfo(status, olConfig)
	adapters := getAdapters(ctx, serviceStatus.Adapter)
	adapterMap := make(map[string]bool)
	for _, adapter := range adapters {
		adapterMap[adapter] = true
	}
	deviceNetworkParams := types.DeviceNetworkStatus{}
	for _, uplink := range ctx.DeviceNetworkStatus.UplinkStatus {
		if _, ok := adapterMap[uplink.IfName]; ok == true {
			deviceNetworkParams.UplinkStatus =
				append(deviceNetworkParams.UplinkStatus, uplink)
		}
	}
	createLispEidConfiglet(lispRunDirname, serviceStatus.LispStatus.IID,
		olConfig.EID, olConfig.LispSignature, deviceNetworkParams,
		bridgeName, bridgeName, additionalInfo,
		serviceStatus.LispStatus.MapServers, ctx.separateDataPlane)
}

// Returns the link
func findBridge(bridgeName string) (*netlink.Bridge, error) {

	var bridgeLink *netlink.Bridge
	link, err := netlink.LinkByName(bridgeName)
	if link == nil {
		errStr := fmt.Sprintf("findBridge(%s) failed %s",
			bridgeName, err)
		// XXX how to handle this failure? bridge disappeared?
		return nil, errors.New(errStr)
	}
	switch link.(type) {
	case *netlink.Bridge:
		bridgeLink = link.(*netlink.Bridge)
	default:
		errStr := fmt.Sprintf("findBridge(%s) not a bridge %T",
			bridgeName, link)
		// XXX why wouldn't it be a bridge?
		return nil, errors.New(errStr)
	}
	return bridgeLink, nil
}

// XXX Need additional logic for IPv6 underlays.
func getUlAddrs(ctx *zedrouterContext, ifnum int, appNum int,
	status *types.UnderlayNetworkStatus,
	netstatus *types.NetworkObjectStatus) (string, string) {

	log.Printf("getUlAddrs(%d/%d)\n", ifnum, appNum)

	bridgeIPAddr := ""
	appIPAddr := ""

	// Allocate bridgeIPAddr based on BridgeMac
	log.Printf("getUlAddrs(%d/%d for %s) bridgeMac %s\n",
		ifnum, appNum, netstatus.UUID.String(),
		status.BridgeMac.String())
	addr, err := lookupOrAllocateIPv4(ctx, netstatus,
		status.BridgeMac)
	if err != nil {
		log.Printf("lookupOrAllocatePv4 failed %s\n", err)
	} else {
		bridgeIPAddr = addr
	}

	if status.AppIPAddr != nil {
		// Static IP assignment case.
		// Note that appIPAddr can be in a different subnet.
		// Assumption is that the config specifies a gateway/router
		// in the same subnet as the static address.
		appIPAddr = status.AppIPAddr.String()
	} else {
		// XXX or change type of VifInfo.Mac to avoid parsing?
		mac, err := net.ParseMAC(status.Mac)
		if err != nil {
			log.Fatal("ParseMAC failed: ", status.Mac, err)
		}
		log.Printf("getUlAddrs(%d/%d for %s) app Mac %s\n",
			ifnum, appNum, netstatus.UUID.String(), mac.String())
		addr, err := lookupOrAllocateIPv4(ctx, netstatus, mac)
		if err != nil {
			log.Printf("lookupOrAllocateIPv4 failed %s\n", err)
		} else {
			appIPAddr = addr
		}
	}
	log.Printf("getUlAddrs(%d/%d) done %s/%s\n",
		ifnum, appNum, bridgeIPAddr, appIPAddr)
	return bridgeIPAddr, appIPAddr
}

// Caller should clear the appropriate status.Pending* if the the caller will
// return after adding the error.
func addError(ctx *zedrouterContext,
	status *types.AppNetworkStatus, tag string, err error) {

	log.Printf("%s: %s\n", tag, err.Error())
	status.Error = appendError(status.Error, tag, err.Error())
	status.ErrorTime = time.Now()
	publishAppNetworkStatus(ctx, status)
}

func appendError(allErrors string, prefix string, lasterr string) string {
	return fmt.Sprintf("%s%s: %s\n\n", allErrors, prefix, lasterr)
}

// Note that handleModify will not touch the EID; just ACLs
// XXX should we check that nothing else has changed?
// XXX If so flag other changes as errors; would need lastError in status.
func handleModify(ctx *zedrouterContext, key string,
	config types.AppNetworkConfig, status *types.AppNetworkStatus) {

	log.Printf("handleModify(%v) for %s\n",
		config.UUIDandVersion, config.DisplayName)

	// No check for version numbers since the ACLs etc might change
	// even for the same version.

	appNum := status.AppNum
	if debug {
		log.Printf("handleModify appNum %d\n", appNum)
	}

	// Check for unsupported changes
	if config.IsZedmanager != status.IsZedmanager {
		errStr := fmt.Sprintf("Unsupported: IsZedmanager changed for %s",
			config.UUIDandVersion)
		status.PendingModify = false
		addError(ctx, status, "handleModify", errors.New(errStr))
		log.Printf("handleModify done for %s\n", config.DisplayName)
		return
	}
	// XXX We could should we allow the addition of interfaces
	// if the domU would find out through some hotplug event.
	// But deletion is hard.
	// For now don't allow any adds or deletes.
	if len(config.OverlayNetworkList) != status.OlNum {
		errStr := fmt.Sprintf("Unsupported: Changed number of overlays for %s",
			config.UUIDandVersion)
		status.PendingModify = false
		addError(ctx, status, "handleModify", errors.New(errStr))
		log.Printf("handleModify done for %s\n", config.DisplayName)
		return
	}
	if len(config.UnderlayNetworkList) != status.UlNum {
		errStr := fmt.Sprintf("Unsupported: Changed number of underlays for %s",
			config.UUIDandVersion)
		status.PendingModify = false
		addError(ctx, status, "handleModify", errors.New(errStr))
		log.Printf("handleModify done for %s\n", config.DisplayName)
		return
	}

	status.SeparateDataPlane = ctx.separateDataPlane
	status.PendingModify = true
	status.UUIDandVersion = config.UUIDandVersion
	publishAppNetworkStatus(ctx, status)

	if config.IsZedmanager {
		if config.SeparateDataPlane != ctx.separateDataPlane {
			errStr := fmt.Sprintf("Unsupported: Changing experimental data plane flag on the fly\n")

			status.PendingModify = false
			addError(ctx, status, "handleModify",
				errors.New(errStr))
			log.Printf("handleModify done for %s\n",
				config.DisplayName)
			return
		}
		olConfig := config.OverlayNetworkList[0]
		olStatus := status.OverlayNetworkList[0]
		olNum := 1
		olIfname := "dbo" + strconv.Itoa(olNum) + "x" +
			strconv.Itoa(appNum)
		// Assume there is no UUID for management overlay

		// Note: we ignore olConfig.AppMacAddr for IsMgmt

		// Update ACLs
		err := updateACLConfiglet(olIfname, olIfname, true, olStatus.ACLs,
			olConfig.ACLs, 6, "", "")
		if err != nil {
			addError(ctx, status, "updateACL", err)
		}
		status.PendingModify = false
		publishAppNetworkStatus(ctx, status)
		log.Printf("handleModify done for %s\n", config.DisplayName)
		return
	}

	// Note that with IPv4/IPv6/LISP interfaces the domU can do
	// dns lookups on either IPv4 and IPv6 on any interface, hence should
	// configure the ipsets for all the domU's interfaces/bridges.
	ipsets := compileAppInstanceIpsets(ctx, config.OverlayNetworkList,
		config.UnderlayNetworkList)

	// Look for ACL changes in overlay
	for i, olConfig := range config.OverlayNetworkList {
		olNum := i + 1
		if debug {
			log.Printf("handleModify olNum %d\n", olNum)
		}
		// Need to check that index exists
		if len(status.OverlayNetworkList) < olNum {
			log.Println("Missing status for overlay %d; can not modify\n",
				olNum)
			continue
		}
		olStatus := status.OverlayNetworkList[olNum-1]
		bridgeName := olStatus.Bridge

		netconfig := lookupNetworkObjectConfig(ctx,
			olConfig.Network.String())
		if netconfig == nil {
			errStr := fmt.Sprintf("no network config for %s",
				olConfig.Network.String())
			err := errors.New(errStr)
			addError(ctx, status, "lookupNetworkObjectConfig", err)
			continue
		}
		netstatus := lookupNetworkObjectStatus(ctx,
			olConfig.Network.String())
		if netstatus == nil {
			// We had a netconfig but no status!
			errStr := fmt.Sprintf("no network status for %s",
				olConfig.Network.String())
			err := errors.New(errStr)
			addError(ctx, status, "lookupNetworkObjectStatus", err)
			continue
		}

		// XXX could there be a change to AssignedIPv6Address aka EID?
		// If so updateACLConfiglet needs to know old and new

		err := updateACLConfiglet(bridgeName, olStatus.Vif, false,
			olStatus.ACLs, olConfig.ACLs, 6, olStatus.BridgeIPAddr,
			olConfig.EID.String())
		if err != nil {
			addError(ctx, status, "updateACL", err)
		}

		// Look for added or deleted ipsets
		newIpsets, staleIpsets, restartDnsmasq := diffIpsets(ipsets,
			netstatus.BridgeIPSets)

		if restartDnsmasq && olStatus.BridgeIPAddr != "" {
			hostsDirpath := globalRunDirname + "/hosts." + bridgeName
			stopDnsmasq(bridgeName, false)
			createDnsmasqConfiglet(bridgeName,
				olStatus.BridgeIPAddr, netconfig, hostsDirpath,
				newIpsets)
			startDnsmasq(bridgeName)
		}
		removeVifFromBridge(netstatus, olStatus.Vif)
		netstatus.BridgeIPSets = newIpsets
		publishNetworkObjectStatus(ctx, netstatus)

		maybeRemoveStaleIpsets(staleIpsets)

		serviceStatus := lookupAppLink(ctx, olConfig.Network)
		if serviceStatus == nil {
			// Lisp service might not have arrived as part of configuration.
			// Bail now and let the service activation take care of creating
			// Lisp configlets and re-start lispers.net
			continue
		}

		additionalInfo := generateAdditionalInfo(*status, olConfig)

		// Update any signature changes
		// XXX should we check that EID didn't change?

		// Create LISP configlets for IID and EID/signature
		// XXX shared with others???
		updateLispConfiglet(lispRunDirname, false,
			serviceStatus.LispStatus.IID,
			olConfig.EID, olConfig.LispSignature,
			*ctx.DeviceNetworkStatus, bridgeName, bridgeName,
			additionalInfo, serviceStatus.LispStatus.MapServers,
			ctx.separateDataPlane)
	}
	// Look for ACL changes in underlay
	for i, ulConfig := range config.UnderlayNetworkList {
		ulNum := i + 1
		if debug {
			log.Printf("handleModify ulNum %d\n", ulNum)
		}
		// Need to check that index exists
		if len(status.UnderlayNetworkList) < ulNum {
			log.Println("Missing status for underlay %d; can not modify\n",
				ulNum)
			continue
		}
		ulStatus := status.UnderlayNetworkList[ulNum-1]
		bridgeName := ulStatus.Bridge
		appIPAddr := ulStatus.AssignedIPAddr

		netconfig := lookupNetworkObjectConfig(ctx,
			ulConfig.Network.String())
		if netconfig == nil {
			errStr := fmt.Sprintf("no network config for %s",
				ulConfig.Network.String())
			err := errors.New(errStr)
			addError(ctx, status, "lookupNetworkObjectConfig", err)
			continue
		}
		netstatus := lookupNetworkObjectStatus(ctx,
			ulConfig.Network.String())
		if netstatus == nil {
			// We had a netconfig but no status!
			errStr := fmt.Sprintf("no network status for %s",
				ulConfig.Network.String())
			err := errors.New(errStr)
			addError(ctx, status, "lookupNetworkObjectStatus", err)
			continue
		}
		// XXX could there be a change to AssignedIPAddress?
		// If so updateNetworkACLConfiglet needs to know old and new
		err := updateACLConfiglet(bridgeName, ulStatus.Vif, false,
			ulStatus.ACLs, ulConfig.ACLs, 4, ulStatus.BridgeIPAddr,
			appIPAddr)
		if err != nil {
			addError(ctx, status, "updateACL", err)
		}

		newIpsets, staleIpsets, restartDnsmasq := diffIpsets(ipsets,
			netstatus.BridgeIPSets)

		if restartDnsmasq && ulStatus.BridgeIPAddr != "" {
			hostsDirpath := globalRunDirname + "/hosts." + bridgeName
			stopDnsmasq(bridgeName, false)
			createDnsmasqConfiglet(bridgeName,
				ulStatus.BridgeIPAddr, netconfig, hostsDirpath,
				newIpsets)
			startDnsmasq(bridgeName)
		}
		removeVifFromBridge(netstatus, ulStatus.Vif)
		netstatus.BridgeIPSets = newIpsets
		publishNetworkObjectStatus(ctx, netstatus)

		maybeRemoveStaleIpsets(staleIpsets)
	}

	// Write out what we modified to AppNetworkStatus
	status.OverlayNetworkList = make([]types.OverlayNetworkStatus,
		len(config.OverlayNetworkList))
	for i, _ := range config.OverlayNetworkList {
		status.OverlayNetworkList[i].OverlayNetworkConfig =
			config.OverlayNetworkList[i]
	}
	status.UnderlayNetworkList = make([]types.UnderlayNetworkStatus,
		len(config.UnderlayNetworkList))
	for i, _ := range config.UnderlayNetworkList {
		status.UnderlayNetworkList[i].UnderlayNetworkConfig =
			config.UnderlayNetworkList[i]
	}
	status.PendingModify = false
	publishAppNetworkStatus(ctx, status)
	log.Printf("handleModify done for %s\n", config.DisplayName)
}

func maybeRemoveStaleIpsets(staleIpsets []string) {
	// Remove stale ipsets
	// In case if there are any references to these ipsets from other
	// domUs, then the kernel would not remove them.
	// The ipset destroy command would just fail.
	for _, ipset := range staleIpsets {
		err := ipsetDestroy(fmt.Sprintf("ipv4.%s", ipset))
		if err != nil {
			log.Println("ipset destroy ipv4", ipset, err)
		}
		err = ipsetDestroy(fmt.Sprintf("ipv6.%s", ipset))
		if err != nil {
			log.Println("ipset destroy ipv6", ipset, err)
		}
	}
}

func handleDelete(ctx *zedrouterContext, key string,
	status *types.AppNetworkStatus) {

	log.Printf("handleDelete(%v) for %s\n",
		status.UUIDandVersion, status.DisplayName)

	appNum := status.AppNum
	maxOlNum := status.OlNum
	maxUlNum := status.UlNum
	if debug {
		log.Printf("handleDelete appNum %d maxOlNum %d maxUlNum %d\n",
			appNum, maxOlNum, maxUlNum)
	}

	status.PendingDelete = true
	publishAppNetworkStatus(ctx, status)

	if status.IsZedmanager {
		if len(status.OverlayNetworkList) != 1 ||
			len(status.UnderlayNetworkList) != 0 {
			errStr := "Malformed IsZedmanager status; ignored"
			status.PendingDelete = false
			addError(ctx, status, "handleDelete",
				errors.New(errStr))
			log.Printf("handleDelete done for %s\n",
				status.DisplayName)
			return
		}
		// Remove global state for device
		deviceEID = net.IP{}
		deviceIID = 0
		additionalInfoDevice = nil

		olNum := 1
		olStatus := &status.OverlayNetworkList[0]
		olIfname := "dbo" + strconv.Itoa(olNum) + "x" +
			strconv.Itoa(appNum)
		// Assume there is no UUID for management overlay

		// Delete the address from loopback
		// Delete fd00::/8 route
		// Delete fe80::1 neighbor

		//    ip addr del ${EID}/128 dev ${olIfname}
		EID := status.OverlayNetworkList[0].EID
		addr, err := netlink.ParseAddr(EID.String() + "/128")
		if err != nil {
			errStr := fmt.Sprintf("ParseAddr %s failed: %s",
				EID, err)
			status.PendingDelete = false
			addError(ctx, status, "handleDelete",
				errors.New(errStr))
			log.Printf("handleDelete done for %s\n",
				status.DisplayName)
			return
		}
		attrs := netlink.NewLinkAttrs()
		attrs.Name = olIfname
		oLink := &netlink.Dummy{LinkAttrs: attrs}
		// XXX can we skip explicit deletes and just remove the oLink?
		if err := netlink.AddrDel(oLink, addr); err != nil {
			errStr := fmt.Sprintf("AddrDel %s failed: %s",
				EID, err)
			addError(ctx, status, "handleDelete",
				errors.New(errStr))
		}

		//    ip route del fd00::/8 via fe80::1 dev $intf
		index := oLink.Attrs().Index
		_, ipnet, err := net.ParseCIDR("fd00::/8")
		if err != nil {
			log.Fatal("ParseCIDR fd00::/8 failed:\n", err)
		}
		via := net.ParseIP("fe80::1")
		if via == nil {
			log.Fatal("ParseIP fe80::1 failed: ", err)
		}
		rt := netlink.Route{Dst: ipnet, LinkIndex: index, Gw: via}
		if err := netlink.RouteDel(&rt); err != nil {
			errStr := fmt.Sprintf("RouteDel fd00::/8 failed: %s",
				err)
			addError(ctx, status, "handleDelete",
				errors.New(errStr))
		}
		//    ip nei del fe80::1 lladdr 0:0:0:0:0:1 dev $intf
		neigh := netlink.Neigh{LinkIndex: index, IP: via}
		if err := netlink.NeighDel(&neigh); err != nil {
			errStr := fmt.Sprintf("NeighDel fe80::1 failed: %s",
				err)
			addError(ctx, status, "handleDelete",
				errors.New(errStr))
		}

		// Remove link and associated addresses
		netlink.LinkDel(oLink)

		// Delete overlay hosts file
		hostsDirpath := globalRunDirname + "/hosts." + olIfname
		deleteHostsConfiglet(hostsDirpath, true)

		// Default ipset
		deleteDefaultIpsetConfiglet(olIfname, true)

		// Delete ACLs
		err = deleteACLConfiglet(olIfname, olIfname, true, olStatus.ACLs,
			6, "", "")
		if err != nil {
			addError(ctx, status, "deleteACL", err)
		}

		// Delete LISP configlets
		deleteLispConfiglet(lispRunDirname, true, olStatus.MgmtIID,
			olStatus.EID, *ctx.DeviceNetworkStatus,
			ctx.separateDataPlane)
		status.PendingDelete = false
		publishAppNetworkStatus(ctx, status)

		// Write out what we modified to AppNetworkStatus aka delete
		unpublishAppNetworkStatus(ctx, status)

		appNumFree(status.UUIDandVersion.UUID)
		log.Printf("handleDelete done for %s\n", status.DisplayName)
		return
	}
	// Note that with IPv4/IPv6/LISP interfaces the domU can do
	// dns lookups on either IPv4 and IPv6 on any interface, hence should
	// configure the ipsets for all the domU's interfaces/bridges.
	// We skip our own contributions since we're going away
	ipsets := compileOldAppInstanceIpsets(ctx, status.OverlayNetworkList,
		status.UnderlayNetworkList, status.Key())

	// Delete everything for overlay
	for olNum := 1; olNum <= maxOlNum; olNum++ {
		if debug {
			log.Printf("handleDelete olNum %d\n", olNum)
		}
		// Need to check that index exists
		if len(status.OverlayNetworkList) < olNum {
			log.Println("Missing status for overlay %d; can not clean up\n",
				olNum)
			continue
		}

		olStatus := status.OverlayNetworkList[olNum-1]
		bridgeName := olStatus.Bridge

		netconfig := lookupNetworkObjectConfig(ctx,
			olStatus.Network.String())
		if netconfig == nil {
			errStr := fmt.Sprintf("no network config for %s",
				olStatus.Network.String())
			err := errors.New(errStr)
			addError(ctx, status, "lookupNetworkObjectStatus", err)
			continue
		}
		netstatus := lookupNetworkObjectStatus(ctx,
			olStatus.Network.String())
		if netstatus == nil {
			// We had a netconfig but no status!
			errStr := fmt.Sprintf("no network status for %s",
				olStatus.Network.String())
			err := errors.New(errStr)
			addError(ctx, status, "lookupNetworkObjectStatus", err)
			continue
		}

		// Delete ACLs
		err := deleteACLConfiglet(bridgeName, olStatus.Vif, false,
			olStatus.ACLs, 6, olStatus.BridgeIPAddr,
			olStatus.EID.String())
		if err != nil {
			addError(ctx, status, "deleteACL", err)
		}

		// Delete underlay hosts file for this app
		hostsDirpath := globalRunDirname + "/hosts." + bridgeName
		removeFromHostsConfiglet(hostsDirpath, status.DisplayName)

		deleteDefaultIpsetConfiglet(olStatus.Vif, true)

		// Look for added or deleted ipsets
		newIpsets, staleIpsets, restartDnsmasq := diffIpsets(ipsets,
			netstatus.BridgeIPSets)

		if restartDnsmasq && olStatus.BridgeIPAddr != "" {
			stopDnsmasq(bridgeName, false)
			createDnsmasqConfiglet(bridgeName,
				olStatus.BridgeIPAddr, netconfig, hostsDirpath,
				newIpsets)
			startDnsmasq(bridgeName)
		}
		netstatus.BridgeIPSets = newIpsets
		maybeRemoveStaleIpsets(staleIpsets)

		// If service does not exist overlays would not have been created
		serviceStatus := lookupAppLink(ctx, olStatus.Network)
		if serviceStatus == nil {
			// Lisp service might already have been deleted.
			// As part of Lisp service deletion, we delete all overlays.
			continue
		}

		// Delete LISP configlets
		deleteLispConfiglet(lispRunDirname, false,
			serviceStatus.LispStatus.IID, olStatus.EID,
			*ctx.DeviceNetworkStatus,
			ctx.separateDataPlane)
	}

	// XXX check if any IIDs are now unreferenced and delete them
	// XXX requires looking at all of configDir and statusDir

	// Delete everything in underlay
	for ulNum := 1; ulNum <= maxUlNum; ulNum++ {
		if debug {
			log.Printf("handleDelete ulNum %d\n", ulNum)
		}
		// Need to check that index exists
		if len(status.UnderlayNetworkList) < ulNum {
			log.Println("Missing status for underlay %d; can not clean up\n",
				ulNum)
			continue
		}
		ulStatus := status.UnderlayNetworkList[ulNum-1]
		bridgeName := ulStatus.Bridge

		netconfig := lookupNetworkObjectConfig(ctx,
			ulStatus.Network.String())
		if netconfig == nil {
			errStr := fmt.Sprintf("no network config for %s",
				ulStatus.Network.String())
			err := errors.New(errStr)
			addError(ctx, status, "lookupNetworkObjectConfig", err)
			continue
		}
		netstatus := lookupNetworkObjectStatus(ctx,
			ulStatus.Network.String())
		if netstatus == nil {
			// We had a netconfig but no status!
			errStr := fmt.Sprintf("no network status for %s",
				ulStatus.Network.String())
			err := errors.New(errStr)
			addError(ctx, status, "lookupNetworkObjectStatus", err)
			continue
		}

		// XXX or change type of VifInfo.Mac?
		mac, err := net.ParseMAC(ulStatus.Mac)
		if err != nil {
			log.Fatal("ParseMAC failed: ",
				ulStatus.Mac, err)
		}
		err = releaseIPv4(ctx, netstatus, mac)
		if err != nil {
			// XXX publish error?
			addError(ctx, status, "releaseIPv4", err)
		}

		appMac := ulStatus.Mac
		appIPAddr := ulStatus.AssignedIPAddr
		removehostDnsmasq(bridgeName, appMac, appIPAddr)

		err = deleteACLConfiglet(bridgeName, ulStatus.Vif, false,
			ulStatus.ACLs, 4, ulStatus.BridgeIPAddr, appIPAddr)
		if err != nil {
			addError(ctx, status, "deleteACL", err)
		}

		// Delete underlay hosts file for this app
		hostsDirpath := globalRunDirname + "/hosts." + bridgeName
		removeFromHostsConfiglet(hostsDirpath,
			status.DisplayName)
		// Look for added or deleted ipsets
		newIpsets, staleIpsets, restartDnsmasq := diffIpsets(ipsets,
			netstatus.BridgeIPSets)

		if restartDnsmasq && ulStatus.BridgeIPAddr != "" {
			stopDnsmasq(bridgeName, false)
			createDnsmasqConfiglet(bridgeName,
				ulStatus.BridgeIPAddr, netconfig, hostsDirpath,
				newIpsets)
			startDnsmasq(bridgeName)
		}
		netstatus.BridgeIPSets = newIpsets
		maybeRemoveStaleIpsets(staleIpsets)
	}
	status.PendingDelete = false
	publishAppNetworkStatus(ctx, status)

	// Write out what we modified to AppNetworkStatus aka delete
	unpublishAppNetworkStatus(ctx, status)

	appNumFree(status.UUIDandVersion.UUID)
	log.Printf("handleDelete done for %s\n", status.DisplayName)
}

func pkillUserArgs(userName string, match string, printOnError bool) {
	cmd := "pkill"
	args := []string{
		// XXX note that alpine does not support -u
		// XXX		"-u",
		// XXX		userName,
		"-f",
		match,
	}
	_, err := wrap.Command(cmd, args...).Output()
	if err != nil && printOnError {
		log.Printf("Command %v %v failed: %s\n", cmd, args, err)
	}
}

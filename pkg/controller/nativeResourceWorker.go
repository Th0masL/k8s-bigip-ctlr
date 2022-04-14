package controller

import (
	"context"
	"fmt"
	"github.com/F5Networks/k8s-bigip-ctlr/pkg/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sort"
	"strings"
	"sync"
	"time"

	routeapi "github.com/openshift/api/route/v1"

	log "github.com/F5Networks/k8s-bigip-ctlr/pkg/vlogger"
	"gopkg.in/yaml.v2"
	v1 "k8s.io/api/core/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"reflect"
)

// nativeResourceWorker starts the Custom Resource Worker.
func (ctlr *Controller) nativeResourceWorker() {
	log.Debugf("Starting Native Resource Worker")
	ctlr.setInitialServiceCount()
	ctlr.initialiseExtendedRouteConfig()
	for ctlr.processNativeResource() {
	}
}

// processNativeResource gets resources from the nativeResourceQueue and processes the resource
// depending  on its kind.
func (ctlr *Controller) processNativeResource() bool {
	key, quit := ctlr.nativeResourceQueue.Get()
	if quit {
		// The controller is shutting down.
		log.Debugf("Resource Queue is empty, Going to StandBy Mode")
		return false
	}
	var isRetryableError bool

	defer ctlr.nativeResourceQueue.Done(key)
	rKey := key.(*rqKey)
	log.Debugf("Processing Key: %v", rKey)

	// During Init time, just accumulate all the poolMembers by processing only services
	if ctlr.initState && rKey.kind != Namespace {
		if rKey.kind != Service {
			ctlr.nativeResourceQueue.AddRateLimited(key)
			return true
		}
		ctlr.initialSvcCount--
		if ctlr.initialSvcCount <= 0 {
			ctlr.initState = false
		}
	}

	// Check the type of resource and process accordingly.
	switch rKey.kind {

	case Route:
		route := rKey.rsc.(*routeapi.Route)
		// processRoutes knows when to delete a VS (in the event of global config update and route delete)
		// so should not trigger delete from here
		err := ctlr.processRoutes(route.Namespace, false)
		if err != nil {
			// TODO
			utilruntime.HandleError(fmt.Errorf("Sync %v failed with %v", key, err))
			isRetryableError = true
		}
	case ConfigMap:
		cm := rKey.rsc.(*v1.ConfigMap)
		err, ok := ctlr.processConfigMap(cm, rKey.rscDelete)
		if err != nil {
			utilruntime.HandleError(fmt.Errorf("Sync %v failed with %v", key, err))
			break
		}

		if !ok {
			isRetryableError = true
		}

	case Service:
		svc := rKey.rsc.(*v1.Service)

		_ = ctlr.processService(svc, nil, rKey.rscDelete)

		if svc.Spec.Type == v1.ServiceTypeLoadBalancer {
			err := ctlr.processLBServices(svc, rKey.rscDelete)
			if err != nil {
				// TODO
				utilruntime.HandleError(fmt.Errorf("Sync %v failed with %v", key, err))
				isRetryableError = true
			}
			break
		}
		if ctlr.initState {
			break
		}
		for _, rg := range getAffectedRouteGroups(svc) {
			ctlr.updatePoolMembersForRoutes(rg)
		}
	case Endpoints:
		ep := rKey.rsc.(*v1.Endpoints)
		svc := ctlr.getServiceForEndpoints(ep)
		// No Services are effected with the change in service.
		if nil == svc {
			break
		}

		_ = ctlr.processService(svc, ep, rKey.rscDelete)

		if svc.Spec.Type == v1.ServiceTypeLoadBalancer {
			err := ctlr.processLBServices(svc, rKey.rscDelete)
			if err != nil {
				// TODO
				utilruntime.HandleError(fmt.Errorf("Sync %v failed with %v", key, err))
				isRetryableError = true
			}
			break
		}
		for _, rg := range getAffectedRouteGroups(svc) {
			ctlr.updatePoolMembersForRoutes(rg)
		}

	case Namespace:
		ns := rKey.rsc.(*v1.Namespace)
		nsName := ns.ObjectMeta.Name
		if rKey.rscDelete {
			// TODO: Delete all the resource configs from the store

			ctlr.nrInformers[nsName].stop()
			ctlr.esInformers[nsName].stop()
			delete(ctlr.nrInformers, nsName)
			delete(ctlr.esInformers, nsName)
			ctlr.namespacesMutex.Lock()
			delete(ctlr.namespaces, nsName)
			ctlr.namespacesMutex.Unlock()
			log.Debugf("Removed Namespace: '%v' from CIS scope", nsName)
		} else {
			ctlr.namespacesMutex.Lock()
			ctlr.namespaces[nsName] = true
			ctlr.namespacesMutex.Unlock()
			_ = ctlr.addNamespacedInformers(nsName)
			ctlr.nrInformers[nsName].start()
			ctlr.esInformers[nsName].start()
			log.Debugf("Added Namespace: '%v' to CIS scope", nsName)
		}
	default:
		log.Errorf("Unknown resource Kind: %v", rKey.kind)
	}
	if isRetryableError {
		ctlr.nativeResourceQueue.AddRateLimited(key)
	} else {
		ctlr.nativeResourceQueue.Forget(key)
	}

	if ctlr.nativeResourceQueue.Len() == 0 && ctlr.resources.isConfigUpdated() {
		config := ResourceConfigRequest{
			ltmConfig:          ctlr.resources.getLTMConfigDeepCopy(),
			shareNodes:         ctlr.shareNodes,
			dnsConfig:          ctlr.resources.getGTMConfigCopy(),
			defaultRouteDomain: ctlr.defaultRouteDomain,
		}
		go ctlr.TeemData.PostTeemsData()
		config.reqId = ctlr.enqueueReq(config)
		ctlr.Agent.PostConfig(config)
		ctlr.initState = false
		ctlr.resources.updateCaches()
	}
	return true
}

func (ctlr *Controller) processRoutes(routeGroup string, triggerDelete bool) error {
	startTime := time.Now()
	namespace := routeGroup
	defer func() {
		endTime := time.Now()
		log.Debugf("Finished syncing RouteGroup/Namespace %v (%v)",
			routeGroup, endTime.Sub(startTime))
	}()

	extdSpec := ctlr.resources.getExtendedRouteSpec(routeGroup)
	if extdSpec == nil {
		return fmt.Errorf("extended Route Spec not available for RouteGroup/Namespace: %v", routeGroup)
	}

	routes := ctlr.getGroupedRoutes(routeGroup)

	if triggerDelete || len(routes) == 0 {
		// Delete all possible virtuals for this route group
		for _, portStruct := range getBasicVirtualPorts() {
			rsName := frameRouteVSName(routeGroup, extdSpec, portStruct)
			if ctlr.getVirtualServer(namespace, rsName) != nil {
				log.Debugf("Removing virtual %v belongs to RouteGroup: %v from Namespace: %v",
					rsName, routeGroup, namespace)
				ctlr.deleteVirtualServer(namespace, rsName)
			}
		}
		return nil
	}

	portStructs := getVirtualPortsForRoutes(routes)
	vsMap := make(ResourceMap)
	processingError := false

	for _, portStruct := range portStructs {
		rsName := frameRouteVSName(routeGroup, extdSpec, portStruct)

		// Delete rsCfg if it is HTTP port and the Route does not handle HTTPTraffic
		if portStruct.protocol == "http" && !doRoutesHandleHTTP(routes) {
			ctlr.deleteVirtualServer(routeGroup, rsName)
			continue
		}

		rsCfg := &ResourceConfig{}
		rsCfg.Virtual.Partition = routeGroup
		rsCfg.MetaData.ResourceType = VirtualServer
		rsCfg.Virtual.Enabled = true
		rsCfg.Virtual.Name = rsName
		rsCfg.MetaData.Protocol = portStruct.protocol
		rsCfg.Virtual.SetVirtualAddress(
			extdSpec.VServerAddr,
			portStruct.port,
		)
		rsCfg.MetaData.baseResources = make(map[string]string)
		rsCfg.IntDgMap = make(InternalDataGroupMap)
		rsCfg.IRulesMap = make(IRulesMap)
		rsCfg.customProfiles = make(map[SecretKey]CustomProfile)

		err := ctlr.handleRouteGroupExtendedSpec(rsCfg, extdSpec)

		if err != nil {
			processingError = true
			log.Errorf("%v", err)
			break
		}

		for _, rt := range routes {
			rsCfg.MetaData.baseResources[rt.Namespace+"/"+rt.Name] = Route
			err, servicePort := ctlr.getServicePort(rt)
			if err != nil {
				processingError = true
				log.Errorf("%v", err)
				break
			}
			err = ctlr.prepareResourceConfigFromRoute(rsCfg, rt, routeGroup, servicePort)
			if err != nil {
				processingError = true
				log.Errorf("%v", err)
				break
			}

			if isSecureRoute(rt) {
				//TLS Logic
				processed := ctlr.handleRouteTLS(rsCfg, rt, extdSpec, servicePort)
				if !processed {
					// Processing failed
					// Stop processing further routes
					processingError = true
					break
				}

				log.Debugf("Updated Route %s with TLSProfile", rt.ObjectMeta.Name)
			}
		}

		if processingError {
			log.Errorf("Unable to Process Route Group %s", routeGroup)
			break
		}

		// Save ResourceConfig in temporary Map
		vsMap[rsName] = rsCfg

		if ctlr.PoolMemberType == NodePort {
			ctlr.updatePoolMembersForNodePort(rsCfg, namespace)
		} else {
			ctlr.updatePoolMembersForCluster(rsCfg, namespace)
		}
	}

	if !processingError {
		for name, rscfg := range vsMap {
			rsMap := ctlr.resources.getPartitionResourceMap(routeGroup)
			rsMap[name] = rscfg
		}
	}

	return nil
}

func (ctlr *Controller) getGroupedRoutes(routeGroup string) []*routeapi.Route {
	// Get the route group
	orderedRoutes := ctlr.getOrderedRoutes(routeGroup)
	var assocRoutes []*routeapi.Route
	uniqueHostPathMap := map[string]struct{}{}
	for _, route := range orderedRoutes {
		// TODO: add combinations for a/b - svc weight ; valid svcs or not
		if _, found := uniqueHostPathMap[route.Spec.Host+route.Spec.Path]; found {
			log.Errorf(" Discarding route %v due to duplicate host %v, path %v combination", route.Name, route.Spec.Host, route.Spec.To)
			continue
		} else {
			uniqueHostPathMap[route.Spec.Host+route.Spec.Path] = struct{}{}
			assocRoutes = append(assocRoutes, route)
		}
	}
	return assocRoutes
}

func (ctlr *Controller) handleRouteGroupExtendedSpec(rsCfg *ResourceConfig, extdSpec *ExtendedRouteGroupSpec) error {
	if extdSpec.SNAT == "" {
		rsCfg.Virtual.SNAT = DEFAULT_SNAT
	} else {
		rsCfg.Virtual.SNAT = extdSpec.SNAT
	}
	rsCfg.Virtual.WAF = extdSpec.WAF
	rsCfg.Virtual.IRules = extdSpec.IRules
	return nil
}

// gets the target port for the route
// if targetPort is set to IntVal, it's used directly
// otherwise the port is fetched from the associated service
func (ctlr *Controller) getServicePort(
	route *routeapi.Route,
) (error, int32) {
	log.Debugf("Finding port for route %v", route.Name)
	var err error
	var port int32
	nrInf, ok := ctlr.getNamespacedEssentialInformer(route.Namespace)
	if !ok {
		return fmt.Errorf("Informer not found for namespace: %v", route.Namespace), port
	}
	svcIndexer := nrInf.svcInformer.GetIndexer()
	svcName := route.Spec.To.Name
	if route.Spec.Port != nil {
		strVal := route.Spec.Port.TargetPort.StrVal
		if strVal == "" {
			port = route.Spec.Port.TargetPort.IntVal
		} else {
			port, err = resource.GetServicePort(route.Namespace, svcName, svcIndexer, strVal, resource.ResourceTypeRoute)
			if nil != err {
				return fmt.Errorf("Error while processing port for route %s: %v", route.Name, err), port
			}
		}
	} else {
		port, err = resource.GetServicePort(route.Namespace, svcName, svcIndexer, "", resource.ResourceTypeRoute)
		if nil != err {
			return fmt.Errorf("Error while processing port for route %s: %v", route.Name, err), port

		}
	}
	log.Debugf("Port %v found for route %s", port, route.Name)
	return nil, port

}

func (ctlr *Controller) prepareResourceConfigFromRoute(
	rsCfg *ResourceConfig,
	route *routeapi.Route,
	routeGroup string,
	servicePort int32,
) error {

	rsCfg.MetaData.hosts = append(rsCfg.MetaData.hosts, route.Spec.Host)

	pool := Pool{
		Name: formatPoolName(
			route.Namespace,
			route.Spec.To.Name,
			servicePort,
			"",
		),
		Partition:       rsCfg.Virtual.Partition,
		ServiceName:     route.Spec.To.Name,
		ServicePort:     servicePort,
		NodeMemberLabel: "",
	}

	rsCfg.Pools = append(rsCfg.Pools, pool)

	rules := ctlr.prepareRouteLTMRules(route, routeGroup, pool.Name)
	if rules == nil {
		return fmt.Errorf("failed to create LTM Rules")
	}

	policyName := formatPolicyName(route.Spec.Host, routeGroup, rsCfg.Virtual.Name)

	rsCfg.AddRuleToPolicy(policyName, routeGroup, rules)

	return nil
}

// prepareRouteLTMRules prepares LTM Policy rules for VirtualServer
func (ctlr *Controller) prepareRouteLTMRules(
	route *routeapi.Route,
	routeGroup string,
	poolName string,
) *Rules {
	rlMap := make(ruleMap)
	wildcards := make(ruleMap)

	uri := route.Spec.Host + route.Spec.Path
	path := route.Spec.Path

	ruleName := formatVirtualServerRuleName(route.Spec.Host, routeGroup, path, poolName)

	event := HTTPRequest

	rl, err := createRule(uri, poolName, ruleName, event)
	if nil != err {
		log.Errorf("Error configuring rule: %v", err)
		return nil
	}

	if strings.HasPrefix(uri, "*.") == true {
		wildcards[uri] = rl
	} else {
		rlMap[uri] = rl
	}

	var wg sync.WaitGroup
	wg.Add(2)

	sortrules := func(r ruleMap, rls *Rules, ordinal int) {
		for _, v := range r {
			*rls = append(*rls, v)
		}
		//sort.Sort(sort.Reverse(*rls))
		for _, v := range *rls {
			v.Ordinal = ordinal
			ordinal++
		}
		wg.Done()
	}

	rls := Rules{}
	go sortrules(rlMap, &rls, 0)

	w := Rules{}
	go sortrules(wildcards, &w, len(rlMap))

	wg.Wait()

	rls = append(rls, w...)
	sort.Sort(rls)

	return &rls
}

func (ctlr *Controller) updatePoolMembersForRoutes(routeGroup string) {
	extdSpec := ctlr.resources.getExtendedRouteSpec(routeGroup)
	if extdSpec == nil {
		//log.Debugf("extended Route Spec not available for RouteGroup/Namespace: %v", routeGroup)
		return
	}
	namespace := routeGroup
	for _, portStruct := range getBasicVirtualPorts() {
		rsName := frameRouteVSName(routeGroup, extdSpec, portStruct)
		rsCfg := ctlr.getVirtualServer(namespace, rsName)
		if rsCfg == nil {
			continue
		}
		freshRsCfg := &ResourceConfig{}
		freshRsCfg.copyConfig(rsCfg)
		if ctlr.PoolMemberType == NodePort {
			ctlr.updatePoolMembersForNodePort(freshRsCfg, namespace)
		} else {
			ctlr.updatePoolMembersForCluster(freshRsCfg, namespace)
		}
		_ = ctlr.resources.setResourceConfig(namespace, rsName, freshRsCfg)
	}
}

func (ctlr *Controller) initialiseExtendedRouteConfig() {
	splits := strings.Split(ctlr.routeSpecCMKey, "/")
	ns, cmName := splits[0], splits[1]
	cm, err := ctlr.kubeClient.CoreV1().ConfigMaps(ns).Get(context.TODO(), cmName, metav1.GetOptions{})
	if err != nil {
		log.Errorf("Unable to Get Extended Route Spec Config Map: %v, %v", ctlr.routeSpecCMKey, err)
	}
	err, _ = ctlr.processConfigMap(cm, false)
	if err != nil {
		log.Errorf("Unable to Process Extended Route Spec Config Map: %v, %v", ctlr.routeSpecCMKey, err)
	}
}

func (ctlr *Controller) processConfigMap(cm *v1.ConfigMap, isDelete bool) (error, bool) {
	startTime := time.Now()
	defer func() {
		endTime := time.Now()
		log.Debugf("Finished syncing local extended spec configmap: %v/%v (%v)",
			cm.Namespace, cm.Name, endTime.Sub(startTime))
	}()

	ersData := cm.Data
	es := extendedSpec{}

	//log.Debugf("GCM: %v", cm.Data)

	err := yaml.UnmarshalStrict([]byte(ersData["extendedSpec"]), &es)
	if err != nil {
		return fmt.Errorf("invalid extended route spec in configmap: %v/%v", cm.Namespace, cm.Name), false
	}

	newExtdSpecMap := make(extendedSpecMap, len(ctlr.resources.extdSpecMap))

	if ctlr.isGlobalExtendedRouteSpec(cm) {
		for rg := range es.ExtendedRouteGroupConfigs {
			// ergc needs to be created at every iteration, as we are using address inside this container

			// if this were used as an iteration variable, on every loop we just use the same container instead of creating one
			// using the same container overrides the previous iteration contents, which is not desired
			ergc := es.ExtendedRouteGroupConfigs[rg]
			newExtdSpecMap[ergc.Namespace] = &extendedParsedSpec{
				override: ergc.AllowOverride,
				local:    nil,
				global:   &ergc.ExtendedRouteGroupSpec,
			}
		}

		// Global configmap once gets processed even before processing other native resources
		if ctlr.initState {
			ctlr.resources.extdSpecMap = newExtdSpecMap
			return nil, true
		}

		// deletedSpecs: the spec blocks are deleted from the configmap
		// modifiedSpecs: specific params of spec entry are changed because of which virutals need to be deleted and framed again
		// updatedSpecs: parameters are updated, so just reprocess the resources
		// createSpecs: new spec blocks are added to the configmap
		var deletedSpecs, modifiedSpecs, updatedSpecs, createdSpecs []string

		if isDelete {
			for ns := range newExtdSpecMap {
				deletedSpecs = append(deletedSpecs, ns)
			}
		} else {
			for ns, spec := range ctlr.resources.extdSpecMap {
				newSpec, ok := newExtdSpecMap[ns]
				if !ok {
					deletedSpecs = append(deletedSpecs, ns)
					continue
				}
				if !reflect.DeepEqual(spec, newExtdSpecMap[ns]) {
					if spec.global.VServerName != newSpec.global.VServerName || spec.override != newSpec.override {
						// Update to VServerName or override should trigger delete and recreation of object
						modifiedSpecs = append(deletedSpecs, ns)
					} else {
						updatedSpecs = append(modifiedSpecs, ns)
					}
				}
			}
			for ns, _ := range newExtdSpecMap {
				_, ok := ctlr.resources.extdSpecMap[ns]
				if !ok {
					createdSpecs = append(createdSpecs, ns)
				}
			}
		}

		for _, ns := range deletedSpecs {
			_ = ctlr.processRoutes(ns, true)
			if ctlr.resources.extdSpecMap[ns].local == nil {
				delete(ctlr.resources.extdSpecMap, ns)
			} else {
				ctlr.resources.extdSpecMap[ns].global = nil
				ctlr.resources.extdSpecMap[ns].override = false
			}
		}

		for _, ns := range modifiedSpecs {
			_ = ctlr.processRoutes(ns, true)
			ctlr.resources.extdSpecMap[ns].override = newExtdSpecMap[ns].override
			ctlr.resources.extdSpecMap[ns].global = newExtdSpecMap[ns].global
			err := ctlr.processRoutes(ns, false)
			if err != nil {
				log.Errorf("Failed to process RouteGroup: %v with modified extended spec", ns)
			}
		}

		for _, ns := range updatedSpecs {
			ctlr.resources.extdSpecMap[ns].override = newExtdSpecMap[ns].override
			ctlr.resources.extdSpecMap[ns].global = newExtdSpecMap[ns].global
			err := ctlr.processRoutes(ns, false)
			if err != nil {
				log.Errorf("Failed to process RouteGroup: %v with updated extended spec", ns)
			}
		}

		for _, ns := range createdSpecs {
			ctlr.resources.extdSpecMap[ns] = &extendedParsedSpec{}
			ctlr.resources.extdSpecMap[ns].override = newExtdSpecMap[ns].override
			ctlr.resources.extdSpecMap[ns].global = newExtdSpecMap[ns].global
			err := ctlr.processRoutes(ns, false)
			if err != nil {
				log.Errorf("Failed to process RouteGroup: %v on addition of extended spec", ns)
			}
		}

	} else if len(es.ExtendedRouteGroupConfigs) > 0 {
		ergc := es.ExtendedRouteGroupConfigs[0]
		if ergc.Namespace != cm.Namespace {
			return fmt.Errorf("Invalid Extended Route Spec Block in configmap: %v/%v", cm.Namespace, cm.Name), true
		}
		if spec, ok := ctlr.resources.extdSpecMap[ergc.Namespace]; ok {
			if isDelete {
				if !spec.override {
					spec.local = nil
					return nil, true
				}
				_ = ctlr.processRoutes(ergc.Namespace, true)
				spec.local = nil
				// process routes again, this time routes get processed along with global config
				err := ctlr.processRoutes(ergc.Namespace, false)
				if err != nil {
					log.Errorf("Failed to process RouteGroup: %v on with global extended spec after deletion of local extended spec", ergc.Namespace)
				}
				return nil, true
			}

			if !spec.override || spec.global == nil {
				spec.local = &ergc.ExtendedRouteGroupSpec
				return nil, true
			}
			// creation event
			if spec.local == nil {
				if !reflect.DeepEqual(*(spec.global), ergc.ExtendedRouteGroupSpec) {
					if spec.global.VServerName != ergc.ExtendedRouteGroupSpec.VServerName {
						// Delete existing virtual that was framed with globla config
						// later build new virtual with local config
						_ = ctlr.processRoutes(ergc.Namespace, true)
					}
					spec.local = &ergc.ExtendedRouteGroupSpec
					err := ctlr.processRoutes(ergc.Namespace, false)
					if err != nil {
						log.Errorf("Failed to process RouteGroup: %v on addition of extended spec", ergc.Namespace)
					}
				}
				return nil, true
			}

			// update event
			if !reflect.DeepEqual(*(spec.local), ergc.ExtendedRouteGroupSpec) {
				// if update event, update to VServerName should trigger delete and recreation of object
				if spec.local.VServerName != ergc.ExtendedRouteGroupSpec.VServerName {
					_ = ctlr.processRoutes(ergc.Namespace, true)
				}
				spec.local = &ergc.ExtendedRouteGroupSpec
				err := ctlr.processRoutes(ergc.Namespace, false)
				if err != nil {
					log.Errorf("Failed to process RouteGroup: %v on addition of extended spec", ergc.Namespace)
				}
				return nil, true
			}

		} else {
			// Need not process routes as there is no confirmation of override yet
			ctlr.resources.extdSpecMap[ergc.Namespace] = &extendedParsedSpec{
				override: false,
				local:    &ergc.ExtendedRouteGroupSpec,
				global:   nil,
			}
			return nil, false
		}
	}

	return nil, true
}

func (ctlr *Controller) isGlobalExtendedRouteSpec(cm *v1.ConfigMap) bool {
	cmKey := cm.Namespace + "/" + cm.Name

	if cmKey == ctlr.routeSpecCMKey {
		return true
	}

	return false
}

func (ctlr *Controller) getOrderedRoutes(namespace string) []*routeapi.Route {
	var resources []interface{}
	var err error
	var allRoutes []*routeapi.Route

	nrInf, ok := ctlr.getNamespacedNativeInformer(namespace)
	if !ok {
		log.Errorf("Informer not found for namespace: %v", namespace)
		return nil
	}

	if namespace == "" {
		resources = nrInf.routeInformer.GetIndexer().List()
	} else {
		// Get list of Routes and process them.
		resources, err = nrInf.routeInformer.GetIndexer().ByIndex("namespace", namespace)
		if err != nil {
			log.Errorf("Unable to get list of Routes for namespace '%v': %v",
				namespace, err)
			return nil
		}
	}

	for _, obj := range resources {
		rt := obj.(*routeapi.Route)
		allRoutes = append(allRoutes, rt)
	}
	sort.Slice(allRoutes, func(i, j int) bool {
		if allRoutes[i].Spec.Host == allRoutes[j].Spec.Host {
			if (len(allRoutes[i].Spec.Path) == 0 || len(allRoutes[j].Spec.Path) == 0) && (allRoutes[i].Spec.Path == "/" || allRoutes[j].Spec.Path == "/") {
				return allRoutes[i].CreationTimestamp.Before(&allRoutes[j].CreationTimestamp)
			}
		}
		return (allRoutes[i].Spec.Host < allRoutes[j].Spec.Host) ||
			(allRoutes[i].Spec.Host == allRoutes[j].Spec.Host &&
				allRoutes[i].Spec.Path == allRoutes[j].Spec.Path &&
				allRoutes[i].CreationTimestamp.Before(&allRoutes[j].CreationTimestamp)) ||
			(allRoutes[i].Spec.Host == allRoutes[j].Spec.Host &&
				allRoutes[i].Spec.Path < allRoutes[j].Spec.Path)
	})

	return allRoutes
}

func doRoutesHandleHTTP(routes []*routeapi.Route) bool {
	for _, route := range routes {
		if !isSecureRoute(route) {
			// If it is not TLS VirtualServer(HTTPS), then it is HTTP server
			return true
		}

		// If Allow or Redirect happens then HTTP Traffic is being handled.
		if route.Spec.TLS.InsecureEdgeTerminationPolicy == routeapi.InsecureEdgeTerminationPolicyAllow ||
			route.Spec.TLS.InsecureEdgeTerminationPolicy == routeapi.InsecureEdgeTerminationPolicyRedirect {
			return true
		}
	}

	return false
}

func isSecureRoute(route *routeapi.Route) bool {
	return route.Spec.TLS != nil
}

func getAffectedRouteGroups(svc *v1.Service) []string {
	return []string{svc.Namespace}
}

func getBasicVirtualPorts() []portStruct {
	return []portStruct{
		{
			protocol: "http",
			port:     DEFAULT_HTTP_PORT,
		},
		{
			protocol: "https",
			port:     DEFAULT_HTTPS_PORT,
		},
	}
}

func getVirtualPortsForRoutes(routes []*routeapi.Route) []portStruct {
	ports := []portStruct{
		{
			protocol: "http",
			port:     DEFAULT_HTTP_PORT,
		},
	}

	for _, rt := range routes {
		if isSecureRoute(rt) {
			return getBasicVirtualPorts()
		}
	}
	return ports
}

func frameRouteVSName(routeGroup string,
	extdSpec *ExtendedRouteGroupSpec,
	portStruct portStruct,
) string {
	var rsName string
	if extdSpec.VServerName != "" {
		rsName = formatCustomVirtualServerName(
			extdSpec.VServerName,
			portStruct.port,
		)
	} else {
		rsName = formatCustomVirtualServerName(
			"routes_"+routeGroup,
			portStruct.port,
		)
	}
	return rsName
}

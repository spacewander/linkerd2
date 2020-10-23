package destination

import (
	"context"
	"fmt"

	pb "github.com/linkerd/linkerd2-proxy-api/go/destination"
	"github.com/linkerd/linkerd2-proxy-api/go/net"
	"github.com/linkerd/linkerd2/controller/api/destination/watcher"
	"github.com/linkerd/linkerd2/pkg/addr"
	"github.com/linkerd/linkerd2/pkg/k8s"
	logging "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const defaultWeight uint32 = 10000

// endpointTranslator satisfies EndpointUpdateListener and translates updates
// into Destination.Get messages.
type endpointTranslator struct {
	controllerNS        string
	identityTrustDomain string
	enableH2Upgrade     bool
	nodeTopologyLabels  map[string]string

	availableEndpoints watcher.AddressSet
	filteredSnapshot   watcher.AddressSet
	stream             pb.Destination_GetServer
	log                *logging.Entry
}

func newEndpointTranslator(
	ctx context.Context,
	controllerNS string,
	identityTrustDomain string,
	enableH2Upgrade bool,
	service string,
	srcNodeName string,
	k8sClient kubernetes.Interface,
	stream pb.Destination_GetServer,
	log *logging.Entry,
) *endpointTranslator {
	log = log.WithFields(logging.Fields{
		"component": "endpoint-translator",
		"service":   service,
	})

	nodeTopologyLabels, err := getK8sNodeTopology(ctx, k8sClient, srcNodeName)
	if err != nil {
		log.Errorf("Failed to get node topology for node %s: %s", srcNodeName, err)
	}
	availableEndpoints := newEmptyAddressSet()

	filteredSnapshot := newEmptyAddressSet()

	return &endpointTranslator{
		controllerNS,
		identityTrustDomain,
		enableH2Upgrade,
		nodeTopologyLabels,
		availableEndpoints,
		filteredSnapshot,
		stream,
		log,
	}
}

func (et *endpointTranslator) Add(set watcher.AddressSet) {
	for id, address := range set.Addresses {
		et.availableEndpoints.Addresses[id] = address
	}

	et.sendFilteredUpdate(set)
}

func (et *endpointTranslator) Remove(set watcher.AddressSet) {
	for id := range set.Addresses {
		delete(et.availableEndpoints.Addresses, id)
	}

	et.sendFilteredUpdate(set)
}

func (et *endpointTranslator) sendFilteredUpdate(set watcher.AddressSet) {
	et.availableEndpoints = watcher.AddressSet{
		Addresses:       et.availableEndpoints.Addresses,
		Labels:          set.Labels,
		TopologicalPref: set.TopologicalPref,
	}

	filtered := et.filterAddresses()
	diffAdd, diffRemove := et.diffEndpoints(filtered)

	if len(diffAdd.Addresses) > 0 {
		et.sendClientAdd(diffAdd)
	}
	if len(diffRemove.Addresses) > 0 {
		et.sendClientRemove(diffRemove)
	}

	et.filteredSnapshot = filtered
}

// filterAddresses is responsible for filtering endpoints based on service topology preference.
// The client will receive only endpoints with the same topology label value as the source node,
// the order of labels is based on the topological preference elicited from the K8s service.
func (et *endpointTranslator) filterAddresses() watcher.AddressSet {
	if len(et.availableEndpoints.TopologicalPref) == 0 {
		allAvailEndpoints := make(map[watcher.ID]watcher.Address)
		for k, v := range et.availableEndpoints.Addresses {
			allAvailEndpoints[k] = v
		}
		return watcher.AddressSet{
			Addresses: allAvailEndpoints,
			Labels:    et.availableEndpoints.Labels,
		}
	}

	et.log.Debugf("Filtering through address set with preference %v", et.availableEndpoints.TopologicalPref)
	filtered := make(map[watcher.ID]watcher.Address)
	for _, pref := range et.availableEndpoints.TopologicalPref {
		// '*' as a topology preference means all endpoints
		if pref == "*" {
			return et.availableEndpoints
		}

		srcLocality, ok := et.nodeTopologyLabels[pref]
		if !ok {
			continue
		}

		for id, address := range et.availableEndpoints.Addresses {
			addrLocality := address.TopologyLabels[pref]
			if addrLocality == srcLocality {
				filtered[id] = address
			}
		}

		// if we filtered at least one endpoint, it means that preference has been satisfied
		if len(filtered) > 0 {
			et.log.Debugf("Filtered %d from a total of %d", len(filtered), len(et.availableEndpoints.Addresses))
			return watcher.AddressSet{
				Addresses: filtered,
				Labels:    et.availableEndpoints.Labels,
			}
		}
	}

	// if we have no filtered endpoints or the '*' preference then no topology pref is satisfied
	return newEmptyAddressSet()
}

// diffEndpoints calculates the difference between the filtered set of endpoints in the current (Add/Remove) operation
// and the snapshot of previously filtered endpoints. This diff allows the client to receive only the endpoints that
// satisfy the topological preference, by adding new endpoints and removing stale ones.
func (et *endpointTranslator) diffEndpoints(filtered watcher.AddressSet) (watcher.AddressSet, watcher.AddressSet) {
	add := make(map[watcher.ID]watcher.Address)
	remove := make(map[watcher.ID]watcher.Address)

	for id, address := range filtered.Addresses {
		if _, ok := et.filteredSnapshot.Addresses[id]; !ok {
			add[id] = address
		}
	}

	for id, address := range et.filteredSnapshot.Addresses {
		if _, ok := filtered.Addresses[id]; !ok {
			remove[id] = address
		}
	}

	return watcher.AddressSet{
			Addresses: add,
			Labels:    filtered.Labels,
		},
		watcher.AddressSet{
			Addresses: remove,
			Labels:    filtered.Labels,
		}
}

func (et *endpointTranslator) NoEndpoints(exists bool) {
	et.log.Debugf("NoEndpoints(%+v)", exists)

	et.availableEndpoints.Addresses = map[watcher.ID]watcher.Address{}
	et.filteredSnapshot.Addresses = map[watcher.ID]watcher.Address{}

	u := &pb.Update{
		Update: &pb.Update_NoEndpoints{
			NoEndpoints: &pb.NoEndpoints{
				Exists: exists,
			},
		},
	}

	et.log.Debugf("Sending destination no endpoints: %+v", u)
	if err := et.stream.Send(u); err != nil {
		et.log.Errorf("Failed to send address update: %s", err)
	}
}

func (et *endpointTranslator) sendClientAdd(set watcher.AddressSet) {
	addrs := []*pb.WeightedAddr{}
	for _, address := range set.Addresses {
		var (
			wa  *pb.WeightedAddr
			err error
		)
		if address.Pod != nil {
			wa, err = et.toWeightedAddr(address)
		} else {
			var authOverride *pb.AuthorityOverride
			if address.AuthorityOverride != "" {
				authOverride = &pb.AuthorityOverride{
					AuthorityOverride: address.AuthorityOverride,
				}
			}

			// handling address with no associated pod
			var addr *net.TcpAddress
			addr, err = et.toAddr(address)
			wa = &pb.WeightedAddr{
				Addr:              addr,
				Weight:            defaultWeight,
				AuthorityOverride: authOverride,
			}

			if address.Identity != "" {
				wa.TlsIdentity = &pb.TlsIdentity{
					Strategy: &pb.TlsIdentity_DnsLikeIdentity_{
						DnsLikeIdentity: &pb.TlsIdentity_DnsLikeIdentity{
							Name: address.Identity,
						},
					},
				}
				// in this case we most likely have a proxy on the other side, so set protocol hint as well.
				if et.enableH2Upgrade {
					wa.ProtocolHint = &pb.ProtocolHint{
						Protocol: &pb.ProtocolHint_H2_{
							H2: &pb.ProtocolHint_H2{},
						},
					}
				}
			}
		}
		if err != nil {
			et.log.Errorf("Failed to translate endpoints to weighted addr: %s", err)
			continue
		}
		addrs = append(addrs, wa)
	}

	add := &pb.Update{Update: &pb.Update_Add{
		Add: &pb.WeightedAddrSet{
			Addrs:        addrs,
			MetricLabels: set.Labels,
		},
	}}

	et.log.Debugf("Sending destination add: %+v", add)
	if err := et.stream.Send(add); err != nil {
		et.log.Errorf("Failed to send address update: %s", err)
	}
}

func (et *endpointTranslator) sendClientRemove(set watcher.AddressSet) {
	addrs := []*net.TcpAddress{}
	for _, address := range set.Addresses {
		tcpAddr, err := et.toAddr(address)
		if err != nil {
			et.log.Errorf("Failed to translate endpoints to addr: %s", err)
			continue
		}
		addrs = append(addrs, tcpAddr)
	}

	remove := &pb.Update{Update: &pb.Update_Remove{
		Remove: &pb.AddrSet{
			Addrs: addrs,
		},
	}}

	et.log.Debugf("Sending destination remove: %+v", remove)
	if err := et.stream.Send(remove); err != nil {
		et.log.Errorf("Failed to send address update: %s", err)
	}
}

func (et *endpointTranslator) toAddr(address watcher.Address) (*net.TcpAddress, error) {
	ip, err := addr.ParseProxyIPV4(address.IP)
	if err != nil {
		return nil, err
	}
	return &net.TcpAddress{
		Ip:   ip,
		Port: address.Port,
	}, nil
}

func (et *endpointTranslator) toWeightedAddr(address watcher.Address) (*pb.WeightedAddr, error) {
	controllerNS := address.Pod.Labels[k8s.ControllerNSLabel]
	sa, ns := k8s.GetServiceAccountAndNS(address.Pod)
	labels := k8s.GetPodLabels(address.OwnerKind, address.OwnerName, address.Pod)

	// If the pod is controlled by any Linkerd control plane, then it can be hinted
	// that this destination knows H2 (and handles our orig-proto translation).
	var hint *pb.ProtocolHint
	if et.enableH2Upgrade && controllerNS != "" {
		hint = &pb.ProtocolHint{
			Protocol: &pb.ProtocolHint_H2_{
				H2: &pb.ProtocolHint_H2{},
			},
		}
	}

	// If the pod is controlled by the same Linkerd control plane, then it can
	// participate in identity with peers.
	//
	// TODO this should be relaxed to match a trust domain annotation so that
	// multiple meshes can participate in identity if they share trust roots.
	var identity *pb.TlsIdentity
	if et.identityTrustDomain != "" &&
		controllerNS == et.controllerNS &&
		address.Pod.Annotations[k8s.IdentityModeAnnotation] == k8s.IdentityModeDefault {

		id := fmt.Sprintf("%s.%s.serviceaccount.identity.%s.%s", sa, ns, controllerNS, et.identityTrustDomain)
		identity = &pb.TlsIdentity{
			Strategy: &pb.TlsIdentity_DnsLikeIdentity_{
				DnsLikeIdentity: &pb.TlsIdentity_DnsLikeIdentity{
					Name: id,
				},
			},
		}
	}

	tcpAddr, err := et.toAddr(address)
	if err != nil {
		return nil, err
	}

	return &pb.WeightedAddr{
		Addr:         tcpAddr,
		Weight:       defaultWeight,
		MetricLabels: labels,
		TlsIdentity:  identity,
		ProtocolHint: hint,
	}, nil
}

func getK8sNodeTopology(ctx context.Context, k8sClient kubernetes.Interface, srcNode string) (map[string]string, error) {
	nodeTopology := make(map[string]string)
	node, err := k8sClient.CoreV1().Nodes().Get(ctx, srcNode, metav1.GetOptions{})
	if err != nil {
		return nodeTopology, err
	}

	for k, v := range node.Labels {
		if k == corev1.LabelHostname ||
			k == corev1.LabelZoneFailureDomainStable ||
			k == corev1.LabelZoneRegionStable {
			nodeTopology[k] = v
		}
	}

	return nodeTopology, nil
}

func newEmptyAddressSet() watcher.AddressSet {
	return watcher.AddressSet{
		Addresses:       make(map[watcher.ID]watcher.Address),
		Labels:          make(map[string]string),
		TopologicalPref: []string{},
	}
}

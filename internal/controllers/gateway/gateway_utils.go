package gateway

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	"github.com/kong/kubernetes-ingress-controller/v2/internal/annotations"
)

// -----------------------------------------------------------------------------
// Gateway Utilities
// -----------------------------------------------------------------------------

const (
	// maxConds is the maximum number of status conditions a Gateway can have at one time.
	maxConds = 8
)

// isGatewayScheduled returns boolean whether or not the gateway object was scheduled
// previously by the gateway controller.
func isGatewayScheduled(gateway *gatewayv1alpha2.Gateway) bool {
	for _, cond := range gateway.Status.Conditions {
		if cond.Type == string(gatewayv1alpha2.GatewayConditionScheduled) &&
			cond.Reason == string(gatewayv1alpha2.GatewayReasonScheduled) &&
			cond.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// isGatewayReady returns boolean whether the ready condition exists
// for the given gateway object if it matches the currently known generation of that object.
func isGatewayReady(gateway *gatewayv1alpha2.Gateway) bool {
	for _, cond := range gateway.Status.Conditions {
		if cond.Type == string(gatewayv1alpha2.GatewayConditionReady) && cond.Reason == string(gatewayv1alpha2.GatewayReasonReady) && cond.ObservedGeneration == gateway.Generation {
			return true
		}
	}
	return false
}

// isGatewayInClassAndUnmanaged returns boolean if the provided combination of gateway and class
// is controlled by this controller and the gateway is configured for unmanaged mode.
func isGatewayInClassAndUnmanaged(gatewayClass *gatewayv1alpha2.GatewayClass, gateway gatewayv1alpha2.Gateway) bool {
	_, ok := annotations.ExtractUnmanagedGatewayMode(gateway.Annotations)
	return ok && gatewayClass.Spec.ControllerName == ControllerName
}

// getRefFromPublishService splits a publish service string in the format namespace/name into a types.NamespacedName
// and verifies the contents producing an error if they don't match namespace/name format.
func getRefFromPublishService(publishService string) (types.NamespacedName, error) {
	publishServiceSplit := strings.SplitN(publishService, "/", 3)
	if len(publishServiceSplit) != 2 {
		return types.NamespacedName{}, fmt.Errorf("--publish-service expected in format 'namespace/name' but got %s", publishService)
	}
	return types.NamespacedName{
		Namespace: publishServiceSplit[0],
		Name:      publishServiceSplit[1],
	}, nil
}

// pruneGatewayStatusConds cleans out old status conditions if the Gateway currently has more
// status conditions set than the 8 maximum allowed by the Kubernetes API.
func pruneGatewayStatusConds(gateway *gatewayv1alpha2.Gateway) *gatewayv1alpha2.Gateway {
	if len(gateway.Status.Conditions) > maxConds {
		gateway.Status.Conditions = gateway.Status.Conditions[len(gateway.Status.Conditions)-maxConds:]
	}
	return gateway
}

// reconcileGatewaysIfClassMatches is a filter function to convert a list of gateways into a list
// of reconciliation requests for those gateways based on which match the given class.
func reconcileGatewaysIfClassMatches(gatewayClass client.Object, gateways []gatewayv1alpha2.Gateway) (recs []reconcile.Request) {
	for _, gateway := range gateways {
		if string(gateway.Spec.GatewayClassName) == gatewayClass.GetName() {
			recs = append(recs, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: gateway.Namespace,
					Name:      gateway.Name,
				},
			})
		}
	}
	return
}

func getListenerStatus(
	gateway *gatewayv1alpha2.Gateway,
	kongListens []gatewayv1alpha2.Listener,
) []gatewayv1alpha2.ListenerStatus {
	statuses := []gatewayv1alpha2.ListenerStatus{}
	protocols := map[gatewayv1alpha2.ProtocolType]map[gatewayv1alpha2.PortNumber]bool{}

	existingListenerStatuses := make(map[gatewayv1alpha2.SectionName]gatewayv1alpha2.ListenerStatus, len(gateway.Status.Listeners))
	for _, listenerStatus := range gateway.Status.Listeners {
		existingListenerStatuses[listenerStatus.Name] = listenerStatus
	}

	for _, listen := range kongListens {
		_, ok := protocols[listen.Protocol]
		if !ok {
			protocols[listen.Protocol] = map[gatewayv1alpha2.PortNumber]bool{}
		}
		protocols[listen.Protocol][listen.Port] = true
	}

	portsToProtocol := make(map[gatewayv1alpha2.PortNumber]gatewayv1alpha2.ProtocolType, len(gateway.Spec.Listeners))
	portsToHostnames := make(map[gatewayv1alpha2.PortNumber]map[gatewayv1alpha2.Hostname]gatewayv1alpha2.SectionName,
		len(gateway.Spec.Listeners))

	// we need to run through listeners with existing no conflict statuses first
	// they take precedence in the event of a conflict later. we do not perform conflict checks here, only that
	// the current status is not conflicted. we assume the next section will have inserted the correct status
	for _, listener := range gateway.Spec.Listeners {
		if existingListenerStatus, ok := existingListenerStatuses[listener.Name]; ok {
			for _, condition := range existingListenerStatus.Conditions {
				if condition.Type == string(gatewayv1alpha2.ListenerConditionConflicted) &&
					condition.Status == metav1.ConditionFalse {
					if _, ok := portsToProtocol[listener.Port]; !ok {
						portsToProtocol[listener.Port] = listener.Protocol
					}
					if listener.Protocol == gatewayv1alpha2.HTTPProtocolType ||
						listener.Protocol == gatewayv1alpha2.HTTPSProtocolType ||
						listener.Protocol == gatewayv1alpha2.TLSProtocolType {
						if _, ok := portsToHostnames[listener.Port]; !ok {
							portsToHostnames[listener.Port] = make(map[gatewayv1alpha2.Hostname]gatewayv1alpha2.SectionName)
						}
						var hostname gatewayv1alpha2.Hostname
						if listener.Hostname == nil {
							hostname = gatewayv1alpha2.Hostname("")
						} else {
							hostname = *listener.Hostname
						}
						portsToHostnames[listener.Port][hostname] = listener.Name
					}
				}
			}
		}
	}

	for _, listener := range gateway.Spec.Listeners {
		var attachedRoutes int32
		if existingListenerStatus, ok := existingListenerStatuses[listener.Name]; ok {
			attachedRoutes = existingListenerStatus.AttachedRoutes
		}
		status := gatewayv1alpha2.ListenerStatus{
			Name:           listener.Name,
			Conditions:     []metav1.Condition{},
			SupportedKinds: supportedRouteGroupKinds,
			AttachedRoutes: attachedRoutes,
		}
		// TODO this only handles some Listener conditions and reasons as needed to check cross-listener compatibility
		// and unattachability due to missing Kong configuration. There are others available and it may be appropriate
		// for us to add them https://github.com/Kong/kubernetes-ingress-controller/issues/2558
		if _, ok := portsToProtocol[listener.Port]; !ok {
			portsToProtocol[listener.Port] = listener.Protocol
		} else {
			// Either each Listener within the group specifies the “HTTP” Protocol or each Listener within the group
			// specifies either the “HTTPS” or “TLS” Protocol.
			// TCP and UDP listeners must always use unique ports
			if portsToProtocol[listener.Port] == gatewayv1alpha2.TCPProtocolType ||
				portsToProtocol[listener.Port] == gatewayv1alpha2.UDPProtocolType {
				status.Conditions = append(status.Conditions, metav1.Condition{
					Type:               string(gatewayv1alpha2.ListenerConditionConflicted),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: gateway.Generation,
					// TODO we should check transition time rather than always nowing
					// https://github.com/Kong/kubernetes-ingress-controller/issues/2556
					LastTransitionTime: metav1.Now(),
					// TODO confirm upstream. this sounds a bit odd since it's maybe same protocol, they just can't
					// share a port. this sounds more correct than HostnameConflict though. there are no conformance
					// tests yet
					Reason: string(gatewayv1alpha2.ListenerReasonProtocolConflict),
				})
			} else if portsToProtocol[listener.Port] == listener.Protocol ||
				listener.Protocol == gatewayv1alpha2.HTTPSProtocolType && portsToProtocol[listener.Port] == gatewayv1alpha2.TLSProtocolType ||
				listener.Protocol == gatewayv1alpha2.TLSProtocolType && portsToProtocol[listener.Port] == gatewayv1alpha2.HTTPSProtocolType {
				if _, ok := portsToHostnames[listener.Port]; !ok {
					portsToHostnames[listener.Port] = make(map[gatewayv1alpha2.Hostname]gatewayv1alpha2.SectionName)
				}
				// Each Listener within the group specifies a Hostname that is unique within the group.
				// As a special case, one Listener within a group may omit Hostname, in which case this Listener
				// matches when no other Listener matches.
				var hostname gatewayv1alpha2.Hostname
				if listener.Hostname == nil {
					hostname = gatewayv1alpha2.Hostname("")
				} else {
					hostname = *listener.Hostname
				}
				if _, exists := portsToHostnames[listener.Port][hostname]; !exists {
					portsToHostnames[listener.Port][hostname] = listener.Name
				} else {
					// ignore if we already added ourselves when handling existing
					if !(portsToHostnames[listener.Port][hostname] == listener.Name) {
						status.Conditions = append(status.Conditions, metav1.Condition{
							Type:               string(gatewayv1alpha2.ListenerConditionConflicted),
							Status:             metav1.ConditionTrue,
							ObservedGeneration: gateway.Generation,
							LastTransitionTime: metav1.Now(),
							Reason:             string(gatewayv1alpha2.ListenerReasonHostnameConflict),
						})
					}
				}
			} else {
				status.Conditions = append(status.Conditions, metav1.Condition{
					Type:               string(gatewayv1alpha2.ListenerConditionConflicted),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: gateway.Generation,
					LastTransitionTime: metav1.Now(),
					Reason:             string(gatewayv1alpha2.ListenerReasonProtocolConflict),
				})
			}
		}

		if len(protocols[listener.Protocol]) == 0 {
			status.Conditions = append(status.Conditions, metav1.Condition{
				Type:               string(gatewayv1alpha2.ListenerConditionDetached),
				Status:             metav1.ConditionTrue,
				ObservedGeneration: gateway.Generation,
				LastTransitionTime: metav1.Now(),
				Reason:             string(gatewayv1alpha2.ListenerReasonUnsupportedProtocol),
				Message:            "no Kong listen with the requested protocol is configured",
			})
		}
		if _, ok := protocols[listener.Protocol][listener.Port]; !ok {
			status.Conditions = append(status.Conditions, metav1.Condition{
				Type:               string(gatewayv1alpha2.ListenerConditionDetached),
				Status:             metav1.ConditionTrue,
				ObservedGeneration: gateway.Generation,
				LastTransitionTime: metav1.Now(),
				Reason:             string(gatewayv1alpha2.ListenerReasonPortUnavailable),
				Message:            "no Kong listen with the requested protocol is configured for the requested port",
			})
		}
		// if we've gotten this far with no conditions, the listener is good to go
		if len(status.Conditions) == 0 {
			status.Conditions = append(status.Conditions,
				metav1.Condition{
					Type:               string(gatewayv1alpha2.ListenerConditionConflicted),
					Status:             metav1.ConditionFalse,
					ObservedGeneration: gateway.Generation,
					LastTransitionTime: metav1.Now(),
					Reason:             string(gatewayv1alpha2.ListenerReasonNoConflicts),
				},
				metav1.Condition{
					Type:               string(gatewayv1alpha2.ListenerConditionReady),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: gateway.Generation,
					LastTransitionTime: metav1.Now(),
					Reason:             string(gatewayv1alpha2.ListenerReasonReady),
					Message:            "the listener is ready and available for routing",
				},
			)
		} else {
			// unsure if we want to add the ready=false condition on a per-failure basis or use this else to just mark
			// it generic unready if we hit anything bad. do any failure conditions block readiness? do we care about
			// having distinct ready false messages, assuming we have more descriptive messages in the other conditions?
			status.Conditions = append(status.Conditions, metav1.Condition{
				Type:               string(gatewayv1alpha2.ListenerConditionReady),
				Status:             metav1.ConditionFalse,
				ObservedGeneration: gateway.Generation,
				LastTransitionTime: metav1.Now(),
				Reason:             string(gatewayv1alpha2.ListenerReasonPending),
				Message:            "the listener is not ready and cannot route requests",
			})
		}
		statuses = append(statuses, status)
	}
	return statuses
}

// -----------------------------------------------------------------------------
// Gateway Utils - Watch Predicate Helpers
// -----------------------------------------------------------------------------

// isGatewayClassEventInClass produces a boolean whether or not a given event which contains
// one or more GatewayClass objects is supported by this controller according to those
// objects ControllerName.
func isGatewayClassEventInClass(log logr.Logger, watchEvent interface{}) bool {
	objs := make([]client.Object, 0, 2)
	switch e := watchEvent.(type) {
	case event.CreateEvent:
		objs = append(objs, e.Object)
	case event.DeleteEvent:
		objs = append(objs, e.Object)
	case event.GenericEvent:
		objs = append(objs, e.Object)
	case event.UpdateEvent:
		objs = append(objs, e.ObjectOld)
		objs = append(objs, e.ObjectNew)
	default:
		log.Error(fmt.Errorf("invalid type"), "received invalid event type in event handlers", "found", reflect.TypeOf(watchEvent))
		return false
	}

	for _, obj := range objs {
		gwc, ok := obj.(*gatewayv1alpha2.GatewayClass)
		if !ok {
			log.Error(fmt.Errorf("invalid type"), "received invalid object type in event handlers", "expected", "GatewayClass", "found", reflect.TypeOf(obj))
			continue
		}
		if gwc.Spec.ControllerName == ControllerName {
			return true
		}
	}

	return false
}

/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cloud

import (
	"fmt"
	"net"
	"time"

	"github.com/golang/glog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	clientv1 "k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/v1"
	"k8s.io/kubernetes/pkg/client/clientset_generated/clientset"
	coreinformers "k8s.io/kubernetes/pkg/client/informers/informers_generated/externalversions/core/v1"
	clientretry "k8s.io/kubernetes/pkg/client/retry"
	"k8s.io/kubernetes/pkg/cloudprovider"
	nodeutil "k8s.io/kubernetes/pkg/util/node"
)

var UpdateNodeSpecBackoff = wait.Backoff{
	Steps:    20,
	Duration: 50 * time.Millisecond,
	Jitter:   1.0,
}

type CloudNodeController struct {
	nodeInformer coreinformers.NodeInformer
	kubeClient   clientset.Interface
	recorder     record.EventRecorder

	cloud cloudprovider.Interface

	// Value controlling NodeController monitoring period, i.e. how often does NodeController
	// check node status posted from kubelet. This value should be lower than nodeMonitorGracePeriod
	// set in controller-manager
	nodeMonitorPeriod time.Duration
}

const (
	// nodeStatusUpdateRetry controls the number of retries of writing NodeStatus update.
	nodeStatusUpdateRetry = 5

	// The amount of time the nodecontroller should sleep between retrying NodeStatus updates
	retrySleepTime = 20 * time.Millisecond

	//Taint denoting that a node needs to be processed by external cloudprovider
	CloudTaintKey = "ExternalCloudProvider"

	nodeStatusUpdateFrequency = 10 * time.Second

	LabelProvidedIPAddr = "beta.kubernetes.io/provided-node-ip"
)

// NewCloudNodeController creates a CloudNodeController object
func NewCloudNodeController(
	nodeInformer coreinformers.NodeInformer,
	kubeClient clientset.Interface,
	cloud cloudprovider.Interface,
	nodeMonitorPeriod time.Duration) *CloudNodeController {

	eventBroadcaster := record.NewBroadcaster()
	recorder := eventBroadcaster.NewRecorder(api.Scheme, clientv1.EventSource{Component: "cloudcontrollermanager"})
	eventBroadcaster.StartLogging(glog.Infof)
	if kubeClient != nil {
		glog.V(0).Infof("Sending events to api server.")
		eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: v1core.New(kubeClient.Core().RESTClient()).Events("")})
	} else {
		glog.V(0).Infof("No api server defined - no events will be sent to API server.")
	}

	cnc := &CloudNodeController{
		nodeInformer:      nodeInformer,
		kubeClient:        kubeClient,
		recorder:          recorder,
		cloud:             cloud,
		nodeMonitorPeriod: nodeMonitorPeriod,
	}

	nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: cnc.AddCloudNode,
	})

	return cnc
}

// This controller deletes a node if kubelet is not reporting
// and the node is gone from the cloud provider.
func (cnc *CloudNodeController) Run() {
	go func() {
		defer utilruntime.HandleCrash()

		instances, ok := cnc.cloud.Instances()
		if !ok {
			utilruntime.HandleError(fmt.Errorf("failed to get instances from cloud provider"))
			return
		}

		// Start a loop to periodically update the node addresses obtained from the cloud
		go wait.Until(func() {
			nodes, err := cnc.kubeClient.Core().Nodes().List(metav1.ListOptions{ResourceVersion: "0"})
			if err != nil {
				glog.Errorf("Error monitoring node status: %v", err)
				return
			}

			for i := range nodes.Items {
				node := &nodes.Items[i]
				nodeAddresses, err := instances.NodeAddressesByProviderID(node.Spec.ProviderID)
				if err != nil {
					nodeAddresses, err = instances.NodeAddresses(types.NodeName(node.Name))
					if err != nil {
						glog.Errorf("failed to get node address from cloud provider: %v", err)
						continue
					}
				}
				// Do not process nodes that are still tainted
				taints, err := v1.GetTaintsFromNodeAnnotations(node.Annotations)
				if err != nil {
					glog.Errorf("could not get taints from node %s", node.Name)
					continue
				}

				var cloudTaint *v1.Taint
				for _, taint := range taints {
					if taint.Key == CloudTaintKey {
						cloudTaint = &taint
					}
				}

				if cloudTaint != nil {
					glog.V(5).Infof("This node %s is still tainted. Will not process.", node.Name)
					continue
				}
				var nodeIP net.IP
				if ip, ok := node.ObjectMeta.Labels[LabelProvidedIPAddr]; ok {
					nodeIP = net.ParseIP(ip)
				}
				// Check if a hostname address exists in the cloud provided addresses
				hostnameExists := false
				for i := range nodeAddresses {
					if nodeAddresses[i].Type == v1.NodeHostName {
						hostnameExists = true
					}
				}
				// If hostname was not present in cloud provided addresses, use the hostname
				// from the existing node (populated by kubelet)
				var hostnameAddress *v1.NodeAddress
				if !hostnameExists {
					for _, addr := range node.Status.Addresses {
						if addr.Type == v1.NodeHostName {
							hostnameAddress = &addr
						}
					}
				}
				// If nodeIP was suggested by user, ensure that
				// it can be found in the cloud as well (consistent with the behaviour in kubelet)
				if nodeIP != nil {
					var providedIP *v1.NodeAddress
					for i := range nodeAddresses {
						if nodeAddresses[i].Address == nodeIP.String() {
							providedIP = &nodeAddresses[i]
						}
					}
					if providedIP == nil {
						glog.Errorf("failed to get node address from cloudprovider that matches ip: %v", nodeIP)
						continue
					}
					nodeAddresses = []v1.NodeAddress{
						{Type: providedIP.Type, Address: providedIP.Address},
					}
				}
				if hostnameAddress != nil {
					nodeAddresses = append(nodeAddresses, *hostnameAddress)
				}
				nodeCopy, err := api.Scheme.DeepCopy(node)
				if err != nil {
					glog.Errorf("failed to copy node to a new object")
					continue
				}
				newNode := nodeCopy.(*v1.Node)
				newNode.Status.Addresses = nodeAddresses
				_, err = nodeutil.PatchNodeStatus(cnc.kubeClient, types.NodeName(node.Name), node, newNode)
				if err != nil {
					glog.Errorf("Error patching node with cloud ip addresses = [%v]", err)
				}
			}
		}, nodeStatusUpdateFrequency, wait.NeverStop)

		go wait.Until(func() {
			nodes, err := cnc.kubeClient.Core().Nodes().List(metav1.ListOptions{ResourceVersion: "0"})
			if err != nil {
				glog.Errorf("Error monitoring node status: %v", err)
				return
			}

			for i := range nodes.Items {
				var currentReadyCondition *v1.NodeCondition
				node := &nodes.Items[i]
				// Try to get the current node status
				// If node status is empty, then kubelet has not posted ready status yet. In this case, process next node
				for rep := 0; rep < nodeStatusUpdateRetry; rep++ {
					_, currentReadyCondition = v1.GetNodeCondition(&node.Status, v1.NodeReady)
					if currentReadyCondition != nil {
						break
					}
					name := node.Name
					node, err = cnc.kubeClient.Core().Nodes().Get(name, metav1.GetOptions{})
					if err != nil {
						glog.Errorf("Failed while getting a Node to retry updating NodeStatus. Probably Node %s was deleted.", name)
						break
					}
					time.Sleep(retrySleepTime)
				}
				if currentReadyCondition == nil {
					glog.Errorf("Update status of Node %v from CloudNodeController exceeds retry count.", node.Name)
					continue
				}
				// If the known node status says that Node is NotReady, then check if the node has been removed
				// from the cloud provider. If node cannot be found in cloudprovider, then delete the node immediately
				if currentReadyCondition != nil {
					if currentReadyCondition.Status != v1.ConditionTrue {
						// Check with the cloud provider to see if the node still exists. If it
						// doesn't, delete the node immediately.
						if _, err := instances.ExternalID(types.NodeName(node.Name)); err != nil {
							if err == cloudprovider.InstanceNotFound {
								glog.V(2).Infof("Deleting node no longer present in cloud provider: %s", node.Name)
								ref := &v1.ObjectReference{
									Kind:      "Node",
									Name:      node.Name,
									UID:       types.UID(node.UID),
									Namespace: "",
								}
								glog.V(2).Infof("Recording %s event message for node %s", "DeletingNode", node.Name)
								cnc.recorder.Eventf(ref, v1.EventTypeNormal, fmt.Sprintf("Deleting Node %v because it's not present according to cloud provider", node.Name), "Node %s event: %s", node.Name, "DeletingNode")
								go func(nodeName string) {
									defer utilruntime.HandleCrash()
									if err := cnc.kubeClient.Core().Nodes().Delete(node.Name, nil); err != nil {
										glog.Errorf("unable to delete node %q: %v", node.Name, err)
									}
								}(node.Name)
							}
							glog.Errorf("Error getting node data from cloud: %v", err)
						}
					}
				}
			}
		}, cnc.nodeMonitorPeriod, wait.NeverStop)
	}()
}

func (cnc *CloudNodeController) AddCloudNode(obj interface{}) {
	node := obj.(*v1.Node)
	instances, ok := cnc.cloud.Instances()
	if !ok {
		utilruntime.HandleError(fmt.Errorf("cloudprovider does not support instances"))
		return
	}

	// This initializes nodes with cloud info
	// Only initializes nodes that were created with the "ExternalCloudProvider" taint
	taints, err := v1.GetTaintsFromNodeAnnotations(node.Annotations)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("could not get taints from node %s", node.Name))
		return
	}

	var cloudTaint *v1.Taint
	for _, taint := range taints {
		if taint.Key == CloudTaintKey {
			cloudTaint = &taint
		}
	}

	if cloudTaint == nil {
		glog.V(2).Infof("This node is registered without the cloud taint. Will not process.")
		return
	}

	err = clientretry.RetryOnConflict(UpdateNodeSpecBackoff, func() error {
		curNode, err := cnc.kubeClient.Core().Nodes().Get(node.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if curNode.Spec.ProviderID == "" {
			return fmt.Errorf("Node does not have providerID set. Cannot continue processing node.")
		}

		// If user provided an IP address, ensure that IP address is found
		// in the cloud provider before removing the taint on the node
		var nodeIP net.IP
		if ip, ok := node.ObjectMeta.Labels[LabelProvidedIPAddr]; ok {
			nodeIP = net.ParseIP(ip)
		}
		if nodeIP != nil {
			nodeAddresses, err := instances.NodeAddressesByProviderID(node.Spec.ProviderID)
			if err != nil {
				nodeAddresses, err = instances.NodeAddresses(types.NodeName(node.Name))
				if err != nil {
					glog.Errorf("failed to get node address from cloud provider: %v", err)
					return nil
				}
			}
			var providedIP *v1.NodeAddress
			for i := range nodeAddresses {
				if nodeAddresses[i].Address == nodeIP.String() {
					providedIP = &nodeAddresses[i]
				}
			}
			if providedIP == nil {
				glog.Errorf("failed to get node address for node %s from cloudprovider that matches ip: %v", node.Name, nodeIP)
				return nil
			}
		}

		instanceType, err := instances.InstanceTypeByProviderID(curNode.Spec.ProviderID)
		if err != nil {
			instanceType, err = instances.InstanceType(types.NodeName(curNode.Name))
			if err != nil {
				return err
			}
		}
		if instanceType != "" {
			glog.Infof("Adding node label from cloud provider: %s=%s", metav1.LabelInstanceType, instanceType)
			curNode.ObjectMeta.Labels[metav1.LabelInstanceType] = instanceType
		}

		// Since there are node taints, do we still need this?
		// This condition marks the node as unusable until routes are initialized in the cloud provider
		if cnc.cloud.ProviderName() == "gce" {
			curNode.Status.Conditions = append(node.Status.Conditions, v1.NodeCondition{
				Type:               v1.NodeNetworkUnavailable,
				Status:             v1.ConditionTrue,
				Reason:             "NoRouteCreated",
				Message:            "Node created without a route",
				LastTransitionTime: metav1.Now(),
			})
		}

		zones, ok := cnc.cloud.Zones()
		if ok {
			zone, err := zones.GetZone()
			if err != nil {
				return fmt.Errorf("failed to get zone from cloud provider: %v", err)
			}
			if zone.FailureDomain != "" {
				glog.Infof("Adding node label from cloud provider: %s=%s", metav1.LabelZoneFailureDomain, zone.FailureDomain)
				curNode.ObjectMeta.Labels[metav1.LabelZoneFailureDomain] = zone.FailureDomain
			}
			if zone.Region != "" {
				glog.Infof("Adding node label from cloud provider: %s=%s", metav1.LabelZoneRegion, zone.Region)
				curNode.ObjectMeta.Labels[metav1.LabelZoneRegion] = zone.Region
			}
		}

		nodeWithoutCloudTaint, _, err := v1.RemoveTaint(curNode, cloudTaint)
		if err != nil {
			return err
		}

		_, err = nodeutil.PatchNodeStatus(cnc.kubeClient, types.NodeName(curNode.Name), node, nodeWithoutCloudTaint)
		return err
	})
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
}

package e2etests

import (
	"context"
	"fmt"
	"strings"
	"time"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	sriovOperatorNS = "openshift-sriov-network-operator"
	sriovVendorID   = "8086"
	sriovInterface  = "net1"
	sriovVFCount    = "4"
)

type sriovHardware struct {
	nodeName string
	pfName   string
	deviceID string
	vendorID string
}

// discoverNetObservSriovHardware keeps this test scoped to the two cluster types
// supported by the original SR-IOV test and confirms that the expected PF is
// visible through the SR-IOV Network Operator.
func discoverNetObservSriovHardware() sriovHardware {
	ctx := context.Background()
	_, err := k8sClient.CoreV1().Namespaces().Get(ctx, sriovOperatorNS, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		g.Skip("SR-IOV Network Operator is not installed")
	}
	o.Expect(err).NotTo(o.HaveOccurred())

	consoleRoute, err := routeV1Client.RouteV1().Routes("openshift-console").Get(ctx, "console", metav1.GetOptions{})
	o.Expect(err).NotTo(o.HaveOccurred(), "failed to get the console route")

	var pfName, deviceID string
	switch {
	case strings.Contains(consoleRoute.Spec.Host, "sriov.openshift-qe.sdn.com"):
		pfName, deviceID = "ens2f0", "159b"
	case strings.Contains(consoleRoute.Spec.Host, "offload.openshift-qe.sdn.com"):
		pfName, deviceID = "ens2f1", "1583"
	default:
		g.Skip("SR-IOV flow test is supported only on the RDU1/RDU2 test clusters")
	}
	waitForSriovOperatorReady()

	nodeStates, err := k8sDynClient.Resource(gvrMap["sriovnetworknodestate"]).Namespace(sriovOperatorNS).List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		g.Skip("SR-IOV Network Operator has not created SriovNetworkNodeState resources")
	}
	o.Expect(err).NotTo(o.HaveOccurred())
	if len(nodeStates.Items) == 0 {
		g.Skip("SR-IOV Network Operator has not discovered any SR-IOV nodes")
	}

	for _, nodeState := range nodeStates.Items {
		interfaces, found, nestedErr := unstructured.NestedSlice(nodeState.Object, "status", "interfaces")
		o.Expect(nestedErr).NotTo(o.HaveOccurred())
		if !found {
			continue
		}
		for _, rawInterface := range interfaces {
			iface, ok := rawInterface.(map[string]interface{})
			if !ok {
				continue
			}
			name, _ := iface["name"].(string)
			device, _ := iface["deviceID"].(string)
			vendor, _ := iface["vendor"].(string)
			device = strings.TrimPrefix(strings.ToLower(device), "0x")
			vendor = strings.TrimPrefix(strings.ToLower(vendor), "0x")
			if name == pfName && device == deviceID && vendor == sriovVendorID {
				return sriovHardware{
					nodeName: nodeState.GetName(), pfName: pfName, deviceID: deviceID, vendorID: sriovVendorID,
				}
			}
		}
	}

	g.Skip(fmt.Sprintf("SR-IOV device %s (vendor %s) on %s was not found", deviceID, sriovVendorID, pfName))
	return sriovHardware{}
}

func waitForSriovOperatorReady() {
	ctx := context.Background()
	operatorPods, err := k8sClient.CoreV1().Pods(sriovOperatorNS).List(ctx, metav1.ListOptions{LabelSelector: "name=sriov-network-operator"})
	o.Expect(err).NotTo(o.HaveOccurred())
	if len(operatorPods.Items) == 0 {
		g.Skip("SR-IOV Network Operator is not running")
	}

	for _, selector := range []string{
		"app=network-resources-injector",
		"app=operator-webhook",
		"app=sriov-network-config-daemon",
		"name=sriov-network-operator",
	} {
		err := wait.PollUntilContextTimeout(ctx, 10*time.Second, 5*time.Minute, false, func(ctx context.Context) (bool, error) {
			pods, listErr := k8sClient.CoreV1().Pods(sriovOperatorNS).List(ctx, metav1.ListOptions{LabelSelector: selector})
			if listErr != nil {
				return false, listErr
			}
			if len(pods.Items) == 0 {
				return false, nil
			}
			for _, pod := range pods.Items {
				if pod.Status.Phase != corev1.PodRunning {
					return false, nil
				}
				ready := false
				for _, condition := range pod.Status.Conditions {
					if condition.Type == corev1.PodReady {
						ready = condition.Status == corev1.ConditionTrue
						break
					}
				}
				if !ready {
					return false, nil
				}
			}
			return true, nil
		})
		assertWaitPollNoErr(err, fmt.Sprintf("SR-IOV operator pods with selector %q are not ready", selector))
	}
}

func createSriovNetwork(templatePath, name, resourceName, networkNamespace, ip string) {
	err := applyNsResourceFromTemplateByAdmin(sriovOperatorNS,
		"--ignore-unknown-parameters=true", "-f", templatePath, "-p",
		"NAMESPACE="+sriovOperatorNS,
		"NETWORK_NAME="+name,
		"RESOURCE_NAME="+resourceName,
		"NETWORK_NAMESPACE="+networkNamespace,
		"IP_ADDRESS="+ip+"/24",
	)
	o.Expect(err).NotTo(o.HaveOccurred(), "failed to create SR-IOV network %s", name)
}

func waitForSriovNetworkAttachment(name, networkNamespace string) {
	g.By(fmt.Sprintf("Wait for network attachment %s in namespace %s", name, networkNamespace))
	err := wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 5*time.Minute, false, func(ctx context.Context) (bool, error) {
		_, getErr := k8sDynClient.Resource(gvrMap["net-attach-def"]).Namespace(networkNamespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(getErr) {
			return false, nil
		}
		return getErr == nil, getErr
	})
	assertWaitPollNoErr(err, fmt.Sprintf("network attachment %s was not created in %s", name, networkNamespace))
}

func createSriovTrafficPod(templatePath, name, namespace, nodeName, networkName, peerIP string) {
	err := applyNsResourceFromTemplateByAdmin(namespace,
		"--ignore-unknown-parameters=true", "-f", templatePath, "-p",
		"POD_NAME="+name,
		"NAMESPACE="+namespace,
		"NODE_NAME="+nodeName,
		"NETWORK_NAME="+networkName,
		"PEER_IP="+peerIP,
	)
	o.Expect(err).NotTo(o.HaveOccurred(), "failed to create SR-IOV traffic pod %s", name)
}

func waitForSriovTrafficPodReady(name, namespace string) {
	err := wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 15*time.Minute, false, func(ctx context.Context) (bool, error) {
		current, getErr := k8sClient.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return false, getErr
		}
		if current.Status.Phase != corev1.PodRunning {
			return false, nil
		}
		for _, condition := range current.Status.Conditions {
			if condition.Type == corev1.PodReady {
				return condition.Status == corev1.ConditionTrue, nil
			}
		}
		return false, nil
	})
	assertWaitPollNoErr(err, fmt.Sprintf("SR-IOV traffic pod %s did not become ready", name))
}

func waitForSriovPolicyAndResource(nodeName, resourceName string) {
	resourceKey := corev1.ResourceName("openshift.io/" + resourceName)
	err := wait.PollUntilContextTimeout(context.Background(), 10*time.Second, 30*time.Minute, false, func(ctx context.Context) (bool, error) {
		state, getErr := k8sDynClient.Resource(gvrMap["sriovnetworknodestate"]).Namespace(sriovOperatorNS).Get(ctx, nodeName, metav1.GetOptions{})
		if getErr != nil {
			return false, getErr
		}
		syncStatus, _, nestedErr := unstructured.NestedString(state.Object, "status", "syncStatus")
		if nestedErr != nil {
			return false, nestedErr
		}
		if syncStatus == "Failed" {
			return false, fmt.Errorf("SR-IOV node state %s reports syncStatus=Failed", nodeName)
		}
		if syncStatus != "Succeeded" {
			return false, nil
		}

		node, getErr := k8sClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if getErr != nil {
			return false, getErr
		}
		available, found := node.Status.Allocatable[resourceKey]
		return found && available.Value() >= 1, nil
	})
	assertWaitPollNoErr(err, fmt.Sprintf("SR-IOV resource openshift.io/%s did not become available on %s", resourceName, nodeName))
}

func waitForSriovNodePolicyResourceRemoved(nodeName, resourceName string) {
	resourceKey := corev1.ResourceName("openshift.io/" + resourceName)
	err := wait.PollUntilContextTimeout(context.Background(), 10*time.Second, 30*time.Minute, false, func(ctx context.Context) (bool, error) {
		state, getErr := k8sDynClient.Resource(gvrMap["sriovnetworknodestate"]).Namespace(sriovOperatorNS).Get(ctx, nodeName, metav1.GetOptions{})
		if getErr != nil {
			return false, getErr
		}
		syncStatus, _, nestedErr := unstructured.NestedString(state.Object, "status", "syncStatus")
		if nestedErr != nil {
			return false, nestedErr
		}
		if syncStatus != "Succeeded" {
			return false, nil
		}
		node, getErr := k8sClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if getErr != nil {
			return false, getErr
		}
		available, stillAdvertised := node.Status.Allocatable[resourceKey]
		return !stillAdvertised || available.Value() == 0, nil
	})
	assertWaitPollNoErr(err, fmt.Sprintf("SR-IOV resource openshift.io/%s was not removed from %s", resourceName, nodeName))
}

func getSriovFlowRecords(labels Lokilabels, lokiURL string, startTime time.Time, parameters ...string) []FlowRecord {
	// GetMonolithicLokiFlowLogs retries until it finds matching records or times
	// out, so an outer polling loop would multiply the timeout unnecessarily.
	records, err := labels.GetMonolithicLokiFlowLogsRegex(lokiURL, startTime, parameters...)
	o.Expect(err).NotTo(o.HaveOccurred(), "failed to query SR-IOV flows from Loki")
	return records
}

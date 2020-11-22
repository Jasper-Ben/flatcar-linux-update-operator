// Package operator contains main implementation of Flatcar Linux Update Operator.
package operator

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	"github.com/coreos/locksmith/pkg/timeutil"
	"github.com/kinvolk/flatcar-linux-update-operator/pkg/constants"
	"github.com/kinvolk/flatcar-linux-update-operator/pkg/k8sutil"
)

const (
	eventSourceComponent               = "update-operator"
	leaderElectionEventSourceComponent = "update-operator-leader-election"
	// agentDefaultAppName is the label value for the 'app' key that agents are
	// expected to be labeled with.
	agentDefaultAppName = "flatcar-linux-update-agent"
	maxRebootingNodes   = 1

	leaderElectionResourceName = "flatcar-linux-update-operator-lock"

	// Arbitrarily copied from KVO.
	leaderElectionLease = 90 * time.Second
	// ReconciliationPeriod.
	reconciliationPeriod = 30 * time.Second
)

// justRebootedSelector is a selector for combination of annotations
// expected to be on a node after it has completed a reboot.
//
// The update-operator sets constants.AnnotationOkToReboot to true to
// trigger a reboot, and the update-agent sets
// constants.AnnotationRebootNeeded and
// constants.AnnotationRebootInProgress to false when it has finished.
func justRebootedSelector() fields.Selector {
	return fields.Set(map[string]string{
		constants.AnnotationOkToReboot:       constants.True,
		constants.AnnotationRebootNeeded:     constants.False,
		constants.AnnotationRebootInProgress: constants.False,
	}).AsSelector()
}

// wantsRebootSelector is a selector for the annotation expected to be on a node when it wants to be rebooted.
//
// The update-agent sets constants.AnnotationRebootNeeded to true when
// it would like to reboot, and false when it starts up.
//
// If constants.AnnotationRebootPaused is set to "true", the update-agent will not consider it for rebooting.
func wantsRebootSelector() (fields.Selector, error) {
	return fields.ParseSelector(strings.Join([]string{
		constants.AnnotationRebootNeeded + "==" + constants.True,
		constants.AnnotationRebootPaused + "!=" + constants.True,
		constants.AnnotationOkToReboot + "!=" + constants.True,
		constants.AnnotationRebootInProgress + "!=" + constants.True,
	}, ","))
}

// stillRebootingSelector is a selector for the annotation set expected to be
// on a node when it's in the process of rebooting.
func stillRebootingSelector() fields.Selector {
	return fields.Set(map[string]string{
		constants.AnnotationOkToReboot:   constants.True,
		constants.AnnotationRebootNeeded: constants.True,
	}).AsSelector()
}

// beforeRebootReq requires a node to be waiting for before reboot checks to complete.
func beforeRebootReq() *labels.Requirement {
	req, _ := labels.NewRequirement(constants.LabelBeforeReboot, selection.In, []string{constants.True})

	return req
}

// afterRebootReq requires a node to be waiting for after reboot checks to complete.
func afterRebootReq() *labels.Requirement {
	req, _ := labels.NewRequirement(constants.LabelAfterReboot, selection.In, []string{constants.True})

	return req
}

// notBeforeRebootReq is the inverse of the beforeRebootReq.
func notBeforeRebootReq() *labels.Requirement {
	req, _ := labels.NewRequirement(constants.LabelBeforeReboot, selection.NotIn, []string{constants.True})

	return req
}

// notAfterRebootReq is the inverse of afterRebootReq.
func notAfterRebootReq() *labels.Requirement {
	req, _ := labels.NewRequirement(constants.LabelAfterReboot, selection.NotIn, []string{constants.True})

	return req
}

// Kontroller implement operator part of FLUO.
type Kontroller struct {
	kc kubernetes.Interface
	nc corev1client.NodeInterface
	er record.EventRecorder

	// Annotations to look for before and after reboots.
	beforeRebootAnnotations []string
	afterRebootAnnotations  []string

	leaderElectionClient        *kubernetes.Clientset
	leaderElectionEventRecorder record.EventRecorder
	// Namespace is the kubernetes namespace any resources (e.g. locks,
	// configmaps, agents) should be created and read under.
	// It will be set to the namespace the operator is running in automatically.
	namespace string

	// Auto-label Flatcar Container Linux nodes for migration compatibility.
	autoLabelContainerLinux bool

	// Reboot window.
	rebootWindow *timeutil.Periodic

	// Deprecated.
	manageAgent    bool
	agentImageRepo string
}

// Config configures a Kontroller.
type Config struct {
	// Kubernetes client.
	Client kubernetes.Interface
	// Migration compatibility.
	AutoLabelContainerLinux bool
	// Annotations to look for before and after reboots.
	BeforeRebootAnnotations []string
	AfterRebootAnnotations  []string
	// Reboot window.
	RebootWindowStart  string
	RebootWindowLength string
	// Deprecated.
	ManageAgent    bool
	AgentImageRepo string
}

// New initializes a new Kontroller.
func New(config Config) (*Kontroller, error) {
	// Kubernetes client.
	if config.Client == nil {
		return nil, fmt.Errorf("kubernetes client must not be nil")
	}

	kc := config.Client

	// Node interface.
	nc := kc.CoreV1().Nodes()

	// Create event emitter.
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&corev1client.EventSinkImpl{Interface: kc.CoreV1().Events("")})
	er := broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: eventSourceComponent})

	leaderElectionClientConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("error creating leader election client config: %w", err)
	}

	leaderElectionClient, err := kubernetes.NewForConfig(leaderElectionClientConfig)
	if err != nil {
		return nil, fmt.Errorf("error creating leader election client: %w", err)
	}

	leaderElectionBroadcaster := record.NewBroadcaster()
	leaderElectionBroadcaster.StartRecordingToSink(&corev1client.EventSinkImpl{
		Interface: corev1client.New(leaderElectionClient.CoreV1().RESTClient()).Events(""),
	})

	leaderElectionEventRecorder := leaderElectionBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{
		Component: leaderElectionEventSourceComponent,
	})

	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		return nil, fmt.Errorf("unable to determine operator namespace: please ensure POD_NAMESPACE " +
			"environment variable is set")
	}

	var rebootWindow *timeutil.Periodic

	if config.RebootWindowStart != "" && config.RebootWindowLength != "" {
		rw, err := timeutil.ParsePeriodic(config.RebootWindowStart, config.RebootWindowLength)
		if err != nil {
			return nil, fmt.Errorf("parsing reboot window: %w", err)
		}

		rebootWindow = rw
	}

	return &Kontroller{
		kc:                          kc,
		nc:                          nc,
		er:                          er,
		beforeRebootAnnotations:     config.BeforeRebootAnnotations,
		afterRebootAnnotations:      config.AfterRebootAnnotations,
		leaderElectionClient:        leaderElectionClient,
		leaderElectionEventRecorder: leaderElectionEventRecorder,
		namespace:                   namespace,
		autoLabelContainerLinux:     config.AutoLabelContainerLinux,
		manageAgent:                 config.ManageAgent,
		agentImageRepo:              config.AgentImageRepo,
		rebootWindow:                rebootWindow,
	}, nil
}

// Run starts the operator reconcilitation process and runs until the stop
// channel is closed.
func (k *Kontroller) Run(stop <-chan struct{}) error {
	err := k.withLeaderElection()
	if err != nil {
		return err
	}

	// Start Flatcar Container Linux node auto-labeler.
	if k.autoLabelContainerLinux {
		go wait.Until(k.legacyLabeler, reconciliationPeriod, stop)
	}

	// Before doing anything else, make sure the associated agent daemonset is
	// ready if it's our responsibility.
	if k.manageAgent && k.agentImageRepo != "" {
		// Create or update the update-agent daemonset.
		err := k.runDaemonsetUpdate(k.agentImageRepo)
		if err != nil {
			klog.Errorf("unable to ensure managed agents are ready: %v", err)

			return err
		}
	}

	klog.V(5).Info("starting controller")

	// Call the process loop each period, until stop is closed.
	wait.Until(k.process, reconciliationPeriod, stop)

	klog.V(5).Info("stopping controller")

	return nil
}

// withLeaderElection creates a new context which is cancelled when this
// operator does not hold a lock to operate on the cluster.
func (k *Kontroller) withLeaderElection() error {
	// TODO: a better id might be necessary.
	// Currently, KVO uses env.POD_NAME and the upstream controller-manager uses this.
	// Both end up having the same value in general, but Hostname is
	// more likely to have a value.
	id, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("getting hostname: %w", err)
	}

	resLock := &resourcelock.ConfigMapLock{
		ConfigMapMeta: metav1.ObjectMeta{
			Namespace: k.namespace,
			Name:      leaderElectionResourceName,
		},
		Client: k.leaderElectionClient.CoreV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity:      id,
			EventRecorder: k.leaderElectionEventRecorder,
		},
	}

	waitLeading := make(chan struct{})
	go func(waitLeading chan<- struct{}) {
		// Lease values inspired by a combination of
		// https://github.com/kubernetes/kubernetes/blob/f7c07a121d2afadde7aa15b12a9d02858b30a0a9/pkg/apis/componentconfig/v1alpha1/defaults.go#L163-L174
		// and the KVO values
		// See also
		// https://github.com/kubernetes/kubernetes/blob/fc31dae165f406026142f0dd9a98cada8474682a/pkg/client/leaderelection/leaderelection.go#L17
		leaderelection.RunOrDie(context.TODO(), leaderelection.LeaderElectionConfig{
			Lock:          resLock,
			LeaseDuration: leaderElectionLease,
			//nolint:gomnd // Set renew deadline to 2/3rd of the lease duration to give
			//             // controller enough time to renew the lease.
			RenewDeadline: leaderElectionLease * 2 / 3,
			//nolint:gomnd // Retry duration is usually around 1/10th of lease duration,
			//             // but given low dynamics of FLUO, 1/3rd should also be fine.
			RetryPeriod: leaderElectionLease / 3,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(ctx context.Context) { // was: func(stop <-chan struct{
					klog.V(5).Info("started leading")
					waitLeading <- struct{}{}
				},
				OnStoppedLeading: func() {
					klog.Fatalf("leaderelection lost")
				},
			},
		})
	}(waitLeading)

	<-waitLeading

	return nil
}

// process performs the reconcilitation to coordinate reboots.
func (k *Kontroller) process() {
	klog.V(4).Info("Going through a loop cycle")

	// First make sure that all of our nodes are in a well-defined state with
	// respect to our annotations and labels, and if they are not, then try to
	// fix them.
	klog.V(4).Info("Cleaning up node state")

	err := k.cleanupState()
	if err != nil {
		klog.Errorf("Failed to cleanup node state: %v", err)

		return
	}

	// Find nodes with the after-reboot=true label and check if all provided
	// annotations are set. if all annotations are set to true then remove the
	// after-reboot=true label and set reboot-ok=false, telling the agent that
	// the reboot has completed.
	klog.V(4).Info("Checking if configured after-reboot annotations are set to true")

	err = k.checkAfterReboot()
	if err != nil {
		klog.Errorf("Failed to check after reboot: %v", err)

		return
	}

	// Find nodes which just rebooted but haven't run after-reboot checks.
	// remove after-reboot annotations and add the after-reboot=true label.
	klog.V(4).Info("Labeling rebooted nodes with after-reboot label")

	err = k.markAfterReboot()
	if err != nil {
		klog.Errorf("Failed to update recently rebooted nodes: %v", err)

		return
	}

	// Find nodes with the before-reboot=true label and check if all provided
	// annotations are set. if all annotations are set to true then remove the
	// before-reboot=true label and set reboot=ok=true, telling the agent it's
	// time to reboot.
	klog.V(4).Info("Checking if configured before-reboot annotations are set to true")

	err = k.checkBeforeReboot()
	if err != nil {
		klog.Errorf("Failed to check before reboot: %v", err)

		return
	}

	// Take some number of the rebootable nodes. remove before-reboot
	// annotations and add the before-reboot=true label.
	klog.V(4).Info("Labeling rebootable nodes with before-reboot label")

	err = k.markBeforeReboot()
	if err != nil {
		klog.Errorf("Failed to update rebootable nodes: %v", err)

		return
	}
}

// cleanupState attempts to make sure nodes are in a well-defined state before
// performing state changes on them.
// If there is an error getting the list of nodes or updating any of them, an
// error is immediately returned.
func (k *Kontroller) cleanupState() error {
	nodelist, err := k.nc.List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}

	selector, _ := wantsRebootSelector()

	for _, n := range nodelist.Items {
		err = k8sutil.UpdateNodeRetry(k.nc, n.Name, func(node *corev1.Node) {
			// Make sure that nodes with the before-reboot label actually
			// still wants to reboot.
			if _, exists := node.Labels[constants.LabelBeforeReboot]; exists {
				if !selector.Matches(fields.Set(node.Annotations)) {
					klog.Warningf("Node %v no longer wanted to reboot while we were trying to label it so: %v",
						node.Name, node.Annotations)
					delete(node.Labels, constants.LabelBeforeReboot)
					for _, annotation := range k.beforeRebootAnnotations {
						delete(node.Annotations, annotation)
					}
				}
			}
		})
		if err != nil {
			return fmt.Errorf("cleaning up node %q: %w", n.Name, err)
		}
	}

	return nil
}

// checkBeforeReboot gets all nodes with the before-reboot=true label and checks
// if all of the configured before-reboot annotations are set to true. If they
// are, it deletes the before-reboot=true label and sets reboot-ok=true to tell
// the agent that it is ready to start the actual reboot process.
// If it goes to set reboot-ok=true and finds that the node no longer wants a
// reboot, then it just deletes the before-reboot=true label.
// If there is an error getting the list of nodes or updating any of them, an
// error is immediately returned.
func (k *Kontroller) checkBeforeReboot() error {
	nodelist, err := k.nc.List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}

	preRebootNodes := k8sutil.FilterNodesByRequirement(nodelist.Items, beforeRebootReq())

	for _, n := range preRebootNodes {
		if hasAllAnnotations(n, k.beforeRebootAnnotations) {
			klog.V(4).Infof("Deleting label %q for %q", constants.LabelBeforeReboot, n.Name)
			klog.V(4).Infof("Setting annotation %q to true for %q", constants.AnnotationOkToReboot, n.Name)

			err = k8sutil.UpdateNodeRetry(k.nc, n.Name, func(node *corev1.Node) {
				delete(node.Labels, constants.LabelBeforeReboot)
				// Cleanup the before-reboot annotations.
				for _, annotation := range k.beforeRebootAnnotations {
					klog.V(4).Infof("Deleting annotation %q from node %q", annotation, node.Name)
					delete(node.Annotations, annotation)
				}
				node.Annotations[constants.AnnotationOkToReboot] = constants.True
			})
			if err != nil {
				return fmt.Errorf("updating node %q: %w", n.Name, err)
			}
		}
	}

	return nil
}

// checkAfterReboot gets all nodes with the after-reboot=true label and checks
// if  all of the configured after-reboot annotations are set to true. If they
// are, it deletes the after-reboot=true label and sets reboot-ok=false to tell
// the agent that it has completed it's reboot successfully.
// If there is an error getting the list of nodes or updating any of them, an
// error is immediately returned.
func (k *Kontroller) checkAfterReboot() error {
	nodelist, err := k.nc.List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}

	postRebootNodes := k8sutil.FilterNodesByRequirement(nodelist.Items, afterRebootReq())

	for _, n := range postRebootNodes {
		if hasAllAnnotations(n, k.afterRebootAnnotations) {
			klog.V(4).Infof("Deleting label %q for %q", constants.LabelAfterReboot, n.Name)
			klog.V(4).Infof("Setting annotation %q to false for %q", constants.AnnotationOkToReboot, n.Name)

			err = k8sutil.UpdateNodeRetry(k.nc, n.Name, func(node *corev1.Node) {
				delete(node.Labels, constants.LabelAfterReboot)
				// Cleanup the after-reboot annotations.
				for _, annotation := range k.afterRebootAnnotations {
					klog.V(4).Infof("Deleting annotation %q from node %q", annotation, node.Name)
					delete(node.Annotations, annotation)
				}
				node.Annotations[constants.AnnotationOkToReboot] = constants.False
			})
			if err != nil {
				return fmt.Errorf("updating node %q: %w", n.Name, err)
			}
		}
	}

	return nil
}

// insideRebootWindow checks if process is inside reboot window at the time
// of calling this function.
//
// If reboot window is not configured, true is always returned.
func (k *Kontroller) insideRebootWindow() bool {
	if k.rebootWindow == nil {
		return true
	}

	// Get previous occurrence relative to now.
	period := k.rebootWindow.Previous(time.Now())

	return !(period.End.After(time.Now()))
}

// remainingRebootingCapacity calculates how many more nodes can be rebooted at a time based
// on a given list of nodes.
//
// If maximum capacity is reached, it is logged and list of rebooting nodes is logged as well.
func (k *Kontroller) remainingRebootingCapacity(nodelist *corev1.NodeList) int {
	rebootingNodes := k8sutil.FilterNodesByAnnotation(nodelist.Items, stillRebootingSelector())

	// Nodes running before and after reboot checks are still considered to be "rebooting" to us.
	beforeRebootNodes := k8sutil.FilterNodesByRequirement(nodelist.Items, beforeRebootReq())
	afterRebootNodes := k8sutil.FilterNodesByRequirement(nodelist.Items, afterRebootReq())

	rebootingNodes = append(append(rebootingNodes, beforeRebootNodes...), afterRebootNodes...)

	remainingCapacity := maxRebootingNodes - len(rebootingNodes)

	if remainingCapacity == 0 {
		for _, n := range rebootingNodes {
			klog.Infof("Found node %q still rebooting, waiting", n.Name)
		}

		klog.Infof("Found %d (of max %d) rebooting nodes; waiting for completion", len(rebootingNodes), maxRebootingNodes)
	}

	return remainingCapacity
}

// nodesRequiringReboot filters given list of nodes and returns ones which requires a reboot.
func (k *Kontroller) nodesRequiringReboot(nodelist *corev1.NodeList) []corev1.Node {
	selector, _ := wantsRebootSelector()

	rebootableNodes := k8sutil.FilterNodesByAnnotation(nodelist.Items, selector)

	return k8sutil.FilterNodesByRequirement(rebootableNodes, notBeforeRebootReq())
}

// rebootableNodes returns list of nodes which can be marked for rebooting based on remaining capacity.
func (k *Kontroller) rebootableNodes(nodelist *corev1.NodeList) []*corev1.Node {
	remainingCapacity := k.remainingRebootingCapacity(nodelist)

	nodesRequiringReboot := k.nodesRequiringReboot(nodelist)

	chosenNodes := make([]*corev1.Node, 0, remainingCapacity)
	for i := 0; i < remainingCapacity && i < len(nodesRequiringReboot); i++ {
		chosenNodes = append(chosenNodes, &nodesRequiringReboot[i])
	}

	klog.Infof("Found %d nodes that need a reboot", len(chosenNodes))

	return chosenNodes
}

// markBeforeReboot gets nodes which want to reboot and marks them with the
// before-reboot=true label. This is considered the beginning of the reboot
// process from the perspective of the update-operator. It will only mark
// nodes with this label up to the maximum number of concurrently rebootable
// nodes as configured with the maxRebootingNodes constant. It also checks if
// we are inside the reboot window.
// It cleans up the before-reboot annotations before it applies the label, in
// case there are any left over from the last reboot.
// If there is an error getting the list of nodes or updating any of them, an
// error is immediately returned.
func (k *Kontroller) markBeforeReboot() error {
	nodelist, err := k.nc.List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}

	if !k.insideRebootWindow() {
		klog.V(4).Info("We are outside the reboot window; not labeling rebootable nodes for now")

		return nil
	}

	// Set before-reboot=true for the chosen nodes.
	for _, n := range k.rebootableNodes(nodelist) {
		err = k.mark(n.Name, constants.LabelBeforeReboot, "before-reboot", k.beforeRebootAnnotations)
		if err != nil {
			return fmt.Errorf("labeling node for before reboot checks: %w", err)
		}
	}

	return nil
}

// markAfterReboot gets nodes which have completed rebooting and marks them with
// the after-reboot=true label. A node with the after-reboot=true label is still
// considered to be rebooting from the perspective of the update-operator, even
// though it has completed rebooting from the machines perspective.
// It cleans up the after-reboot annotations before it applies the label, in
// case there are any left over from the last reboot.
// If there is an error getting the list of nodes or updating any of them, an
// error is immediately returned.
func (k *Kontroller) markAfterReboot() error {
	nodelist, err := k.nc.List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}

	// Find nodes which just rebooted.
	justRebootedNodes := k8sutil.FilterNodesByAnnotation(nodelist.Items, justRebootedSelector())
	// Also filter out any nodes that are already labeled with after-reboot=true.
	justRebootedNodes = k8sutil.FilterNodesByRequirement(justRebootedNodes, notAfterRebootReq())

	klog.Infof("Found %d rebooted nodes", len(justRebootedNodes))

	// For all the nodes which just rebooted, remove any old annotations and add the after-reboot=true label.
	for _, n := range justRebootedNodes {
		err = k.mark(n.Name, constants.LabelAfterReboot, "after-reboot", k.afterRebootAnnotations)
		if err != nil {
			return fmt.Errorf("labeling node for after reboot checks: %w", err)
		}
	}

	return nil
}

func (k *Kontroller) mark(nodeName, label, annotationsType string, annotations []string) error {
	klog.V(4).Infof("Deleting annotations %v for %q", annotations, nodeName)
	klog.V(4).Infof("Setting label %q to %q for node %q", label, constants.True, nodeName)

	err := k8sutil.UpdateNodeRetry(k.nc, nodeName, func(node *corev1.Node) {
		for _, annotation := range annotations {
			delete(node.Annotations, annotation)
		}
		node.Labels[label] = constants.True
	})
	if err != nil {
		return fmt.Errorf("setting label %q to %q on node %q: %w", label, constants.True, nodeName, err)
	}

	if len(annotations) > 0 {
		klog.Infof("Waiting for %s annotations on node %q: %v", annotationsType, nodeName, annotations)
	}

	return nil
}

func hasAllAnnotations(node corev1.Node, annotations []string) bool {
	nodeAnnotations := node.GetAnnotations()

	for _, annotation := range annotations {
		value, ok := nodeAnnotations[annotation]
		if !ok || value != constants.True {
			return false
		}
	}

	return true
}
